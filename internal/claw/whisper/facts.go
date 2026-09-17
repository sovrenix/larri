// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package whisper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
)

// HFEndpoint is where model facts come from.
const HFEndpoint = "https://huggingface.co"

// Catalogue maps the names an operator writes to the repositories that hold
// them.
//
// A name table and not a size table, which is the distinction §4a draws. What
// a model is called is a fact about vocabulary and is safe to hardcode; how
// large it is is a fact about files and is measured, because an estimate errs
// in the direction that OOMs.
var Catalogue = map[string]string{
	"tiny":             "Systran/faster-whisper-tiny",
	"tiny.en":          "Systran/faster-whisper-tiny.en",
	"base":             "Systran/faster-whisper-base",
	"base.en":          "Systran/faster-whisper-base.en",
	"small":            "Systran/faster-whisper-small",
	"small.en":         "Systran/faster-whisper-small.en",
	"medium":           "Systran/faster-whisper-medium",
	"medium.en":        "Systran/faster-whisper-medium.en",
	"large-v1":         "Systran/faster-whisper-large-v1",
	"large-v2":         "Systran/faster-whisper-large-v2",
	"large-v3":         "Systran/faster-whisper-large-v3",
	"distil-large-v3":  "Systran/faster-distil-whisper-large-v3",
	"distil-medium.en": "Systran/faster-distil-whisper-medium.en",
}

// Repo resolves a name to a repository.
//
// A name containing a slash is already one, so an operator can point at a
// conversion the catalogue has never heard of without waiting for it to be
// added here.
func Repo(name string) string {
	if name == "" {
		return DefaultModel
	}
	if strings.Contains(name, "/") {
		return name
	}
	if repo, ok := Catalogue[name]; ok {
		return repo
	}
	return name
}

// Measurer reads how large a repository is.
//
// An interface so a plan can be produced without a network, which is what
// makes everything before the money testable.
type Measurer interface {
	Measure(ctx context.Context, repo string) (uint64, error)
}

// HFMeasurer sums a repository's files from the live listing.
type HFMeasurer struct {
	Endpoint string
	Token    secret.Secret
	HTTP     *http.Client
}

var _ Measurer = (*HFMeasurer)(nil)

// NewHFMeasurer builds the live measurer.
func NewHFMeasurer(token secret.Secret) *HFMeasurer {
	return &HFMeasurer{Endpoint: HFEndpoint, Token: token}
}

// Measure returns the total bytes the host will download.
//
// The whole repository rather than a guess at which files matter, because
// snapshot_download fetches the whole repository. Sizing the rig against the
// model file alone would understate the disk the fetch actually needs.
func (m *HFMeasurer) Measure(ctx context.Context, repo string) (uint64, error) {
	base := m.Endpoint
	if base == "" {
		base = HFEndpoint
	}
	url := base + "/api/models/" + repo + "?blobs=true"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if !m.Token.Empty() {
		req.Header.Set("Authorization", "Bearer "+m.Token.Reveal())
	}
	cl := m.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, errs.Newf(errs.ClassModelFailure, "whisper.Measure",
			"%s: %v", repo, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, errs.Newf(errs.ClassModelFailure, "whisper.Measure",
			"no such model %s: check the name, or pass a full repository", repo)
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, errs.Newf(errs.ClassModelFailure, "whisper.Measure",
			"%s is gated and the token cannot read it", repo)
	default:
		return 0, errs.Newf(errs.ClassModelFailure, "whisper.Measure",
			"%s: http %d", repo, resp.StatusCode)
	}

	var body struct {
		Siblings []struct {
			Name string `json:"rfilename"`
			Size *int64 `json:"size"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, errs.Newf(errs.ClassModelFailure, "whisper.Measure",
			"%s: decode: %v", repo, err)
	}
	var total uint64
	for _, s := range body.Siblings {
		if s.Size != nil && *s.Size > 0 {
			total += uint64(*s.Size)
		}
	}
	if total == 0 {
		// A listing that reports no sizes proves nothing, and §4a is explicit
		// that absence of evidence is never failure — but here the number is
		// what decides the card, so there is nothing to fall back to.
		return 0, errs.Newf(errs.ClassModelFailure, "whisper.Measure",
			"%s reports no file sizes", repo)
	}
	return total, nil
}

// StaticMeasurer answers from a table, for tests.
type StaticMeasurer map[string]uint64

var _ Measurer = StaticMeasurer(nil)

func (s StaticMeasurer) Measure(_ context.Context, repo string) (uint64, error) {
	if n, ok := s[repo]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("whisper: no size for %s", repo)
}

// ResidentBytes is how much of a downloaded model ends up in VRAM.
//
// CTranslate2 quantises at load rather than at publication: the repository is
// float16 whatever compute type is asked for, so the download does not shrink
// and the resident weights do. Conflating the two is how a rig is sized for a
// card that cannot hold it, or a disk that cannot hold the fetch.
func ResidentBytes(downloaded uint64, computeType string) uint64 {
	switch computeType {
	case "int8", "int8_float16", "int8_bfloat16":
		return downloaded / 2
	default:
		return downloaded
	}
}
