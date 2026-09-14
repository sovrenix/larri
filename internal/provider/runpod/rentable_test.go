// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

// catalogueTypes are the ids in catalogueJSON, all of them creatable.
var catalogueTypes = []string{
	"NVIDIA GeForce RTX 4090", "NVIDIA A100 80GB PCIe", "NVIDIA RTX A5000",
	"NVIDIA GeForce RTX 3070", "AMD Instinct MI300X OAM",
}

// schemaJSON is the part of RunPod's REST schema that says which GPU types a
// create accepts.
func schemaJSON(ids ...string) string {
	enum, _ := json.Marshal(ids)
	return `{"openapi":"3.0.0","components":{"schemas":{"PodCreateInput":{"properties":{` +
		`"gpuTypeIds":{"type":"array","items":{"type":"string","enum":` + string(enum) + `}}}}}}}`
}

// A catalogue as it was read live: a server edition the create enum spells
// differently, and a MIG slice the create enum does carry.
const mismatchedCatalogue = `{"data":{"gpuTypes":[
 {"id":"NVIDIA RTX PRO 4500 Blackwell Server Edition","displayName":"RTX PRO 4500","memoryInGb":32,
  "secureCloud":true,"maxGpuCount":8,
  "price1":{"uninterruptablePrice":0.34,"stockStatus":"High"}},
 {"id":"NVIDIA B300 SXM6 AC MIG 1g.34gb","displayName":"B300 MIG","memoryInGb":34,
  "secureCloud":true,"maxGpuCount":1,
  "price1":{"uninterruptablePrice":0.90,"stockStatus":"High"}},
 {"id":"NVIDIA GeForce RTX 4090","displayName":"RTX 4090","memoryInGb":24,
  "secureCloud":true,"maxGpuCount":1,
  "price1":{"uninterruptablePrice":0.69,"stockStatus":"High"}}
]}}`

func mismatchedProvider(t *testing.T, schema string, schemaReads *int) *Provider {
	return testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			w.Write([]byte(mismatchedCatalogue))
		case r.URL.Path == schemaPath && schema != "":
			if schemaReads != nil {
				*schemaReads++
			}
			w.Write([]byte(schema))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// The catalogue listed "NVIDIA RTX PRO 4500 Blackwell Server Edition" priced
// and in stock; POST /pods takes "NVIDIA RTX PRO 4500 Blackwell". Nothing in
// the name says so, and every run it was cheapest spent a create attempt
// finding out. The create call's own schema does say so.
func TestOnlyTypesTheCreateSchemaAcceptsAreOffered(t *testing.T) {
	schema := schemaJSON("NVIDIA RTX PRO 4500 Blackwell", "NVIDIA B300 SXM6 AC MIG 1g.34gb",
		"NVIDIA GeForce RTX 4090")
	var noticed string
	p := mismatchedProvider(t, schema, nil)
	p.OnNotice = func(m string) { noticed += m + "; " }

	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range offers {
		got[gpuTypeID(o.OfferID)] = true
	}
	if got["NVIDIA RTX PRO 4500 Blackwell Server Edition"] {
		t.Error("offered a type POST /pods does not accept")
	}
	// A MIG slice the create call lists is rentable, whatever its name
	// suggests; the name rule would have dropped it.
	if !got["NVIDIA B300 SXM6 AC MIG 1g.34gb"] || !got["NVIDIA GeForce RTX 4090"] {
		t.Errorf("dropped types the create schema accepts: offered %v", got)
	}
	if !strings.Contains(noticed, "not a rentable type") {
		t.Errorf("the operator was not told a type was skipped: %q", noticed)
	}
}

// A schema that cannot be read must not empty the market (§4a): the known
// non-rentable types are still dropped and everything else is offered, and
// the operator hears that the check was weaker than usual.
func TestAnUnreadableSchemaFallsBackToTheKnownTypes(t *testing.T) {
	var noticed string
	p := mismatchedProvider(t, "", nil)
	p.OnNotice = func(m string) { noticed += m + "; " }

	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) == 0 {
		t.Fatal("an unreadable schema emptied the market")
	}
	if !strings.Contains(noticed, "could not read which gpu types can be rented") {
		t.Errorf("fallback was silent: %q", noticed)
	}
}

// The schema is 150 KB and does not change inside a run.
func TestTheCreateSchemaIsReadOncePerProcess(t *testing.T) {
	reads := 0
	p := mismatchedProvider(t, schemaJSON("NVIDIA GeForce RTX 4090"), &reads)
	for i := 0; i < 3; i++ {
		if _, err := p.Search(context.Background(), core.Criteria{}); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Errorf("schema read %d times across three searches", reads)
	}
}

// costPerHr is the GPU alone — a live A40 pod on 220 GB of disk reported
// 0.49, the catalogue rate exactly — and the disks bill beside it.
func TestAPodsRateIncludesItsDisks(t *testing.T) {
	cost := 0.49
	inst, err := pod{ID: "p", DesiredStatus: "RUNNING", CostPerHr: &cost,
		ContainerDiskInGb: 20, VolumeInGb: 200}.normalise()
	if err != nil {
		t.Fatal(err)
	}
	if want := 0.49 + 220*0.10/720; math.Abs(inst.PriceHr-want) > 1e-9 {
		t.Errorf("PriceHr = %.5f, want %.5f: the GPU and 220 GB at $0.10/GB/month", inst.PriceHr, want)
	}
	if want := 200 * 0.20 / 720; math.Abs(inst.StorageHr-want) > 1e-9 {
		t.Errorf("StorageHr = %.5f, want %.5f: a stopped pod bills its volume", inst.StorageHr, want)
	}
}

// The quote is for the disk being rented, so a ceiling and a ranking see the
// rate the pod will bill.
func TestSearchQuotesTheDiskBeingRented(t *testing.T) {
	p := mismatchedProvider(t, schemaJSON("NVIDIA GeForce RTX 4090"), nil)
	offers, err := p.Search(context.Background(), core.Criteria{DiskGB: 200})
	if err != nil || len(offers) != 1 {
		t.Fatalf("offers %v, err %v", offers, err)
	}
	if want := 0.69 + 220*0.10/720; math.Abs(offers[0].PriceHr-want) > 1e-9 {
		t.Errorf("quoted %.5f, want %.5f with 200 GB of volume and the 20 GB container disk",
			offers[0].PriceHr, want)
	}
}

// A CUDA process sees one MIG device however many a pod holds, so a MIG type
// is one slice. RunPod prices PRO 6000 MIG 1g.24gb at up to 22 slices; offered
// that way it would read as 528 GB for a split model the engine loads into
// 24 GB. Today that type is not in the create schema — but RunPod does list
// a MIG type there, so the schema is not what keeps this out.
func TestAMIGTypeIsOfferedAsOneSlice(t *testing.T) {
	const mig = "NVIDIA B300 SXM6 AC MIG 1g.34gb"
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			w.Write([]byte(`{"data":{"gpuTypes":[
			 {"id":"` + mig + `","displayName":"B300 MIG 34GB","memoryInGb":34,"secureCloud":true,"maxGpuCount":32,
			  "price1":{"uninterruptablePrice":0.9,"stockStatus":"High"},
			  "price2":{"uninterruptablePrice":1.8,"stockStatus":"High"},
			  "price22":{"uninterruptablePrice":19.8,"stockStatus":"High"}},
			 {"id":"NVIDIA A40","displayName":"A40","memoryInGb":48,"secureCloud":true,"maxGpuCount":4,
			  "price1":{"uninterruptablePrice":0.49,"stockStatus":"High"},
			  "price2":{"uninterruptablePrice":0.98,"stockStatus":"High"}}
			]}}`))
		case r.URL.Path == schemaPath:
			w.Write([]byte(schemaJSON(mig, "NVIDIA A40")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	var migOffers []int
	for _, o := range offers {
		if gpuTypeID(o.OfferID) == mig {
			migOffers = append(migOffers, o.GPUCount)
		}
	}
	if len(migOffers) != 1 || migOffers[0] != 1 {
		t.Errorf("MIG type offered at %v slices; want one", migOffers)
	}
	// Its placeable count does not widen the sizes priced for every type.
	if counts := p.priceCounts(context.Background(), p.rentableCheck(context.Background())); counts[len(counts)-1] != 4 {
		t.Errorf("pricing up to %d cards; the only non-MIG type places 4", counts[len(counts)-1])
	}
}

// RunPod lists AMD's Instinct cards beside NVIDIA's, and says whose they are
// only in the name.
func TestOffersSayWhoMadeTheCard(t *testing.T) {
	for id, want := range map[string]string{
		"AMD Instinct MI300X OAM": "amd", "NVIDIA A40": "nvidia", "Tesla V100-SXM2-16GB": "nvidia",
		"NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb": "nvidia",
	} {
		if got := vendorOf(id); got != want {
			t.Errorf("vendorOf(%q) = %q, want %q", id, got, want)
		}
	}
}
