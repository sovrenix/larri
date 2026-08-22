// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package notice holds the things an operator must be told whether or not
// they asked.
//
// Two kinds, with different lifetimes and different suppressibility:
//
//   - **Policy disclosures** are configurable defaults with consequences —
//     idle reclamation and budget breach both destroy. Shown when the
//     configuration that adopts them is created, and changeable.
//   - **Standing conditions** are properties of the product that no
//     configuration alters. The host reads your prompts. There is no flag for
//     that, so there is no flag to silence it either.
//
// The distinction matters because suppressing the first is a preference and
// suppressing the second would be a lie.
package notice

import (
	"fmt"
	"strings"
)

// PrivacyHeadline is the one line shown every time a rig starts serving.
//
// It is not suppressible. Every other surface may abbreviate, but no surface
// may omit: the operator is about to type into an endpoint whose far end is a
// stranger's machine, and that is the single most consequential fact about the
// product.
const PrivacyHeadline = "Inference runs on rented third-party hardware. " +
	"The host operator can read your prompts and completions."

// PrivacyShort is the compact form for status output and surface footers.
func PrivacyShort() string {
	return PrivacyHeadline + " Send nothing you could not afford to disclose."
}

// PrivacyFull is the first-run explanation.
//
// It explains the mechanism rather than only the rule, because an operator who
// understands *why* encryption does not help will make better decisions about
// what to paste than one who has been handed a prohibition.
func PrivacyFull() string {
	return strings.TrimSpace(`
PRIVACY: what the machine you rent can see

  ` + PrivacyHeadline + `

  Whoever owns that machine has root on it. They can read process memory, GPU
  memory, and the environment of every process — which covers your prompts,
  the model's completions, the weights, and any token used to fetch them.

  Encryption does not change this. The SSH tunnel protects your traffic from
  everyone ON THE WAY to the host; it cannot protect it FROM the host, because
  the data has to be plaintext at the point inference happens.

  Confidential computing — where memory is encrypted against the machine's own
  owner and can be attested — is the only real answer, and it is not available
  on commodity marketplace hardware.

  So the control is a decision, not a setting:

    Do not send a rented GPU anything you could not afford to disclose.

  In practice that means keeping credentials, API keys, personal data, and
  proprietary source you are not free to publish out of prompts sent to a rig.
  For work where that is not acceptable, this is the wrong tool, and saying so
  is more useful than a setting that would imply otherwise.
`)
}

// ConfigComment renders the notice for a client configuration file LARRI
// writes.
//
// This is where it does the most good. The operator reads their IDE config
// months after `larri up` scrolled past, and the endpoint sitting in that file
// looks exactly like a local one — same loopback address, same shape. The
// comment is what distinguishes it.
func ConfigComment(prefix string) string {
	lines := []string{
		"Configured by LARRI. This endpoint is a tunnel to rented third-party",
		"hardware, not a local model. The host operator can read prompts and",
		"completions sent through it. Send nothing you could not afford to",
		"disclose. Remove with: larri down",
	}
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s %s", prefix, l)
	}
	return b.String()
}

// HostSummary names the specific host now serving, for the moment a rig
// becomes ready.
//
// Naming the provider and machine makes the abstraction concrete: "rented
// hardware" is a category, and "vastai machine 44221 in US, California" is a
// place with an owner.
func HostSummary(provider, instanceID, region string) string {
	loc := region
	if loc == "" {
		loc = "location not reported"
	}
	return fmt.Sprintf("Serving from %s instance %s (%s). %s",
		provider, instanceID, loc, PrivacyHeadline)
}
