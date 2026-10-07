// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"fmt"
	"sort"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/secret"
)

// Tier is how writable a client's configuration is (§10.2.1).
//
// R-05 is config corruption from automated edits, and the mitigation that
// matters is not writing carefully — it is knowing which clients may be written
// to at all. The tier is a property of the client, declared by its writer, and
// decided by *observed reload behaviour* rather than by the presence of a
// config mechanism.
type Tier string

const (
	// TierFile: plain-text config the application re-reads. Full automation —
	// back up, write idempotently, revert byte-exact on down.
	TierFile Tier = "A"

	// TierAppStore: SQLite, a keychain, or live process state. Never written
	// directly. Editing a database belonging to a running Electron app is how
	// you corrupt somebody's chat history to save them four clicks. Use the
	// application's own API if it has one; otherwise demote to TierGuided.
	TierAppStore Tier = "B"

	// TierGuided: anything else. Print the exact values, then verify later by
	// probing that the client actually reached the endpoint.
	//
	// A real outcome rather than a failure: two values to paste plus a probe
	// that confirms it worked is a legitimate integration, and it is
	// infinitely better than a corrupted config file.
	TierGuided Tier = "C"
)

// Writable reports whether LARRI may edit this client's configuration itself.
func (t Tier) Writable() bool { return t == TierFile }

// Endpoint is what a client is pointed at.
//
// Only the stable loopback URL and the stable served-model name are ever
// written (P3, FR-WIRE-06). The ephemeral provider host never appears in a
// client's configuration, or every teardown would force a rewrite of every
// client — which is the churn the fixed local port exists to prevent.
type Endpoint struct {
	URL   string
	Model string
	Key   secret.Secret
}

// ClientWriter configures one local application to use the endpoint (§10.2).
//
// The protocol is not negotiable, and Apply implementations are expected to
// honour all of it: detect before writing, back up, write idempotently, and be
// able to revert exactly. A writer that cannot honestly promise byte-exact
// revert must not claim TierFile.
type ClientWriter interface {
	// Name identifies the client, and keys the WiringRecord that reverts it.
	Name() string

	// Tier is how this client's configuration may be touched.
	Tier() Tier

	// Detect reports whether the client is installed and configured here.
	// Never write config for a client that is not present.
	Detect() (bool, error)

	// Apply points the client at the endpoint and returns what changed, in
	// enough detail to undo it exactly.
	Apply(ep Endpoint) (core.WiringRecord, error)

	// Revert restores the pre-up state.
	Revert(rec core.WiringRecord) error
}

// Prober verifies that a client actually reached the endpoint.
//
// Verification applies to every tier, not only the guided one: a file written
// correctly to an application that never re-read it is indistinguishable from
// one that was never written, and the operator finds out when their editor
// fails rather than when LARRI does.
type Prober func(client string) (bool, error)

// Guided is implemented by writers LARRI does not write configuration for.
//
// Tier B and C are the cases where editing somebody's application store or
// live process state would be worse than not editing it, and the tier system
// is only honest if the alternative is a real integration rather than a
// shrug. That alternative is two values and a probe — so a guided writer has
// to be able to say exactly what to paste, and Apply deliberately cannot say
// it, because Apply never runs for these tiers.
//
// The values are the endpoint's, never the provider's (FR-WIRE-06): a guided
// client is configured by hand, once, and an operator who pasted an ephemeral
// host would have to redo it after every teardown.
type Guided interface {
	ClientWriter

	// Instructions are the lines to show the operator, in order.
	Instructions(ep Endpoint) []string
}

// InstructionsFor returns what a writer wants shown, or nothing.
//
// Asked of the writer rather than branched on the tier, so a tier B client
// that drives its own API and still needs a manual step can say so.
func InstructionsFor(w ClientWriter, ep Endpoint) []string {
	g, ok := w.(Guided)
	if !ok {
		return nil
	}
	return g.Instructions(ep)
}

// Apply wires every detected client and returns what was changed.
//
// It never fails the rig. FR-PROV/§16 puts client configuration in its own
// error class for exactly this reason: a rig that serves but could not edit an
// IDE config is still a rig, and refusing to bring one up over a failed write
// would be a worse outcome than the failed write. Errors are returned
// alongside the records so the caller can report them and carry on.
//
// Records are returned for every client that was written, including ones whose
// probe then failed — because something was changed and it still has to be
// reverted on teardown.
func Apply(ws []ClientWriter, ep Endpoint, probe Prober) ([]core.WiringRecord, []error) {
	var (
		recs []core.WiringRecord
		errs []error
	)
	for _, w := range ws {
		ok, err := w.Detect()
		if err != nil {
			errs = append(errs, fmt.Errorf("wire: %s: detect: %w", w.Name(), err))
			continue
		}
		if !ok {
			continue // not installed here; not an error
		}
		if !w.Tier().Writable() {
			// Tier B and C are not written by LARRI. The caller prints the
			// values instead; tier B may already be verifiable, while tier C is
			// verified only after the operator has had a chance to act on those
			// instructions.
			rec := core.WiringRecord{
				Client: w.Name(), Tier: string(w.Tier()), AppliedAt: time.Now().UTC(),
			}
			if w.Tier() != TierGuided {
				rec.Verified = verify(probe, w.Name(), &errs)
			}
			recs = append(recs, rec)
			continue
		}
		rec, err := w.Apply(ep)
		if err != nil {
			errs = append(errs, fmt.Errorf("wire: %s: apply: %w", w.Name(), err))
			continue
		}
		rec.Client = w.Name()
		rec.Tier = string(w.Tier())
		if rec.AppliedAt.IsZero() {
			rec.AppliedAt = time.Now().UTC()
		}
		rec.Verified = verify(probe, w.Name(), &errs)
		recs = append(recs, rec)
	}
	return recs, errs
}

func verify(probe Prober, name string, errs *[]error) bool {
	if probe == nil {
		return false
	}
	ok, err := probe(name)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("wire: %s: probe: %w", name, err))
		return false
	}
	return ok
}

// Revert undoes everything Apply changed, in reverse order.
//
// Reverse, because wiring is applied in a deliberate order and undoing it
// forwards can leave an intermediate state that neither half expected. Every
// record is attempted even when an earlier one fails: a client left pointing at
// a destroyed endpoint is the failure this exists to prevent, and stopping at
// the first error would leave more of them than finishing does.
//
// Like Apply, this never fails a teardown. A rig must still be destroyed even
// if an editor's config could not be put back — the config is recoverable from
// the backup, and a rig left alive is not recoverable at all.
func Revert(recs []core.WiringRecord, byName map[string]ClientWriter) []error {
	var errs []error
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if !Tier(rec.Tier).Writable() {
			continue // nothing was written, so there is nothing to put back
		}
		w, ok := byName[rec.Client]
		if !ok {
			errs = append(errs, fmt.Errorf(
				"wire: %s: no writer to revert with; restore %s by hand",
				rec.Client, orNone(rec.BackupPath)))
			continue
		}
		if err := w.Revert(rec); err != nil {
			errs = append(errs, fmt.Errorf("wire: %s: revert: %w", rec.Client, err))
		}
	}
	return errs
}

// Index keys writers by name, for Revert.
func Index(ws []ClientWriter) map[string]ClientWriter {
	out := make(map[string]ClientWriter, len(ws))
	for _, w := range ws {
		out[w.Name()] = w
	}
	return out
}

// Names lists writers in a stable order, for reporting.
func Names(ws []ClientWriter) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name())
	}
	sort.Strings(out)
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(no backup was recorded)"
	}
	return s
}
