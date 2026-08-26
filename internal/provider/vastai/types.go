// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vastai

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// mbPerGB is Vast's unit for GPU and CPU memory: gpu_ram arrives in MiB.
//
// This constant is the single most dangerous number in the adapter. Reading
// 81920 as gigabytes would make an A100 look like an 80 TB card, every sizing
// check would pass, and the rig would OOM after the operator paid to boot it.
// normalise() therefore range-checks the result rather than trusting the unit.
const mbPerGB = 1024

// ---- search ---------------------------------------------------------------

type boolFilter struct {
	Eq bool `json:"eq"`
}
type intFilter struct {
	Gte *int `json:"gte,omitempty"`
	Lte *int `json:"lte,omitempty"`
}
type floatFilter struct {
	Gte *float64 `json:"gte,omitempty"`
	Lte *float64 `json:"lte,omitempty"`
}
type stringsFilter struct {
	In []string `json:"in,omitempty"`
}

type searchRequest struct {
	Limit       int            `json:"limit,omitempty"`
	Type        string         `json:"type,omitempty"`
	Order       [][]string     `json:"order,omitempty"`
	Rentable    *boolFilter    `json:"rentable,omitempty"`
	GPUName     *stringsFilter `json:"gpu_name,omitempty"`
	NumGPUs     *intFilter     `json:"num_gpus,omitempty"`
	GPURAM      *intFilter     `json:"gpu_ram,omitempty"`
	DPHTotal    *floatFilter   `json:"dph_total,omitempty"`
	Reliability *floatFilter   `json:"reliability,omitempty"`
	DiskSpace   *floatFilter   `json:"disk_space,omitempty"`
	CPUCores    *intFilter     `json:"cpu_cores,omitempty"`
	CPURAM      *floatFilter   `json:"cpu_ram,omitempty"`
	Geolocation *stringsFilter `json:"geolocation,omitempty"`
}

type searchResponse struct {
	Offers []wireOffer `json:"offers"`
}

// wireOffer decodes the subset of an offer LARRI depends on.
//
// Fields that feed a spend decision are pointers so that "absent" is
// distinguishable from "zero" — a missing dph_total must not silently become a
// free machine.
type wireOffer struct {
	ID            *int64   `json:"id"`
	AskContractID *int64   `json:"ask_contract_id"`
	GPUName       *string  `json:"gpu_name"`
	NumGPUs       *int     `json:"num_gpus"`
	GPURAM        *float64 `json:"gpu_ram"`
	DPHTotal      *float64 `json:"dph_total"`
	Reliability   *float64 `json:"reliability"`
	CudaMaxGood   *float64 `json:"cuda_max_good"`
	InetDown      *float64 `json:"inet_down"`
	DiskSpace     *float64 `json:"disk_space"`
	CPUCores      *int     `json:"cpu_cores"`
	CPURAM        *float64 `json:"cpu_ram"`
	Geolocation   *string  `json:"geolocation"`
	MachineID     *int64   `json:"machine_id"`
	Verified      *bool    `json:"verified"`
	Verification  *string  `json:"verification"`
	Rentable      *bool    `json:"rentable"`
	IsBid         *bool    `json:"is_bid"`
	DriverVersion *string  `json:"driver_version"`
	ComputeCap    *float64 `json:"compute_cap"`
}

// normalise converts a wire offer into LARRI's vocabulary, refusing anything
// whose spend-relevant fields are missing or implausible (R-02).
func (o wireOffer) normalise() (core.Offer, error) {
	miss := func(f string) (core.Offer, error) {
		return core.Offer{}, &ShapeDrift{Op: "offer", Err: fmt.Errorf("field %s absent", f)}
	}
	switch {
	case o.DPHTotal == nil:
		return miss("dph_total")
	case o.GPUName == nil:
		return miss("gpu_name")
	case o.NumGPUs == nil:
		return miss("num_gpus")
	case o.GPURAM == nil:
		return miss("gpu_ram")
	}
	id := int64(0)
	if o.AskContractID != nil {
		id = *o.AskContractID
	} else if o.ID != nil {
		id = *o.ID
	} else {
		return miss("id")
	}

	vramGB := int(*o.GPURAM / mbPerGB)
	// A GPU with under 1 GB or over 1 TB of VRAM does not exist. If this
	// trips, the unit changed, and continuing would size against a fiction.
	if vramGB < 1 || vramGB > 1024 {
		return core.Offer{}, &ShapeDrift{Op: "offer", Err: fmt.Errorf(
			"gpu_ram %.0f normalises to %d GB: unit changed", *o.GPURAM, vramGB)}
	}
	if *o.DPHTotal < 0 {
		return core.Offer{}, &ShapeDrift{Op: "offer", Err: fmt.Errorf(
			"dph_total %.4f is negative", *o.DPHTotal)}
	}

	out := core.Offer{
		Provider:     "vastai",
		OfferID:      strconv.FormatInt(id, 10),
		GPUModel:     *o.GPUName,
		GPUCount:     *o.NumGPUs,
		VRAMPerGPUGB: vramGB,
		PriceHr:      *o.DPHTotal,
	}
	if o.CPUCores != nil {
		out.CPUCores = *o.CPUCores
	}
	if o.CPURAM != nil {
		out.RAMGB = int(*o.CPURAM / mbPerGB)
	}
	if o.DiskSpace != nil {
		out.DiskGB = int(*o.DiskSpace)
	}
	if o.Geolocation != nil {
		out.Region = *o.Geolocation
	}
	if o.InetDown != nil {
		out.NetDownMbps = *o.InetDown
	}
	if o.CudaMaxGood != nil {
		out.CUDAVersion = strconv.FormatFloat(*o.CudaMaxGood, 'f', -1, 64)
	}
	if o.DriverVersion != nil {
		out.DriverVersion = *o.DriverVersion
	}
	if o.ComputeCap != nil {
		out.ComputeCapability = int(*o.ComputeCap)
	}
	if o.MachineID != nil {
		out.MachineID = strconv.FormatInt(*o.MachineID, 10)
	}
	if o.IsBid != nil {
		out.Interruptible = *o.IsBid
	}
	// Vast reports reliability as a 0..1 fraction already; clamp rather than
	// trust, since a value outside the range would distort ranking silently.
	if o.Reliability != nil {
		out.Reliability = clamp01(*o.Reliability)
	}
	// "Verified" is Vast's datacentre-certified tier, which is the closest
	// thing the marketplace offers to a trust signal (§15.5.1).
	// The bool is not in the payload — every offer in a 400-offer sample had
	// it null while "verification" carried verified / unverified /
	// deverified. Reading the bool made Certified universally false, so
	// CertifiedOnly would have excluded the whole market: a control that
	// silently rejects everything is worse than no control, because it looks
	// like it is working.
	if o.Verification != nil {
		out.Verification = strings.ToLower(strings.TrimSpace(*o.Verification))
	}
	switch {
	case out.Verification != "":
		out.Certified = out.Verification == "verified"
	case o.Verified != nil:
		out.Certified = *o.Verified
	}
	return out, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---- create ---------------------------------------------------------------

type createRequest struct {
	Image       string  `json:"image"`
	Disk        float64 `json:"disk,omitempty"`
	Label       string  `json:"label,omitempty"`
	RunType     string  `json:"runtype,omitempty"`
	TargetState string  `json:"target_state,omitempty"`
	OnStart     string  `json:"onstart,omitempty"`
	Env         string  `json:"env,omitempty"`
	Price       float64 `json:"price,omitempty"` // bid price, interruptible only
}

type createResponse struct {
	Success     bool  `json:"success"`
	NewContract int64 `json:"new_contract"`
}

// ---- list -----------------------------------------------------------------

// getResponse is the single-instance read. Vast returns the instance under a
// singular key here, not the array the list route uses.
type getResponse struct {
	Instances *wireInstance `json:"instances"`
}

type listResponse struct {
	Success   bool           `json:"success"`
	Instances []wireInstance `json:"instances"`
	NextToken *string        `json:"next_token"`
}

type wireInstance struct {
	ID           *int64         `json:"id"`
	ActualStatus *string        `json:"actual_status"`
	CurState     *string        `json:"cur_state"`
	StatusMsg    *string        `json:"status_msg"`
	IntendedStat *string        `json:"intended_status"`
	NextState    *string        `json:"next_state"`
	DiskUsage    *float64       `json:"disk_usage"`
	Label        *string        `json:"label"`
	SSHHost      *string        `json:"ssh_host"`
	SSHPort      *int           `json:"ssh_port"`
	PublicIP     *string        `json:"public_ipaddr"`
	MachineID    *int64         `json:"machine_id"`
	DPHTotal     *float64       `json:"dph_total"`
	StorageCost  *float64       `json:"storage_cost"`
	StartDate    *float64       `json:"start_date"`
	Ports        map[string]any `json:"ports"`
	AskContract  *int64         `json:"ask_contract_id"`
}

// normalise converts a wire instance into LARRI's vocabulary.
func (i wireInstance) normalise() (core.Instance, error) {
	if i.ID == nil {
		return core.Instance{}, &ShapeDrift{Op: "instance", Err: fmt.Errorf("field id absent")}
	}
	out := core.Instance{
		Provider:   "vastai",
		InstanceID: strconv.FormatInt(*i.ID, 10),
	}
	if i.AskContract != nil {
		out.OfferID = strconv.FormatInt(*i.AskContract, 10)
	}
	if i.DPHTotal != nil {
		out.PriceHr = *i.DPHTotal
	}
	if i.StorageCost != nil {
		out.StorageHr = *i.StorageCost
	}
	if i.SSHHost != nil {
		out.SSHHost = *i.SSHHost
	}
	if i.SSHPort != nil {
		out.SSHPort = *i.SSHPort
	}
	if i.PublicIP != nil {
		out.PublicIP = *i.PublicIP
	}
	if i.StartDate != nil && *i.StartDate > 0 {
		sec := int64(*i.StartDate)
		out.CreatedAt = time.Unix(sec, 0).UTC()
	}
	out.Running = isRunning(i.ActualStatus)
	// Vast reports two different things and both matter. actual_status is the
	// container — what decides whether sshd can be reached. cur_state is the
	// contract, which reads "running" the moment billing starts, long before
	// the container exists. Carrying both means a caller can tell "not ready"
	// from "not reporting", which are different problems.
	switch {
	case i.ActualStatus != nil && *i.ActualStatus != "":
		out.Status = *i.ActualStatus
	case i.CurState != nil && *i.CurState != "":
		out.Status = "contract " + *i.CurState
	}
	if i.StatusMsg != nil {
		out.StatusMsg = strings.TrimSpace(*i.StatusMsg)
	}
	// intended_status is the contract's own goal. It flips to "stopped" when
	// Vast gives up on a container, while actual_status can still read
	// "created" or "loading" — the pair is what distinguishes a slow boot
	// from an abandoned one. next_state carries the same news when
	// intended_status is absent.
	// Vast reports -1 for "not measured", which is not zero bytes.
	if i.DiskUsage != nil && *i.DiskUsage >= 0 {
		out.DiskUsedGB = *i.DiskUsage
	}
	switch {
	case i.IntendedStat != nil && *i.IntendedStat != "":
		out.Intent = strings.TrimSpace(*i.IntendedStat)
	case i.NextState != nil && *i.NextState != "":
		out.Intent = strings.TrimSpace(*i.NextState)
	}
	if i.Label != nil && *i.Label != "" {
		if l, ok := core.DecodeLabel(*i.Label); ok {
			// Both the parsed ID and the raw marker: the ID is what
			// reconciliation compares, the raw is what lets a future version
			// read a label this one did not fully understand.
			out.Labels = map[string]string{
				core.LabelKey:    l.RigID,
				core.LabelRawKey: *i.Label,
			}
		}
	}
	return out, nil
}

// isRunning maps Vast's actual_status onto the one bit LARRI needs.
//
// Everything that is not affirmatively running is treated as not running,
// including "exited", "offline", and an absent status. That direction is
// deliberate: a stopped container still bills for storage, so guessing
// "running" would understate the state, while guessing "not running" only
// causes a re-query.
func isRunning(status *string) bool {
	return status != nil && strings.EqualFold(*status, "running")
}
