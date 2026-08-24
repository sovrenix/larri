// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/secret"
)

// Provider implements provider.Provider for RunPod.
type Provider struct {
	c *Client

	OnDrift  func(error)
	OnNotice func(string)
}

// New builds a provider.
func New(key secret.Secret) *Provider { return &Provider{c: NewClient(key)} }

// NewWithClient builds a provider over a prepared client, for tests.
func NewWithClient(c *Client) *Provider { return &Provider{c: c} }

func (p *Provider) Name() string { return "runpod" }

func (p *Provider) SetOnDrift(f func(error))   { p.OnDrift = f }
func (p *Provider) SetOnNotice(f func(string)) { p.OnNotice = f }

func (p *Provider) notice(format string, a ...any) {
	if p.OnNotice != nil {
		p.OnNotice(fmt.Sprintf(format, a...))
	}
}

// Search returns the catalogue as offers, filtered by the criteria.
//
// Filtering happens here rather than at the provider because the catalogue
// query takes none — it returns every GPU type RunPod sells. The ranker
// applies criteria again and is authoritative; this pass exists to keep the
// obviously-unusable out of the ranked list an operator reads.
func (p *Provider) Search(ctx context.Context, c core.Criteria) ([]core.Offer, error) {
	var data struct {
		GPUTypes []gpuType `json:"gpuTypes"`
	}
	if err := p.c.graphql(ctx, catalogueQuery, &data); err != nil {
		return nil, err
	}
	if len(data.GPUTypes) == 0 {
		return nil, errs.Newf(errs.ClassProviderTransient, "runpod.Search",
			"catalogue returned nothing")
	}

	wantSpot := c.Interruptible == core.Allow || c.Interruptible == core.Require
	out := make([]core.Offer, 0, len(data.GPUTypes))
	dropped := map[dropReason]int{}
	for _, g := range data.GPUTypes {
		o, why, ok := g.normalise(wantSpot)
		if !ok {
			dropped[why]++
			continue
		}
		if !matches(o, c) {
			continue
		}
		out = append(out, o)
	}
	// Said out loud, and by reason. An operator looking for an RTX 3070 that
	// RunPod lists at $0.13 should hear that it is out of stock, rather than
	// conclude LARRI does not know about it — and "out of stock" is a
	// different fact from "too expensive" or "not real hardware".
	for _, why := range []dropReason{dropOutOfStock, dropUnpriced, dropUnpurchasable} {
		if n := dropped[why]; n > 0 {
			p.notice("runpod: %d gpu types skipped (%s)", n, why)
		}
	}
	if len(out) == 0 {
		return nil, errs.Newf(errs.ClassCriteriaUnsatisfiable, "runpod.Search",
			"nothing rentable: %d types listed, all skipped or filtered", len(data.GPUTypes))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PriceHr < out[j].PriceHr })
	return out, nil
}

// matches applies the criteria the catalogue cannot.
func matches(o core.Offer, c core.Criteria) bool {
	if c.MaxPriceHr > 0 && o.PriceHr > c.MaxPriceHr {
		return false
	}
	if c.VRAMPerGPUGB > 0 && o.VRAMPerGPUGB < c.VRAMPerGPUGB {
		return false
	}
	if len(c.GPUModel) > 0 {
		var hit bool
		for _, want := range c.GPUModel {
			if strings.Contains(strings.ToLower(o.GPUModel), strings.ToLower(want)) ||
				strings.Contains(strings.ToLower(o.OfferID), strings.ToLower(want)) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	// Interruptible: the zero value forbids (Q-04), and a spot-priced offer
	// must not be returned to an operator who did not ask for one.
	if o.Interruptible && c.Interruptible != core.Allow && c.Interruptible != core.Require {
		return false
	}
	return true
}

// Create rents a pod.
//
// The label rides in `name`, because RunPod has no tags. That is the only
// place a marker can go, and it is what makes an orphan attributable to the
// rig that created it (FR-STATE-04) — so the name is the label and nothing
// else, rather than a friendly name with the label appended.
func (p *Provider) Create(ctx context.Context, o core.Offer, spec provider.CreateSpec) (*core.Instance, error) {
	if spec.Label == "" {
		return nil, errs.Newf(errs.ClassModelFailure, "runpod.Create",
			"pod has no label: an orphan must be attributable")
	}
	req := createRequest{
		Name:              spec.Label,
		ImageName:         spec.Image,
		GPUTypeIDs:        []string{o.OfferID},
		GPUCount:          maxInt(o.GPUCount, 1),
		GPUTypePriority:   "availability",
		CloudType:         "SECURE",
		ComputeType:       "GPU",
		Interruptible:     o.Interruptible,
		SupportPublicIP:   true,
		Ports:             ports(spec.Ports),
		ContainerDiskInGb: containerDisk(spec.DiskGB),
		VolumeInGb:        volumeDisk(spec.DiskGB),
		VolumeMountPath:   "/workspace",
		Env:               map[string]string{},
	}
	for k, v := range spec.Env {
		req.Env[k] = v.Reveal()
	}
	// RunPod has no ssh key field. Its images read PUBLIC_KEY at start-up and
	// write it into authorized_keys, which is the documented mechanism and the
	// only one that does not require baking a key into an image.
	if spec.SSHPublicKey != "" {
		req.Env["PUBLIC_KEY"] = spec.SSHPublicKey
	}
	req.DockerStartCmd = startCommand(spec.OnStart)

	var created pod
	if err := p.c.rest(ctx, "POST", "/pods", req, &created); err != nil {
		return nil, createClass(err)
	}
	inst, err := created.normalise()
	if err != nil {
		return nil, errs.Newf(errs.ClassProviderUnknownOutcome, "runpod.Create", "%v", err)
	}
	inst.OfferID = o.OfferID
	if inst.PriceHr == 0 {
		inst.PriceHr = o.PriceHr
	}
	return &inst, nil
}

// Get returns one pod, or nil when it is gone.
//
// The includeMachine flag is not optional detail — without it this endpoint
// omits publicIp and portMappings entirely, while the list endpoint returns
// them for the same pod. Measured live: five consecutive single-pod reads
// showed no address at all, and LARRI sat waiting for an ssh endpoint that
// the provider was already publishing elsewhere. The flag is what makes the
// two views agree.
func (p *Provider) Get(ctx context.Context, instanceID string) (*core.Instance, error) {
	var got pod
	err := p.c.rest(ctx, "GET", "/pods/"+instanceID+"?includeMachine=true", nil, &got)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if got.ID == "" {
		return nil, nil
	}
	inst, nerr := got.normalise()
	if nerr != nil {
		return nil, nerr
	}
	return &inst, nil
}

// List returns every pod on the account, running or not.
//
// No desiredStatus filter, deliberately. RunPod supports one, and using it
// would hide EXITED pods — which still exist and still bill for their volume.
// A List that omitted them would report a billing resource as absent and
// teardown would journal it destroyed (R-13).
func (p *Provider) List(ctx context.Context) ([]core.Instance, error) {
	var raw []pod
	if err := p.c.rest(ctx, "GET", "/pods", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]core.Instance, 0, len(raw))
	for _, r := range raw {
		inst, err := r.normalise()
		if err != nil {
			if p.OnDrift != nil {
				p.OnDrift(err)
			}
			continue
		}
		out = append(out, inst)
	}
	return out, nil
}

// Destroy terminates a pod, treating one already gone as done.
//
// Idempotent because teardown retries when it cannot confirm absence
// (FR-DEL-04): the second attempt after a successful first meets a 404, and
// reporting that as failure would call a correctly destroyed rig a failed
// teardown.
func (p *Provider) Destroy(ctx context.Context, instanceID string) error {
	err := p.c.rest(ctx, "DELETE", "/pods/"+instanceID, nil, nil)
	if err == nil || isNotFound(err) {
		return nil
	}
	return err
}

// createRequest is the POST /pods body, in the fields LARRI sets.
type createRequest struct {
	Name              string            `json:"name"`
	ImageName         string            `json:"imageName"`
	GPUTypeIDs        []string          `json:"gpuTypeIds"`
	GPUCount          int               `json:"gpuCount"`
	GPUTypePriority   string            `json:"gpuTypePriority,omitempty"`
	CloudType         string            `json:"cloudType,omitempty"`
	ComputeType       string            `json:"computeType,omitempty"`
	Interruptible     bool              `json:"interruptible"`
	SupportPublicIP   bool              `json:"supportPublicIp"`
	Ports             []string          `json:"ports,omitempty"`
	ContainerDiskInGb int               `json:"containerDiskInGb,omitempty"`
	VolumeInGb        int               `json:"volumeInGb,omitempty"`
	VolumeMountPath   string            `json:"volumeMountPath,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	DockerStartCmd    []string          `json:"dockerStartCmd,omitempty"`
}

// ports renders LARRI's port numbers in RunPod's "port/protocol" form.
//
// SSH always, because the tunnel is the only path in and a pod with no mapped
// 22 is a pod LARRI cannot reach (FR-SEC-15).
func ports(want []int) []string {
	out := []string{"22/tcp"}
	for _, p := range want {
		if p == 22 {
			continue
		}
		out = append(out, fmt.Sprintf("%d/tcp", p))
	}
	return out
}

// containerDisk and volumeDisk split one number across RunPod's two.
//
// The weights go on the **volume**: it survives a stop, where the container
// disk is wiped. That matters because a stopped pod is exactly the state
// §12.4 says to expect and recover from — re-downloading tens of gigabytes
// after an interruption costs more than the storage does. The container disk
// gets a fixed working allowance for the image and its scratch.
const containerDiskGB = 20

func containerDisk(total int) int { return containerDiskGB }

func volumeDisk(total int) int {
	if total <= containerDiskGB {
		return containerDiskGB
	}
	return total
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Reporter = (*Provider)(nil)
)

func init() {
	provider.Register("runpod", func() (provider.Provider, error) {
		// An absent key is allowed. RunPod's catalogue is public, so `larri
		// offers --provider runpod` works without an account — and every
		// call that needs a key refuses with one before reaching the network.
		return New(secret.New(os.Getenv("RUNPOD_API_KEY"))), nil
	})
}

// createClass re-classifies a rejected create so the fallback can engage.
//
// A 400 is model-attributable by default — "the next host fails identically",
// so do not retry (FR-PROV-05). That is right for a bad image and wrong for a
// GPU type RunPod cannot place: the next *type* may be perfectly available,
// and aborting the run over it strands an operator whose criteria were fine.
//
// It matters because the catalogue advertises types the create enum does not
// accept, three of them priced well enough to be chosen. Without this, the
// first such selection ends the run instead of moving on.
func createClass(err error) error {
	if err == nil {
		return nil
	}
	e := strings.ToLower(err.Error())
	switch {
	// "There are no instances currently available" is the message RunPod
	// actually returns, observed live. An earlier guess at the wording missed
	// it, which would have left the fallback dormant exactly when it was
	// needed.
	case strings.Contains(e, "gputype"), strings.Contains(e, "gpu type"),
		strings.Contains(e, "no instances currently available"),
		strings.Contains(e, "no longer any instances available"),
		strings.Contains(e, "instances available"),
		strings.Contains(e, "unavailable"), strings.Contains(e, "capacity"),
		strings.Contains(e, "out of stock"):
		return errs.Newf(errs.ClassHostFailure, "runpod.Create",
			"this gpu type could not be placed: %v", shortest(err))
	}
	return err
}

// shortest trims a wrapped error to its final cause, so a fallback message
// reads as one line rather than as three layers of prefix.
func shortest(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}

// startCommand builds the container start command.
//
// This is where the adapter pays for something its provider does not supply.
//
// Vast fronts every instance with its own SSH machinery, so any image is
// reachable. RunPod does not: its convenient terminal access is a proxy with
// **no port forwarding**, which is precisely the one thing LARRI's tunnel is —
// so the pod needs a real sshd, and upstream engine images do not carry one
// (`vllm/vllm-openai:latest` has no sshd binary at all).
//
// RunPod documents the remedy as a container start command, and this is that
// command with LARRI's own key installation chained after it. Three details
// are load-bearing:
//
//   - It replaces the image's entrypoint rather than running beside it, which
//     is fine and in fact wanted: LARRI launches the engine itself over SSH,
//     so the image's own start-up would only race it.
//   - $PUBLIC_KEY is expanded on the host, from the env this adapter sets.
//   - `sleep infinity` at the end, because when the start command exits the
//     pod does — and a pod that dies the moment sshd is ready is worse than
//     one that never started.
func startCommand(onStart string) []string {
	script := `set -e
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server
mkdir -p /root/.ssh && chmod 700 /root/.ssh
if [ -n "$PUBLIC_KEY" ]; then
  echo "$PUBLIC_KEY" >> /root/.ssh/authorized_keys
fi
chmod 600 /root/.ssh/authorized_keys
mkdir -p /run/sshd
service ssh start || /usr/sbin/sshd
`
	if onStart != "" {
		// LARRI's own key installation, after sshd exists so the directory it
		// writes into is already there. Failures here must not stop the pod:
		// the key is already installed above, and a stranded pod is worse
		// than a redundant step that did not run.
		script += "{ " + onStart + " ; } || true\n"
	}
	script += "sleep infinity\n"
	return []string{"bash", "-c", script}
}
