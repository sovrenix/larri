// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Command refreshimage reports the digest and hardware requirements of a
// runtime image, so a pin and the floors derived from it move together.
//
// The floors are not free-standing facts. TORCH_CUDA_ARCH_LIST and
// CUDA_VERSION belong to one specific image, and reading them from a
// different build than the one LARRI runs is how a V100 came to pass a
// compute-capability check for an image with no Volta kernels in it.
//
// Usage: go run ./internal/devtools/refreshimage [repo:tag]
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const accept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.oci.image.manifest.v1+json"

func main() {
	ref := "vllm/vllm-openai:latest"
	if len(os.Args) > 1 {
		ref = os.Args[1]
	}
	repo, tag, ok := strings.Cut(ref, ":")
	if !ok {
		tag = "latest"
	}
	info, err := describe(repo, tag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refreshimage:", err)
		os.Exit(1)
	}
	fmt.Printf("const DefaultImage = %q\n", repo+"@"+info.digest)
	fmt.Printf("const (\n\tImageArchList = %q\n\tImageCUDA     = %q\n)\n", info.archList, info.cuda)
	fmt.Fprintf(os.Stderr, "\n%s: %.1f GB compressed, %d layers, largest %.1f GB\n",
		ref, float64(info.bytes)/1e9, info.layers, float64(info.largest)/1e9)
	fmt.Fprintf(os.Stderr, "floors implied: MinComputeCapability %d, MinCUDA %d\n",
		capFromArchList(info.archList), cudaTimesTen(info.cuda))
}

type imageInfo struct {
	digest, archList, cuda string
	bytes, largest         int64
	layers                 int
}

func describe(repo, tag string) (imageInfo, error) {
	var out imageInfo
	tok, err := token(repo)
	if err != nil {
		return out, err
	}
	base := "https://registry-1.docker.io/v2/" + repo + "/"

	var idx struct {
		Manifests []struct {
			Digest   string                            `json:"digest"`
			Platform struct{ Architecture, OS string } `json:"platform"`
		} `json:"manifests"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
		Config struct{ Digest string } `json:"config"`
	}
	if err := fetchJSON(base+"manifests/"+tag, tok, &idx); err != nil {
		return out, err
	}
	digest := ""
	for _, m := range idx.Manifests {
		if m.Platform.Architecture == "amd64" && m.Platform.OS == "linux" {
			digest = m.Digest
			break
		}
	}
	man := idx
	if digest != "" {
		if err := fetchJSON(base+"manifests/"+digest, tok, &man); err != nil {
			return out, err
		}
		out.digest = digest
	}
	for _, l := range man.Layers {
		out.bytes += l.Size
		out.layers++
		if l.Size > out.largest {
			out.largest = l.Size
		}
	}
	var cfg struct {
		Config struct{ Env []string } `json:"config"`
	}
	if err := fetchJSON(base+"blobs/"+man.Config.Digest, tok, &cfg); err != nil {
		return out, err
	}
	for _, e := range cfg.Config.Env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "TORCH_CUDA_ARCH_LIST":
			out.archList = v
		case "CUDA_VERSION":
			out.cuda = v
		}
	}
	return out, nil
}

// capFromArchList returns the lowest architecture the image has kernels for,
// times 100. That, not the engine's documented support matrix, is what the
// hardware has to clear.
func capFromArchList(list string) int {
	low := 0
	for _, f := range strings.Fields(list) {
		f = strings.TrimSuffix(f, "+PTX")
		var maj, min int
		if _, err := fmt.Sscanf(f, "%d.%d", &maj, &min); err != nil {
			continue
		}
		if n := maj*100 + min*10; low == 0 || n < low {
			low = n
		}
	}
	return low
}

func cudaTimesTen(v string) int {
	var maj, min int
	if _, err := fmt.Sscanf(v, "%d.%d", &maj, &min); err != nil {
		return 0
	}
	return maj*10 + min
}

func token(repo string) (string, error) {
	var t struct{ Token string }
	err := fetchJSON("https://auth.docker.io/token?service=registry.docker.io&scope=repository:"+repo+":pull", "", &t)
	return t.Token, err
}

func fetchJSON(url, tok string, into any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", accept)
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}
