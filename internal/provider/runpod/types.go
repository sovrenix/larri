// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"fmt"
	"strings"

	"go.sovrenix.com/larri/internal/core"
)

// gpuType is one entry in the GraphQL catalogue.
type gpuType struct {
	ID             string `json:"id"` // matches the REST gpuTypeIds enum exactly
	DisplayName    string `json:"displayName"`
	MemoryInGb     int    `json:"memoryInGb"`
	SecureCloud    bool   `json:"secureCloud"`
	CommunityCloud bool   `json:"communityCloud"`
	MaxGPUCount    int    `json:"maxGpuCount"`
	LowestPrice    *struct {
		MinimumBidPrice      *float64 `json:"minimumBidPrice"`
		UninterruptablePrice *float64 `json:"uninterruptablePrice"`
	} `json:"lowestPrice"`
}

// catalogueQuery asks for everything an Offer needs.
//
// The id is requested as well as the display name because the id is what
// POST /pods accepts — normalising to the pretty name would produce offers
// that cannot be purchased.
const catalogueQuery = `query {
  gpuTypes {
    id displayName memoryInGb secureCloud communityCloud maxGpuCount
    lowestPrice(input: {gpuCount: 1}) { minimumBidPrice uninterruptablePrice }
  }
}`

// normalise turns a catalogue entry into an offer LARRI can rank.
//
// Returns ok=false for a type with no price. RunPod publishes about ten of
// those at any time — hardware it lists but cannot currently place — and an
// offer with no price is one selection cannot rank, every ceiling ignores, and
// cost accounting cannot follow. Dropping them is the honest reading of "no
// price": not free, unavailable.
func (g gpuType) normalise(interruptible bool) (core.Offer, bool) {
	if g.LowestPrice == nil {
		return core.Offer{}, false
	}
	var price float64
	switch {
	case interruptible && g.LowestPrice.MinimumBidPrice != nil:
		price = *g.LowestPrice.MinimumBidPrice
	case g.LowestPrice.UninterruptablePrice != nil:
		price = *g.LowestPrice.UninterruptablePrice
	default:
		return core.Offer{}, false
	}
	if price <= 0 || g.MemoryInGb <= 0 || g.ID == "" {
		return core.Offer{}, false
	}
	if !purchasable(g.ID) {
		return core.Offer{}, false
	}
	name := g.DisplayName
	if name == "" {
		name = g.ID
	}
	return core.Offer{
		Provider: "runpod",
		// The GPU type id, because that is what a create call takes. It is
		// not a machine: see MachineID below.
		OfferID:       g.ID,
		GPUModel:      name,
		GPUCount:      1,
		VRAMPerGPUGB:  g.MemoryInGb,
		PriceHr:       price,
		Interruptible: interruptible,

		// Deliberately absent, both of them.
		//
		// MachineID: RunPod places the pod, so there is no host to name — and
		// naming the GPU type here would make one failed pod exclude every
		// machine of that type for the rest of the run (§5.4).
		//
		// Reliability: there is no host to score. Zero means unreported, and
		// the floor skips offers that report none rather than rejecting the
		// whole catalogue.
		ComputeCapability: computeCapability(g.ID),
	}, true
}

// computeCapability maps a GPU type to its architecture level ×100.
//
// vLLM needs Volta or newer and the selection filter checks this before
// spending, so a wrong answer here costs a rental. Only families LARRI can
// place are listed; anything unrecognised returns 0, which the runtime
// requirement treats as unknown rather than as unsuitable — refusing hardware
// on a lookup miss would be worse than letting the launch report the truth.
func computeCapability(id string) int {
	s := strings.ToUpper(id)
	switch {
	case strings.Contains(s, "B200"), strings.Contains(s, "GB200"):
		return 1000 // Blackwell
	case strings.Contains(s, "H100"), strings.Contains(s, "H200"), strings.Contains(s, "H800"):
		return 900 // Hopper
	case strings.Contains(s, "L40"), strings.Contains(s, "L4"),
		strings.Contains(s, "RTX 40"), strings.Contains(s, "ADA"):
		return 890 // Ada
	case strings.Contains(s, "A100"), strings.Contains(s, "A40"), strings.Contains(s, "A30"):
		return 800 // Ampere (datacentre)
	case strings.Contains(s, "RTX 30"), strings.Contains(s, "A4000"), strings.Contains(s, "A4500"),
		strings.Contains(s, "A5000"), strings.Contains(s, "A6000"), strings.Contains(s, "A2000"):
		return 860 // Ampere (workstation)
	case strings.Contains(s, "V100"):
		return 700 // Volta
	case strings.Contains(s, "MI300"), strings.Contains(s, "MI250"), strings.Contains(s, "INSTINCT"):
		return 0 // AMD: not a CUDA capability at all
	}
	return 0
}

// pod is a REST v2 pod, in the fields LARRI reads.
type pod struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	DesiredStatus     string            `json:"desiredStatus"` // RUNNING | EXITED | TERMINATED
	CostPerHr         *float64          `json:"costPerHr"`
	AdjustedCostPerHr *float64          `json:"adjustedCostPerHr"`
	PublicIP          string            `json:"publicIp"`
	PortMappings      map[string]int    `json:"portMappings"`
	Image             string            `json:"imageName"`
	Env               map[string]string `json:"env"`
	ContainerDiskInGb int               `json:"containerDiskInGb"`
	VolumeInGb        int               `json:"volumeInGb"`
	LastStatusChange  string            `json:"lastStatusChange"`
	Machine           *struct {
		GPUTypeID  string `json:"gpuTypeId"`
		DataCenter string `json:"dataCenterId"`
	} `json:"machine"`
	GPU *struct {
		Count int    `json:"count"`
		ID    string `json:"id"`
	} `json:"gpu"`
}

// normalise turns a pod into an Instance.
//
// Running is derived from desiredStatus rather than from the presence of an
// address: EXITED is a pod that still exists and still bills for its volume,
// and reading it as absent is the mistake that journals a billing resource as
// destroyed (R-13, §12.4).
func (p pod) normalise() (core.Instance, error) {
	if p.ID == "" {
		return core.Instance{}, fmt.Errorf("runpod: pod with no id")
	}
	inst := core.Instance{
		Provider:   "runpod",
		InstanceID: p.ID,
		Running:    strings.EqualFold(p.DesiredStatus, "RUNNING"),
		Status:     strings.ToLower(p.DesiredStatus),
		PublicIP:   p.PublicIP,
		Labels:     map[string]string{},
	}
	// The marker is normalised to the bare rig ID at the adapter boundary.
	//
	// Reconciliation compares it against a rig ID it already holds, so an
	// adapter that stored the prefixed "larri:<id>" form would make every
	// comparison fail and leave the orphan invisible. This is the exact drift
	// the shared conformance suite exists to catch — and it caught it here,
	// on the first run of a second adapter.
	if p.Name != "" {
		inst.Labels[core.LabelRawKey] = p.Name
		if id := labelRigID(p.Name); id != "" {
			inst.Labels[core.LabelKey] = id
		}
	}
	if p.CostPerHr != nil {
		inst.PriceHr = *p.CostPerHr
	}
	if p.AdjustedCostPerHr != nil && *p.AdjustedCostPerHr > 0 {
		inst.PriceHr = *p.AdjustedCostPerHr
	}
	if p.Machine != nil {
		inst.OfferID = p.Machine.GPUTypeID
	} else if p.GPU != nil {
		inst.OfferID = p.GPU.ID
	}

	// SSH reaches the pod through a mapped port on the public address. The
	// mapping is keyed by the container port, so 22 is what to look up.
	if port, ok := p.PortMappings["22"]; ok && port > 0 {
		inst.SSHHost, inst.SSHPort = p.PublicIP, port
	}
	return inst, nil
}

// labelRigID extracts the rig ID from a "larri:<id>" marker.
//
// RunPod has no tags, so the marker rides in the pod name — which means any
// name an operator typed by hand also lands here. A name without the prefix
// is not ours and yields "", which is what keeps someone else's pod out of
// LARRI's orphan list.
func labelRigID(name string) string {
	if id, ok := strings.CutPrefix(name, core.LabelKey+":"); ok {
		return id
	}
	return ""
}

// purchasable rejects catalogue entries POST /pods will not accept.
//
// The two APIs are not kept in sync. The catalogue currently lists a literal
// "unknown" type, and several MIG partitions and Blackwell server editions
// that the create enum does not carry — three of them priced, and one of those
// ($0.50/hr for 32 GB) would rank well enough to be chosen.
//
// Matching the enum exactly would mean embedding 45 strings that go stale the
// week RunPod adds hardware, so this drops only what is unmistakably not a
// rentable type and leaves the rest to fail at create — which now falls back
// to the next offer rather than aborting the run (see createClass).
func purchasable(id string) bool {
	if id == "" || strings.EqualFold(id, "unknown") {
		return false
	}
	// MIG partitions are slices of a card, not a card, and the create enum
	// does not list the ones the catalogue advertises.
	return !strings.Contains(strings.ToLower(id), "mig ")
}
