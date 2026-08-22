// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"go.sovrenix.com/larri/internal/core"
)

// LabelKeyEnv is where the label sealing key is read from.
const LabelKeyEnv = "LARRI_LABEL_KEY"

// LabelKeySource says where a key came from, for the notice LARRI prints.
type LabelKeySource string

const (
	LabelKeyFromEnv  LabelKeySource = "environment"
	LabelKeyFromFile LabelKeySource = "key file"
	LabelKeyAbsent   LabelKeySource = "none configured"
)

// ResolveLabelKey finds the key used to seal provider-side labels.
//
// It resolves the way every other secret does (FR-SEC-01): environment first,
// then a file the operator points at. It is **never generated silently**, and
// that is the point of making it configuration rather than a detail.
//
// A key LARRI invented and stored by itself would be a key the operator does
// not know exists, cannot back up, and loses on reinstall — at which point the
// details of every surviving rig become unreadable, in exactly the situation
// where an orphan most needs explaining. A key the operator supplies is a key
// they can keep.
//
// When none is configured, labels are written **unsealed but attributable**:
// the rig ID is still stamped, so orphan detection and teardown are unaffected.
// Only the descriptive fields are withheld, and LARRI says so rather than
// pretending the resource is unmarked.
func ResolveLabelKey(env func(string) string, readFile func(string) ([]byte, error)) ([]byte, LabelKeySource, error) {
	if env == nil {
		env = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	if v := strings.TrimSpace(env(LabelKeyEnv)); v != "" {
		key, err := decodeLabelKey(v)
		if err != nil {
			return nil, LabelKeyAbsent, fmt.Errorf("config: %s: %w", LabelKeyEnv, err)
		}
		return key, LabelKeyFromEnv, nil
	}
	if path := strings.TrimSpace(env(LabelKeyEnv + "_FILE")); path != "" {
		b, err := readFile(path)
		if err != nil {
			return nil, LabelKeyAbsent, fmt.Errorf("config: read label key: %w", err)
		}
		key, err := decodeLabelKey(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, LabelKeyAbsent, fmt.Errorf("config: %s: %w", path, err)
		}
		return key, LabelKeyFromFile, nil
	}
	return nil, LabelKeyAbsent, nil
}

func decodeLabelKey(v string) ([]byte, error) {
	for _, dec := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := dec.DecodeString(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, fmt.Errorf("expected 32 bytes of base64; generate one with: larri label-key")
}

// LabelSealer builds a sealer from a resolved key, or nil when none is set.
func LabelSealer(key []byte) (core.Sealer, error) {
	if len(key) == 0 {
		return nil, nil
	}
	return core.NewAEADSealer(key)
}

// LabelKeyNotice explains the consequence of the resolved state, for the
// first-run disclosure. Silence about an unset key would leave an operator
// believing details were protected when they are simply absent.
func LabelKeyNotice(src LabelKeySource) string {
	switch src {
	case LabelKeyFromEnv, LabelKeyFromFile:
		return "provider-side labels are sealed; keep " + LabelKeyEnv +
			" backed up or their details become unreadable"
	default:
		return "no " + LabelKeyEnv + " set: rigs are still labelled and " +
			"attributable, but their details are written in the clear where " +
			"the host and provider can read them"
	}
}
