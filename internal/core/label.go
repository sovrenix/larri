// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LabelVersion is the schema of the marker LARRI stamps on provider
// resources. Encoded so a future reader knows what it is looking at.
const LabelVersion = 1

// DefaultLabelLimit is a conservative ceiling for providers that do not
// document one. Adapters pass their platform's real limit.
const DefaultLabelLimit = 255

// Label is what LARRI knows about a rig from the provider side alone.
//
// It exists for the case where local state is gone — a lost disk, a wiped
// machine, a different laptop — and the only surviving record of a billing
// resource is what was stamped on it. Reconciliation can then explain an
// orphan rather than merely detect one: "rig 01J9Z, serving qwen3-coder on
// vllm since Tuesday, port 8000" is actionable where a bare ID is a puzzle.
//
// Three constraints shape the encoding, and they pull against each other:
//
//   - **The host reads this.** It is stamped on a resource owned by a
//     stranger, so the detail fields are **encrypted** (§15.5.4) and the rig
//     ID is not. That split is deliberate and explained there: attribution has
//     to survive losing the key, or an orphan becomes unattributable at
//     exactly the moment it matters most.
//   - **Truncation must degrade, not destroy.** Providers cap label length and
//     do not always say where. The rig ID comes first so that a truncated
//     label still attributes the resource, which is the one thing that must
//     never be lost (FR-STATE-04).
//   - **Future versions must stay readable.** Unknown keys are ignored rather
//     than rejected, so a label written by a later LARRI is still attributable
//     by an earlier one.
type Label struct {
	RigID     string
	Version   int
	Model     string
	Served    string
	Runtime   RuntimeKind
	LocalPort int
	CreatedAt time.Time
	PriceHr   float64
	Provider  string

	// Sealed reports that the detail fields are encrypted and could not be
	// opened with the sealer given. The rig ID is still valid: attribution
	// deliberately does not depend on holding the key.
	Sealed bool
}

// EncodeLabel renders a label, trimmed to fit limit.
//
// With a sealer, everything except the rig ID is encrypted into a single
// opaque field. Without one, the fields are written in the clear, in
// decreasing order of what a recovering LARRI needs, so a provider that
// silently truncates removes the least useful part first.
func EncodeLabel(l Label, limit int, sealer Sealer) string {
	if limit <= 0 {
		limit = DefaultLabelLimit
	}
	head := LabelKey + ":" + l.RigID
	if sealer == nil {
		return encodePlain(l, limit)
	}
	sealed, err := sealer.Seal([]byte(encodeFields(l)))
	if err != nil {
		// A sealer that cannot seal must not silently publish the plaintext
		// it was asked to hide. Attribution survives; the detail does not.
		return head
	}
	full := head + "|e=" + sealed
	if len(full) > limit {
		return head
	}
	return full
}

// encodePlain is the unencrypted form, used when no sealer is configured.
func encodePlain(l Label, limit int) string {
	// The prefix and rig ID are not optional and are never trimmed. A label
	// that cannot attribute its resource is worse than no label, because it
	// looks like someone else's instance.
	head := LabelKey + ":" + l.RigID
	if len(head) > limit {
		return head // over budget already; attribution still wins
	}
	type kv struct{ k, v string }
	var fields []kv
	add := func(k, v string) {
		if v != "" {
			fields = append(fields, kv{k, v})
		}
	}
	add("v", strconv.Itoa(orDefault(l.Version, LabelVersion)))
	if !l.CreatedAt.IsZero() {
		add("t", strconv.FormatInt(l.CreatedAt.UTC().Unix(), 10))
	}
	add("r", string(l.Runtime))
	if l.LocalPort > 0 {
		add("p", strconv.Itoa(l.LocalPort))
	}
	if l.PriceHr > 0 {
		add("c", strconv.FormatFloat(l.PriceHr, 'f', 4, 64))
	}
	add("n", sanitise(l.Served))
	add("m", sanitise(l.Model)) // longest, so last to be added and first to be dropped

	out := head
	for _, f := range fields {
		seg := "|" + f.k + "=" + f.v
		if len(out)+len(seg) > limit {
			break
		}
		out += seg
	}
	return out
}

// encodeFields renders the detail fields as the sealed payload.
func encodeFields(l Label) string {
	var b strings.Builder
	fmt.Fprintf(&b, "v=%d", orDefault(l.Version, LabelVersion))
	if !l.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "|t=%d", l.CreatedAt.UTC().Unix())
	}
	if l.Runtime != "" {
		fmt.Fprintf(&b, "|r=%s", l.Runtime)
	}
	if l.LocalPort > 0 {
		fmt.Fprintf(&b, "|p=%d", l.LocalPort)
	}
	if l.PriceHr > 0 {
		fmt.Fprintf(&b, "|c=%.4f", l.PriceHr)
	}
	if l.Served != "" {
		fmt.Fprintf(&b, "|n=%s", sanitise(l.Served))
	}
	if l.Model != "" {
		fmt.Fprintf(&b, "|m=%s", sanitise(l.Model))
	}
	return b.String()
}

// DecodeLabel parses a label, reporting whether it is LARRI's at all.
//
// Anything after the rig ID is best effort: a truncated tail, an unknown key,
// or a malformed number leaves the rest of the label usable. Attribution is
// the guarantee; the extras are a convenience.
func DecodeLabel(s string) (Label, bool) { return DecodeLabelWith(s, nil) }

// DecodeLabelWith parses a label, decrypting the detail fields when a sealer
// is supplied.
//
// Attribution never depends on the sealer: a label whose payload cannot be
// opened — wrong key, no key, tampered — still yields the rig ID, which is the
// datum that stops a billing resource from looking like a stranger's.
func DecodeLabelWith(s string, sealer Sealer) (Label, bool) {
	rest, ok := strings.CutPrefix(s, LabelKey+":")
	if !ok || rest == "" {
		return Label{}, false
	}
	parts := strings.Split(rest, "|")
	l := Label{RigID: parts[0], Version: LabelVersion}
	if l.RigID == "" {
		return Label{}, false
	}
	for _, p := range parts[1:] {
		if token, ok := strings.CutPrefix(p, "e="); ok {
			if sealer == nil {
				l.Sealed = true
				continue
			}
			pt, err := sealer.Open(token)
			if err != nil {
				l.Sealed = true
				continue
			}
			inner, _ := DecodeLabelWith(LabelKey+":"+l.RigID+"|"+string(pt), nil)
			inner.RigID = l.RigID
			return inner, true
		}
	}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue // truncated tail
		}
		switch k {
		case "v":
			if n, err := strconv.Atoi(v); err == nil {
				l.Version = n
			}
		case "t":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				l.CreatedAt = time.Unix(n, 0).UTC()
			}
		case "r":
			l.Runtime = RuntimeKind(v)
		case "p":
			if n, err := strconv.Atoi(v); err == nil {
				l.LocalPort = n
			}
		case "c":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				l.PriceHr = f
			}
		case "n":
			l.Served = v
		case "m":
			l.Model = v
			// Unknown keys are deliberately ignored, so a label written by a
			// later version stays attributable to an earlier one.
		}
	}
	return l, true
}

// Describe renders a label for an operator staring at an orphan.
func (l Label) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rig %s", l.RigID)
	if l.Model != "" {
		fmt.Fprintf(&b, " serving %s", l.Model)
	}
	if l.Runtime != "" {
		fmt.Fprintf(&b, " on %s", l.Runtime)
	}
	if !l.CreatedAt.IsZero() {
		fmt.Fprintf(&b, ", created %s", l.CreatedAt.Format(time.RFC3339))
	}
	if l.PriceHr > 0 {
		fmt.Fprintf(&b, ", $%.4f/hr", l.PriceHr)
	}
	if l.LocalPort > 0 {
		fmt.Fprintf(&b, ", local port %d", l.LocalPort)
	}
	if l.Sealed {
		b.WriteString(" (details sealed; no key to open them)")
	}
	return b.String()
}

// sanitise removes the separators the encoding relies on, so a model name
// containing one cannot forge a field.
func sanitise(s string) string {
	s = strings.NewReplacer("|", "_", "=", "_", "\n", "_", " ", "_").Replace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// LabelFor builds a Label from a rig.
func LabelFor(r *Rig) Label {
	l := Label{
		RigID: r.ID, Version: LabelVersion,
		Model: r.Model.Ref, Served: r.Model.ServedName,
		Runtime: r.Runtime, LocalPort: r.LocalPort,
		CreatedAt: r.CreatedAt, PriceHr: r.Offer.PriceHr,
		Provider: r.Offer.Provider,
	}
	return l
}
