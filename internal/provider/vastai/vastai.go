// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package vastai adapts the Vast.ai marketplace to LARRI's Provider interface.
//
// Everything Vast-specific dies here (P1): offers, asks, bid pricing, and
// contract IDs are normalised into core.Offer and core.Instance, and nothing
// above this package may branch on which provider it is talking to.
package vastai

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/secret"
)

// Provider is the Vast.ai adapter.
type Provider struct {
	c *Client

	// OnDrift is called when a payload decodes but fails validation.
	// Defaults to ignoring it; the daemon wires this to a WARN log.
	OnDrift func(error)

	// OnNotice reports a non-fatal condition the operator should see, such
	// as a truncated result set. Ranking that cannot see a candidate is
	// indistinguishable from ranking that rejected it, so the truncation has
	// to surface rather than be inferred from a suspiciously round count.
	OnNotice func(string)
}

var _ provider.Provider = (*Provider)(nil)

// New builds an adapter from an API key.
func New(key secret.Secret) *Provider { return &Provider{c: NewClient(key)} }

// NewWithClient builds an adapter around a configured client, for tests.
func NewWithClient(c *Client) *Provider { return &Provider{c: c} }

func (p *Provider) Name() string { return "vastai" }

func (p *Provider) drift(err error) {
	var d *ShapeDrift
	if err != nil && asDrift(err, &d) && p.OnDrift != nil {
		p.OnDrift(d)
	}
}

func asDrift(err error, out **ShapeDrift) bool {
	d, ok := err.(*ShapeDrift)
	if ok {
		*out = d
	}
	return ok
}

// Search returns offers satisfying the criteria.
func (p *Provider) Search(ctx context.Context, c core.Criteria) ([]core.Offer, error) {
	req := searchRequest{
		Limit: searchLimit,
		Type:  "on-demand",
		Order: [][]string{{"dph_total", "asc"}},
		// Only offers that can actually be rented right now. A ranked list
		// full of unrentable machines wastes the operator's attention and
		// invites a create that fails after selection.
		Rentable: &boolFilter{Eq: true},
	}
	// Q-04: interruptible is opt-in. Vast expresses it as a bid contract type.
	if c.Interruptible == core.Require {
		req.Type = "bid"
	}
	if len(c.GPUModel) > 0 {
		req.GPUName = &stringsFilter{In: c.GPUModel}
	}
	if c.GPUCount > 0 {
		req.NumGPUs = &intFilter{Gte: &c.GPUCount}
	}
	if c.VRAMPerGPUGB > 0 {
		mb := c.VRAMPerGPUGB * mbPerGB
		req.GPURAM = &intFilter{Gte: &mb}
	}
	if c.MaxPriceHr > 0 {
		req.DPHTotal = &floatFilter{Lte: &c.MaxPriceHr}
	}
	if c.MinReliability > 0 {
		req.Reliability = &floatFilter{Gte: &c.MinReliability}
	}
	if c.DiskGB > 0 {
		d := float64(c.DiskGB)
		req.DiskSpace = &floatFilter{Gte: &d}
	}
	if c.CPUCores > 0 {
		req.CPUCores = &intFilter{Gte: &c.CPUCores}
	}
	if c.RAMGB > 0 {
		r := float64(c.RAMGB * mbPerGB)
		req.CPURAM = &floatFilter{Gte: &r}
	}
	if len(c.Regions) > 0 {
		req.Geolocation = &stringsFilter{In: c.Regions}
	}

	var resp searchResponse
	err := p.c.do(ctx, "POST", pathSearch, req, &resp)
	p.drift(err)
	if err != nil && !isDrift(err) {
		return nil, err
	}

	// The API returns at most `limit` offers with no truncation flag, so a
	// full page is the only available signal that more existed. Since the
	// server sorts by price ascending, a truncated set is the *cheapest*
	// matches — which is exactly the set a fit-weighted ranking most needs to
	// see past (§8), and exactly where a host fishing for renters would sit
	// (FR-SRCH-08).
	if len(resp.Offers) >= searchLimit && p.OnNotice != nil {
		p.OnNotice(fmt.Sprintf(
			"search returned the maximum %d offers: the result set is truncated to the "+
				"cheapest matches and ranking cannot see beyond them; narrow the criteria",
			searchLimit))
	}

	out := make([]core.Offer, 0, len(resp.Offers))
	for _, o := range resp.Offers {
		n, nerr := o.normalise()
		if nerr != nil {
			// A single unparseable offer must not fail the search, but it
			// must not be silently ranked either.
			if p.OnDrift != nil {
				p.OnDrift(nerr)
			}
			continue
		}
		if !c.Interruptible.Permits(n.Interruptible) {
			continue
		}
		if len(c.BlockRegions) > 0 && matchesAny(n.Region, c.BlockRegions) {
			continue
		}
		if c.CertifiedOnly && !n.Certified {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// Create purchases an offer.
//
// The label is stamped here, from a rig ID minted before this call was made,
// which is what lets reconciliation attribute an instance whose create
// response never arrived (FR-STATE-04).
func (p *Provider) Create(ctx context.Context, o core.Offer, spec provider.CreateSpec) (*core.Instance, error) {
	body := createRequest{
		Image:       spec.Image,
		Disk:        float64(spec.DiskGB),
		Label:       spec.Label,
		RunType:     "ssh",
		TargetState: "running",
		OnStart:     spec.OnStart,
		Env:         renderEnv(spec),
	}
	if o.Interruptible {
		body.Price = o.PriceHr
	}

	var resp createResponse
	err := p.c.do(ctx, "PUT", fmt.Sprintf(pathCreate, o.OfferID), body, &resp)
	p.drift(err)
	if err != nil && !isDrift(err) {
		return nil, err
	}
	if !resp.Success || resp.NewContract == 0 {
		return nil, errs.Newf(errs.ClassProviderUnknownOutcome, "vastai.Create",
			"create reported success=%v contract=%d", resp.Success, resp.NewContract)
	}
	id := strconv.FormatInt(resp.NewContract, 10)

	// The create response carries only an ID. Read the instance back so the
	// caller gets normalised, real values rather than the ones we asked for.
	inst, gerr := p.Get(ctx, id)
	if gerr != nil || inst == nil {
		// The instance exists — the contract ID proves it. Return what is
		// known so the caller can persist it and reconcile the rest.
		return &core.Instance{
			Provider: p.Name(), InstanceID: id, OfferID: o.OfferID,
			PriceHr: o.PriceHr, CreatedAt: time.Now().UTC(),
			Labels: map[string]string{core.LabelKey: labelRigID(spec.Label)},
		}, nil
	}
	return inst, nil
}

// Get returns one instance, running or not.
//
// It reads the single-instance route rather than enumerating the account.
// Supervision polls this every few seconds during a boot, and routing each
// poll through a paginated List of every instance is both wasteful and how a
// live run earned an HTTP 429 while waiting for a host to come up — a rate
// limit provoked entirely by asking the wrong question.
func (p *Provider) Get(ctx context.Context, instanceID string) (*core.Instance, error) {
	var resp getResponse
	err := p.c.do(ctx, "GET", fmt.Sprintf(pathGet, instanceID), nil, &resp)
	if err != nil {
		if isNotFound(err) {
			return nil, nil // absent: the only proof of destruction
		}
		return nil, err
	}
	if resp.Instances == nil {
		return nil, nil
	}
	inst, nerr := resp.Instances.normalise()
	if nerr != nil {
		return nil, nerr
	}
	return &inst, nil
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http 404")
}

// List returns every instance on the account, running or not, across all pages.
//
// Two properties are load-bearing and both are easy to get wrong:
//
//   - **No status filter.** Vast supports select_filters on actual_status, and
//     filtering to running would be the natural thing to write. It would also
//     hide stopped containers, which still bill for storage — so teardown
//     would see absence and journal DESTROYED while the bill continued (R-13).
//   - **Pagination to exhaustion.** The endpoint caps at 25 per page. An
//     adapter that read one page would silently miss every orphan past the
//     twenty-fifth, which is R-01 arriving through a default.
func (p *Provider) List(ctx context.Context) ([]core.Instance, error) {
	var (
		out   []core.Instance
		token string
	)
	for page := 0; ; page++ {
		if page >= listPageBudget {
			return nil, errs.Newf(errs.ClassProviderTransient, "vastai.List",
				"pagination exceeded %d pages", listPageBudget)
		}
		path := fmt.Sprintf("%s?limit=%d", pathList, listPageMax)
		if token != "" {
			path += "&after_token=" + token
		}
		var resp listResponse
		err := p.c.do(ctx, "GET", path, nil, &resp)
		p.drift(err)
		if err != nil && !isDrift(err) {
			return nil, err
		}
		for _, in := range resp.Instances {
			n, nerr := in.normalise()
			if nerr != nil {
				if p.OnDrift != nil {
					p.OnDrift(nerr)
				}
				continue
			}
			out = append(out, n)
		}
		if resp.NextToken == nil || *resp.NextToken == "" || len(resp.Instances) == 0 {
			return out, nil
		}
		token = *resp.NextToken
	}
}

// Destroy removes an instance. A nil error is a claim, not proof: absence from
// List is the evidence teardown checks (FR-DEL-03).
func (p *Provider) Destroy(ctx context.Context, instanceID string) error {
	err := p.c.do(ctx, "DELETE", fmt.Sprintf(pathDestroy, instanceID), nil, nil)
	if err != nil && !isDrift(err) {
		return err
	}
	return nil
}

func isDrift(err error) bool {
	_, ok := err.(*ShapeDrift)
	return ok
}

func matchesAny(s string, pats []string) bool {
	for _, p := range pats {
		if strings.Contains(strings.ToLower(s), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// labelRigID extracts the rig ID from a "larri:<id>" label.
func labelRigID(label string) string {
	if id, ok := strings.CutPrefix(label, core.LabelKey+":"); ok {
		return id
	}
	return label
}

// renderEnv builds Vast's env string.
//
// FR-SEC-15: no -p mapping is emitted for the runtime port. Vast reaches SSH
// through runtype=ssh without a mapping, and a container port that was never
// mapped is unreachable regardless of what listens on it — which is the
// primary network control, enforced by the provider rather than by the host.
func renderEnv(spec provider.CreateSpec) string {
	var b strings.Builder
	for _, port := range spec.Ports {
		fmt.Fprintf(&b, "-p %d:%d ", port, port)
	}
	for k, v := range spec.Env {
		fmt.Fprintf(&b, "-e %s=%s ", k, v.Reveal())
	}
	return strings.TrimSpace(b.String())
}

// AttachSSHKey installs a public key on a running instance.
//
// This is what lets a restarted LARRI recover a rig it can no longer
// authenticate to. It does not retrieve the old credential — that is gone by
// design — it adds a new one, so the identity a recovered rig accepts is
// freshly minted rather than dug out of storage.
func (p *Provider) AttachSSHKey(ctx context.Context, instanceID, publicKey string) error {
	body := struct {
		SSHKey string `json:"ssh_key"`
	}{SSHKey: publicKey}

	// Vast answers a *failed* attach with HTTP 200 and success:false — a
	// probe against a non-existent instance returns 200 carrying an internal
	// Python error, where a genuinely absent route returns 404. So the status
	// code cannot be the check. Trusting it would report a key installed that
	// is not, and the failure would surface later as an unexplained
	// authentication error against a host that never received it.
	var resp struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Msg     string `json:"msg"`
	}
	if err := p.c.do(ctx, "POST", fmt.Sprintf(pathAttach, instanceID), body, &resp); err != nil {
		return err
	}
	if !resp.Success {
		detail := resp.Msg
		if detail == "" {
			detail = resp.Error
		}
		return errs.Newf(errs.ClassProviderTransient, "vastai.AttachSSHKey",
			"attach key to %s: %s", instanceID, detail)
	}
	return nil
}

var _ provider.KeyAttacher = (*Provider)(nil)
