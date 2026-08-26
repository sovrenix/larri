// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"go.sovrenix.com/larri/internal/config"
)

// cmdLabelKey prints a new sealing key for provider-side labels.
//
// It exists because decodeLabelKey names it as the remedy, and for a while it
// did not: an operator who mistyped a key was told to run a command that
// reported "unknown command". A remedy that does not exist is worse than no
// remedy, because it costs the reader their trust in the next one.
//
// The key goes to stdout alone and everything else to stderr, so the obvious
// use works:
//
//	export LARRI_LABEL_KEY=$(larri label-key)
//
// LARRI never generates this by itself. A key it invented and stored would be
// one the operator does not know exists, cannot back up, and loses on
// reinstall — at which point the details of every surviving rig become
// unreadable, in exactly the situation where an orphan most needs explaining.
func cmdLabelKey(args []string) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("label-key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	fmt.Println(encoded)

	fmt.Fprintf(os.Stderr, `
  A 32-byte key, base64-encoded. It seals the descriptive fields LARRI stamps
  on rented resources — model, runtime, price — so the provider and the host
  operator cannot read them. The rig ID stays in the clear, so orphan
  detection and teardown work with or without it.

  Keep it. Losing it does not strand a rig, but it does mean %s
  can no longer say what a surviving one was for.

    export %s=%s

  Or put it somewhere durable and point at that instead:

    export %s_FILE=~/.config/larri/label.key
`, "`larri orphans`", config.LabelKeyEnv, encoded, config.LabelKeyEnv)
	return nil
}
