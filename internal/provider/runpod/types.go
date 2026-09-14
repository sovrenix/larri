// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.sovrenix.com/larri/internal/core"
)

// sizeGuard bounds how many sizes one query may ask for.
//
// Not a limit on what RunPod sells — the ceiling comes from the catalogue, so
// a provider that starts placing twelve-card pods is priced at twelve without
// anyone editing this. It is a bound on the query: sizes cost latency (four
// sizes take a second, thirty-two take four and a half), and a catalogue that
// ever reported an absurd figure would otherwise build an absurd query. The
// largest ever advertised is 32, by a MIG slice LARRI cannot buy, and a live
// conformance check fails if a purchasable type passes this.
const sizeGuard = 32

// defaultSizes is the ladder used when the catalogue cannot be asked how large
// a pod it will place. Ten was the ceiling across every purchasable type when
// this was written; guessing low loses offers, so it is not lower.
const defaultSizes = 10

// sizesQuery asks only how many cards each type accepts.
//
// Its own round trip, and a cheap one: the priced query cannot be built until
// the answer is known, because each size in it is a separate alias.
const sizesQuery = `query { gpuTypes { id maxGpuCount } }`

// countsTo is every size from one card up to n.
//
// Every count, not the powers of two this first shipped with. RunPod prices
// linearly — 3× H100 SXM is $10.47 against $3.49 for one — so three cards buy
// half again what two do, and pricing only 1/2/4/8 rented four cards to hold
// what three would: a third more per hour for as long as the rig lives.
//
// The engines do not agree about which counts they can use, and that is not a
// reason to hide any: llama.cpp splits by layer and takes any number, while
// vLLM needs a degree that divides the attention heads — and sizing answers
// that already, by sizing a three-card offer on the two cards vLLM can reach,
// so it loses to the cheaper two-card offer on price rather than by being
// absent from the market.
func countsTo(n int) []int {
	if n < 1 {
		n = 1
	}
	if n > sizeGuard {
		n = sizeGuard
	}
	out := make([]int, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, i)
	}
	return out
}

// sizeCeiling is the largest pod the catalogue will place from a type LARRI
// can actually buy.
func sizeCeiling(types []gpuSize, rentable func(string) bool) int {
	max := 0
	for _, g := range types {
		// A MIG type is offered as one slice whatever it will place, so its
		// count widens nothing LARRI can use.
		if rentable(g.ID) && !isMIG(g.ID) && g.MaxGPUCount > max {
			max = g.MaxGPUCount
		}
	}
	if max < 1 {
		return defaultSizes
	}
	return max
}

// gpuSize is one catalogue entry in the sizes query.
type gpuSize struct {
	ID          string `json:"id"`
	MaxGPUCount int    `json:"maxGpuCount"`
}

// gpuPrice is what a GPU type costs at one particular count.
//
// Stock is per count as well as per type, and the two disagree routinely: a
// live catalogue read had H100 SXM at High for one card, Medium for two and
// Low for four. Reading the single-card status and applying it to an
// eight-card offer is how selection recommends a pod RunPod will refuse to
// place.
type gpuPrice struct {
	MinimumBidPrice      *float64 `json:"minimumBidPrice"`
	UninterruptablePrice *float64 `json:"uninterruptablePrice"`
	// StockStatus is High, Medium, Low or absent, and it predicts whether
	// a create succeeds. Measured, not assumed: an A40 (High) and an RTX
	// 4090 (Medium) both created on request, while an RTX 3070 (Low) was
	// refused with "there are no instances currently available".
	StockStatus *string `json:"stockStatus"`
}

// gpuType is one entry in the GraphQL catalogue.
type gpuType struct {
	ID             string `json:"id"` // matches the REST gpuTypeIds enum exactly
	DisplayName    string `json:"displayName"`
	MemoryInGb     int    `json:"memoryInGb"`
	SecureCloud    bool   `json:"secureCloud"`
	CommunityCloud bool   `json:"communityCloud"`

	// MaxGPUCount is how many of this type RunPod will put in one pod. It
	// was read and discarded until multi-GPU offers existed, which is why a
	// model needing more VRAM than the largest single card was reported
	// unsatisfiable on a provider that sells eight-card pods.
	MaxGPUCount int `json:"maxGpuCount"`

	// Prices is one entry per size the query asked for, keyed by card count
	// and filled from the price<N> aliases. A map rather than a field each,
	// so the set of sizes lives in one place: a fixed field list is how the
	// query and the decoder drift apart.
	Prices map[int]*gpuPrice `json:"-"`
}

// UnmarshalJSON reads the scalar fields and every price<N> alias.
func (g *gpuType) UnmarshalJSON(b []byte) error {
	type plain gpuType // no UnmarshalJSON, so this does not recurse
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	p.Prices = map[int]*gpuPrice{}
	for k, raw := range all {
		n, ok := strings.CutPrefix(k, "price")
		if !ok {
			continue
		}
		count, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		// A size the catalogue cannot price answers null, and unmarshalling
		// null into a struct succeeds — leaving an entry that is present and
		// empty, which reads as priced.
		if string(raw) == "null" {
			continue
		}
		var price gpuPrice
		if err := json.Unmarshal(raw, &price); err != nil {
			continue // a shape this version does not know
		}
		p.Prices[count] = &price
	}
	*g = gpuType(p)
	return nil
}

// priceAt returns the catalogue's entry for a GPU count, or nil.
func (g gpuType) priceAt(count int) *gpuPrice { return g.Prices[count] }

// catalogueQuery asks for everything an Offer needs.
//
// The id is requested as well as the display name because the id is what
// POST /pods accepts — normalising to the pretty name would produce offers
// that cannot be purchased.
//
// Every GPU count is aliased into the same query rather than fetched in a
// round trip each, so pricing the whole market at every size costs exactly
// what pricing it at one size cost before.
//
// Priced on **Secure Cloud**, because that is the only cloud Create rents
// from. Without the filter lowestPrice answers across both clouds, which in
// practice means Community's rate — and a live pod quoted at $2.78/hr from the
// catalogue billed $3.18/hr, 14% above the figure ranking chose it on and
// above the --max-price it was meant to respect. Stock is read the same way,
// and it disagrees too: 2× A100 SXM was Medium on Secure while Community had
// none at all.
func buildCatalogueQuery(counts []int) string {
	var b strings.Builder
	b.WriteString("query {\n  gpuTypes {\n")
	b.WriteString("    id displayName memoryInGb secureCloud communityCloud maxGpuCount\n")
	for _, n := range counts {
		fmt.Fprintf(&b, "    price%d: lowestPrice(input: {gpuCount: %d, secureCloud: true})"+
			" { minimumBidPrice uninterruptablePrice stockStatus }\n", n, n)
	}
	b.WriteString("  }\n}")
	return b.String()
}

// dropReason says why a catalogue entry cannot be offered, or "" if it can.
type dropReason string

const (
	dropUnpriced      dropReason = "no secure-cloud price"
	dropOutOfStock    dropReason = "out of stock"
	dropLowStock      dropReason = "low stock: allow low stock to consider them"
	dropUnpurchasable dropReason = "not a rentable type"
	dropTooManyGPUs   dropReason = "more gpus than the type allows"
	dropMIGSlices     dropReason = "more than one mig slice"
)

// countSep joins a GPU type id to the number of cards in an offer.
//
// RunPod sells one catalogue entry at several sizes, so the type id alone
// stopped being a unique offer id the moment multi-GPU offers existed —
// and the provider conformance suite requires uniqueness, because two
// listings sharing an id would have selection ranking one against itself.
// The character is one RunPod type ids do not contain, and Create decodes it
// back to the id the REST API accepts.
const countSep = "#"

// offerID encodes a purchase: which GPU type, and how many of them.
func offerID(typeID string, count int) string {
	return typeID + countSep + strconv.Itoa(count)
}

// gpuTypeID recovers the id POST /pods accepts from an offer id.
//
// Tolerant of a bare type id, because a rig created before offers carried a
// count is still in state and still has to be destroyable.
func gpuTypeID(offer string) string {
	if i := strings.LastIndex(offer, countSep); i >= 0 {
		return offer[:i]
	}
	return offer
}

// normalise turns a catalogue entry into an offer LARRI can rank.
//
// Entries are dropped for three reasons and all three are the same reason: an
// offer LARRI cannot actually rent is worse than no offer, because selection
// will choose it and the operator will watch it fail.
//
//   - **No price.** Selection cannot rank it, no ceiling applies to it, and
//     cost accounting cannot follow it. "No price" means unavailable, not
//     free.
//   - **Out of stock.** No stock status at all. Stock is re-read on every
//     search, so this is current reality rather than a permanent exclusion —
//     a type that comes back into stock comes back into the list.
//   - **Low stock, unless allowed.** Low is not unplaceable: five of six
//     Low-stock Secure Cloud pods probed on 2026-09-14 — RTX 2000 Ada, RTX
//     A4000, A100 80GB PCIe, H100 NVL, B200 — were placed on a machine at
//     once, and the sixth was refused with "no instances currently
//     available" and nothing created. An RTX 3070 had been refused the same
//     way earlier. A refusal costs an attempt, not money, so whether to spend
//     attempts on it is the operator's call; allowed, the offer is marked.
//   - **Not purchasable.** The catalogue advertises types POST /pods rejects;
//     rentable is the create call's own list of the ones it accepts.
func (g gpuType) normalise(count int, mode core.Tristate, rentable func(string) bool, allowLow bool) (core.Offer, dropReason, bool) {
	if count > 1 && g.MaxGPUCount > 0 && count > g.MaxGPUCount {
		return core.Offer{}, dropTooManyGPUs, false
	}
	// A MIG slice is part of a card, and a CUDA process sees one MIG device
	// however many a pod holds. Twenty-two slices of a PRO 6000 read as 528 GB
	// for a split model; the engine would load into 24 GB of it.
	if count > 1 && isMIG(g.ID) {
		return core.Offer{}, dropMIGSlices, false
	}
	lowest := g.priceAt(count)
	if lowest == nil {
		return core.Offer{}, dropUnpriced, false
	}
	var (
		price         float64
		interruptible bool
	)
	switch mode {
	case core.Require:
		if lowest.MinimumBidPrice == nil {
			return core.Offer{}, dropUnpriced, false
		}
		price = *lowest.MinimumBidPrice
		interruptible = true
	case core.Allow:
		if lowest.MinimumBidPrice != nil {
			price = *lowest.MinimumBidPrice
			interruptible = true
		} else if lowest.UninterruptablePrice != nil {
			price = *lowest.UninterruptablePrice
		} else {
			return core.Offer{}, dropUnpriced, false
		}
	default: // Forbid
		if lowest.UninterruptablePrice == nil {
			return core.Offer{}, dropUnpriced, false
		}
		price = *lowest.UninterruptablePrice
	}
	if price <= 0 || g.MemoryInGb <= 0 || g.ID == "" {
		return core.Offer{}, dropUnpriced, false
	}
	if !rentable(g.ID) {
		return core.Offer{}, dropUnpurchasable, false
	}
	low := false
	switch stock(lowest.StockStatus) {
	case stockAvailable:
	case stockLow:
		if !allowLow {
			return core.Offer{}, dropLowStock, false
		}
		low = true
	default:
		return core.Offer{}, dropOutOfStock, false
	}
	name := g.DisplayName
	if name == "" {
		name = g.ID
	}
	return core.Offer{
		Provider: "runpod",
		// The GPU type id and the number of cards, because a create call
		// takes both. It is not a machine: see MachineID below.
		OfferID:       offerID(g.ID, count),
		GPUModel:      name,
		GPUCount:      count,
		VRAMPerGPUGB:  g.MemoryInGb,
		PriceHr:       price,
		Interruptible: interruptible,
		LowStock:      low,
		GPUVendor:     vendorOf(g.ID),

		// Deliberately absent, both of them.
		//
		// MachineID: RunPod places the pod, so there is no host to name — and
		// naming the GPU type here would make one failed pod exclude every
		// machine of that type for the rest of the run (§5.4). The fallback
		// knows: with no machine to exclude it excludes the listing, and
		// retires the model only on a second failure.
		//
		// Reliability: there is no host to score. Zero means unreported, and
		// the floor skips offers that report none rather than rejecting the
		// whole catalogue.
		ComputeCapability: computeCapability(g.ID),
	}, "", true
}

type stockLevel int

const (
	stockNone stockLevel = iota
	stockLow
	stockAvailable
)

// stock reads a catalogue stock status.
//
// High and Medium have always created on request. Low usually does and
// sometimes is refused, so it is its own level. Absent, or anything else,
// means the catalogue has nothing to offer.
func stock(status *string) stockLevel {
	if status == nil {
		return stockNone
	}
	switch strings.ToLower(strings.TrimSpace(*status)) {
	case "high", "medium":
		return stockAvailable
	case "low":
		return stockLow
	}
	return stockNone
}

// vendorOf names who makes a GPU type. Every type RunPod lists is NVIDIA's
// except AMD's Instinct cards, which say so in the name.
func vendorOf(id string) string {
	s := strings.ToUpper(id)
	if strings.HasPrefix(s, "AMD ") || strings.Contains(s, "INSTINCT") ||
		strings.Contains(s, "MI300") || strings.Contains(s, "MI250") {
		return "amd"
	}
	return "nvidia"
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

// RunPod bills disk apart from the GPU, and neither the catalogue price nor a
// pod's costPerHr includes it: a pod with 220 GB of disk reported costPerHr
// 0.49, the catalogue's A40 rate exactly. Dollars per gigabyte per month, as
// RunPod publishes them; the month length it does not publish, and Vast's 720
// hours is assumed. The same pod's bill agreed: $0.0051 above the GPU rate for
// 636 s, $0.029/hr of disk against the $0.031/hr these predict.
const (
	runningDiskPerGBMonth = 0.10 // container disk and volume, while the pod runs
	stoppedDiskPerGBMonth = 0.20 // the volume, while the pod is stopped
	hoursPerMonth         = 720
)

// runningDiskHr is what a pod's disks bill by the hour while it runs.
func runningDiskHr(containerGB, volumeGB int) float64 {
	return float64(containerGB+volumeGB) * runningDiskPerGBMonth / hoursPerMonth
}

// stoppedDiskHr is what a stopped pod bills by the hour: its volume. The
// container disk is discarded when a pod stops.
func stoppedDiskHr(volumeGB int) float64 {
	return float64(volumeGB) * stoppedDiskPerGBMonth / hoursPerMonth
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
	// costPerHr is the GPU alone. The disks bill beside it, so the rate the
	// pod costs while it runs is both, and what it costs stopped is the
	// volume at the stopped rate.
	if inst.PriceHr > 0 {
		inst.PriceHr += runningDiskHr(p.ContainerDiskInGb, p.VolumeInGb)
	}
	inst.StorageHr = stoppedDiskHr(p.VolumeInGb)
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

// purchasable is the fallback when the create schema cannot be read: it
// rejects only catalogue entries known not to be rentable.
//
// The two APIs are not kept in sync. The catalogue lists a literal "unknown"
// type and MIG partitions the create enum does not carry — priced, and one
// ($0.50/hr for 32 GB) would rank well enough to be chosen. It also lists
// "NVIDIA RTX PRO 4500 Blackwell Server Edition", which nothing here can tell
// from a real type by its name; that is what reading the schema is for, and
// without it the type costs one failed create before falling back.
func purchasable(id string) bool {
	if id == "" || strings.EqualFold(id, "unknown") {
		return false
	}
	// MIG partitions are slices of a card, not a card, and the create enum
	// does not list the ones the catalogue advertises.
	return !isMIG(id)
}

// isMIG reports a GPU type that is a MIG partition of a card, named like
// "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb". "MI300X" is a
// card, not a partition.
func isMIG(id string) bool {
	return strings.Contains(strings.ToLower(id), "mig ")
}
