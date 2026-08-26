// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package core holds LARRI's normalised domain vocabulary — the types of
// LARRI-DES-001 §4.
//
// These types are the contract between layers. Everything provider-specific
// dies at the adapter boundary (P1): Vast's offers and bids and RunPod's pods
// and cloud tiers are normalised into Offer and Instance here, and nothing
// above the provider layer may branch on which provider it is talking to.
//
// This package is not in §3's package layout. It exists because Criteria,
// Offer, and Instance are needed by provider, rank, state, and daemon alike,
// and ModelSpec and SizingPlan by sizing, runtime, and state — so placing them
// in any one of those packages would make the others depend on it. The layout
// in §3 should gain this entry.
package core

import (
	"encoding/json"
	"time"
)

// Criteria is what the operator asks for.
type Criteria struct {
	GPUModel       []string `json:"gpu_model,omitempty"` // ["A100","H100"]; empty means any
	GPUCount       int      `json:"gpu_count,omitempty"`
	VRAMPerGPUGB   int      `json:"vram_per_gpu_gb,omitempty"`
	VRAMTotalGB    int      `json:"vram_total_gb,omitempty"`
	CPUCores       int      `json:"cpu_cores,omitempty"`
	RAMGB          int      `json:"ram_gb,omitempty"`
	DiskGB         int      `json:"disk_gb,omitempty"`
	Regions        []string `json:"regions,omitempty"` // allow-list; empty means any
	BlockRegions   []string `json:"block_regions,omitempty"`
	MaxPriceHr     float64  `json:"max_price_hr,omitempty"` // USD
	Interruptible  Tristate `json:"interruptible"`          // zero value forbids (Q-04)
	MinReliability float64  `json:"min_reliability,omitempty"`
	Providers      []string `json:"providers,omitempty"`
	CertifiedOnly  bool     `json:"certified_only,omitempty"` // datacentre-certified hosts only
}

// Source is where model weights come from.
type Source string

const (
	SourceHuggingFace    Source = "huggingface"
	SourceOllamaRegistry Source = "ollama"
	SourceLocalPath      Source = "local"
	SourceURL            Source = "url"
)

// ModelSpec is what the operator wants served.
type ModelSpec struct {
	Ref          string   `json:"ref"` // "Qwen/Qwen3-Coder-30B", "llama3.1:70b", or a path
	Source       Source   `json:"source"`
	Revision     string   `json:"revision,omitempty"` // resolved commit, not a branch
	Quantization string   `json:"quantization,omitempty"`
	ContextLen   int      `json:"context_len,omitempty"`
	ServedName   string   `json:"served_name"` // stable wire name, independent of Ref
	Gated        bool     `json:"gated,omitempty"`
	ToolCalling  Tristate `json:"tool_calling"`
	ToolParser   string   `json:"tool_parser,omitempty"` // empty means auto-detect
}

// Offer is a normalised purchasable unit.
type Offer struct {
	Provider      string  `json:"provider"`
	OfferID       string  `json:"offer_id"`
	GPUModel      string  `json:"gpu_model"`
	GPUCount      int     `json:"gpu_count"`
	VRAMPerGPUGB  int     `json:"vram_per_gpu_gb"`
	CPUCores      int     `json:"cpu_cores"`
	RAMGB         int     `json:"ram_gb"`
	DiskGB        int     `json:"disk_gb"`
	Region        string  `json:"region"`
	PriceHr       float64 `json:"price_hr"`
	Interruptible bool    `json:"interruptible"`
	// Reliability is the provider's own score, normalised 0..1. **Zero means
	// the provider does not report one**, which is not the same as a host
	// that always fails — no marketplace lists those. Ask HasReliability
	// rather than comparing against zero.
	//
	// The distinction has teeth: a catalogue-style provider (a GPU type with
	// availability, rather than a named machine) has nothing to report, and a
	// reliability floor applied to its offers would reject every one of them
	// with "reliability 0.00 below floor 0.90" — a rejection that reads like
	// a market with nothing in it.
	Reliability float64 `json:"reliability"`   // normalised 0..1; 0 = unreported
	NetDownMbps float64 `json:"net_down_mbps"` // download time is billed time
	CUDAVersion string  `json:"cuda_version,omitempty"`

	// ComputeCapability is the GPU architecture level times 100 — 610 for
	// Pascal, 700 for Volta, 890 for Ada. It is a *selection* input, not a
	// diagnostic: vLLM will not run below 700, so renting a Pascal card to
	// serve with vLLM buys hardware that cannot work at any price.
	ComputeCapability int    `json:"compute_capability,omitempty"`
	DriverVersion     string `json:"driver_version,omitempty"`
	Certified         bool   `json:"certified,omitempty"`

	// MachineID identifies the physical host. A marketplace lists several
	// offers per machine, so falling back on offer ID alone can land on the
	// same box that just failed — which a live run did, twice.
	MachineID string          `json:"machine_id,omitempty"`
	Raw       json.RawMessage `json:"-"` // provider payload, for debugging only
}

// VRAMTotalGB is the aggregate VRAM an offer provides.
func (o Offer) VRAMTotalGB() int { return o.VRAMPerGPUGB * o.GPUCount }

// Instance is a live provider resource.
type Instance struct {
	Provider   string  `json:"provider"`
	InstanceID string  `json:"instance_id"`
	OfferID    string  `json:"offer_id"`
	PriceHr    float64 `json:"price_hr"`
	StorageHr  float64 `json:"storage_hr,omitempty"` // still charged while STOPPED
	Running    bool    `json:"running"`

	// Status and StatusMsg are the provider's own account of what the
	// instance is doing — "loading" while an image pulls, with a message
	// carrying the progress. Waiting on a boot without reading these is
	// waiting blind, and a blind timer cannot tell a 15 GB pull that is
	// working from a host that has stopped trying.
	Status    string `json:"status,omitempty"`
	StatusMsg string `json:"status_msg,omitempty"`

	// Intent is what the provider means to do with this instance, as
	// distinct from what it is doing. The two disagree exactly when
	// something has gone wrong: a live run watched an instance sit at
	// actual_status "created" — which reads as a boot in progress — while
	// intended_status had already flipped to "stopped", because the
	// container could not be created at all. Waiting on a host the provider
	// has stopped trying to start is waiting forever, at cost.
	Intent string `json:"intent,omitempty"`

	// DiskUsedGB is how much the container has written, when the provider
	// reports it. Bytes landing on disk is progress that owes nothing to the
	// wording of a status message, which is what makes it worth carrying: a
	// pull can be moving several GB while the text the provider publishes
	// stays identical. Negative or absent means unreported.
	DiskUsedGB float64           `json:"disk_used_gb,omitempty"`
	SSHHost    string            `json:"ssh_host"`
	SSHPort    int               `json:"ssh_port"`
	SSHProxied bool              `json:"ssh_proxied,omitempty"`
	PublicIP   string            `json:"public_ip,omitempty"`
	PortMap    map[int]int       `json:"port_map,omitempty"` // container port -> external
	CreatedAt  time.Time         `json:"created_at"`
	Labels     map[string]string `json:"labels,omitempty"` // includes the ownership marker
}

// LabelKey is the provider-side label under which a rig stamps its ID.
// FR-STATE-04: it is what makes orphan attribution possible, which is why
// Rig.ID is minted before the create call rather than after it returns.
const LabelKey = "larri"

// LabelRawKey holds the full provider-side marker as it was read back, so a
// recovering LARRI can parse whatever a past version wrote.
const LabelRawKey = "larri.raw"

// RigID returns the rig this instance belongs to, and whether it is ours.
func (i Instance) RigID() (string, bool) {
	id, ok := i.Labels[LabelKey]
	return id, ok && id != ""
}

// Label returns everything the provider-side marker records about this rig.
//
// Adapters store the raw marker under LabelRaw so recovery can read it even
// when local state is gone entirely — the case where a bare rig ID would be a
// puzzle rather than an answer.
func (i Instance) Label() (Label, bool) {
	if raw, ok := i.Labels[LabelRawKey]; ok {
		return DecodeLabel(raw)
	}
	if id, ok := i.RigID(); ok {
		return Label{RigID: id, Version: LabelVersion}, true
	}
	return Label{}, false
}

// SizingPlan is the output of the sizing engine (§7.3).
type SizingPlan struct {
	RequiredVRAMBytes  uint64 `json:"required_vram_bytes"`
	WeightsBytes       uint64 `json:"weights_bytes"`
	KVCacheBytes       uint64 `json:"kv_cache_bytes"`
	FitsInVRAM         bool   `json:"fits_in_vram"`
	TensorParallelSize int    `json:"tensor_parallel_size"`

	// ComputeCapability is the architecture level of the hardware actually
	// placed, times 100. It is read from the GPU rather than taken from the
	// listing, because the engine's minimum and the weights' minimum are
	// different numbers: vLLM runs on 7.0, but bfloat16 needs 8.0, and a
	// marketplace will happily rent you a Volta box for bf16 weights.
	ComputeCapability int      `json:"compute_capability,omitempty"`
	GPUMemUtilization float64  `json:"gpu_mem_utilization"`
	OffloadLayers     int      `json:"offload_layers"`
	ContextLen        int      `json:"context_len"` // possibly reduced from requested
	Warnings          []string `json:"warnings,omitempty"`
}

// RuntimeKind identifies an inference engine.
type RuntimeKind string

const (
	RuntimeVLLM     RuntimeKind = "vllm"
	RuntimeLlamaCpp RuntimeKind = "llamacpp"
	RuntimeOllama   RuntimeKind = "ollama"
)

// Transition is one entry in a rig's history and in the journal.
type Transition struct {
	At       time.Time      `json:"ts"`
	From     LifecycleState `json:"from"`
	To       LifecycleState `json:"to"`
	Note     string         `json:"note,omitempty"`
	TraceID  string         `json:"trace_id,omitempty"`
	SpanID   string         `json:"span_id,omitempty"`
	PriceHr  float64        `json:"price_hr,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Offer    string         `json:"offer,omitempty"`
}

// WiringRecord is what was changed on a client, so it can be reverted exactly.
type WiringRecord struct {
	Client     string    `json:"client"`
	Tier       string    `json:"tier"` // A | B | C, see §10.2.1
	Path       string    `json:"path,omitempty"`
	BackupPath string    `json:"backup_path,omitempty"`
	AppliedAt  time.Time `json:"applied_at"`
	Verified   bool      `json:"verified"` // a probe confirmed the client reached the endpoint
}

// Rig is the whole managed unit and the root of persisted state.
type Rig struct {
	ID        string         `json:"id"` // ULID, minted before any provider call
	State     LifecycleState `json:"state"`
	Criteria  Criteria       `json:"criteria"`
	Model     ModelSpec      `json:"model"`
	Runtime   RuntimeKind    `json:"runtime"`
	Offer     Offer          `json:"offer"`
	Instance  *Instance      `json:"instance,omitempty"` // nil until CREATING resolves
	Plan      SizingPlan     `json:"plan"`
	LocalPort int            `json:"local_port"`

	// HostKeyFingerprint is the SSH host key pinned at bring-up. A public
	// fingerprint, so persisting it discloses nothing, and persisting it is
	// what lets a later change be recognised as a change rather than as a
	// first sight (FR-SEC-04).
	HostKeyFingerprint string         `json:"host_key_fingerprint,omitempty"`
	Wiring             []WiringRecord `json:"wiring,omitempty"`
	History            []Transition   `json:"history,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	End                *Termination   `json:"end,omitempty"` // nil while the rig lives
}

// Billable reports whether this rig currently costs money.
func (r *Rig) Billable() bool { return r.State.Billable() }

// HasReliability reports whether the provider supplied a reliability score.
//
// Named rather than left as a comparison against zero, so the convention lives
// in one place and a floor cannot be applied to a provider that has nothing to
// measure.
func (o Offer) HasReliability() bool { return o.Reliability > 0 }
