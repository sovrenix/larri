// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/secret"
)

// The catalogue as RunPod actually returns it, including the entries with no
// price — about ten at any time, hardware listed but not placeable.
const catalogueJSON = `{"data":{"gpuTypes":[
 {"id":"NVIDIA GeForce RTX 4090","displayName":"RTX 4090","memoryInGb":24,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "lowestPrice":{"minimumBidPrice":0.34,"uninterruptablePrice":0.69,"stockStatus":"Medium"}},
 {"id":"NVIDIA A100 80GB PCIe","displayName":"A100 PCIe","memoryInGb":80,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "lowestPrice":{"minimumBidPrice":1.19,"uninterruptablePrice":1.19,"stockStatus":"High"}},
 {"id":"NVIDIA RTX A5000","displayName":"RTX A5000","memoryInGb":24,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "lowestPrice":{"minimumBidPrice":null,"uninterruptablePrice":null,"stockStatus":null}},
 {"id":"NVIDIA GeForce RTX 3070","displayName":"RTX 3070","memoryInGb":8,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "lowestPrice":{"minimumBidPrice":0.13,"uninterruptablePrice":0.13,"stockStatus":"Low"}},
 {"id":"AMD Instinct MI300X OAM","displayName":"MI300X","memoryInGb":192,
  "secureCloud":true,"communityCloud":false,"maxGpuCount":8,
  "lowestPrice":{"minimumBidPrice":0.5,"uninterruptablePrice":0.5,"stockStatus":"High"}}
]}}`

func testProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(secret.New("test-key"))
	c.RESTURL = srv.URL
	c.GraphQLURL = srv.URL + "/graphql"
	c.HTTP = srv.Client()
	return NewWithClient(c)
}

func catalogueOnly(t *testing.T) *Provider {
	return testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			w.Write([]byte(catalogueJSON))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

// An offer with no price cannot be ranked, no ceiling applies to it, and cost
// accounting cannot follow it. "No price" means unavailable, not free.
func TestUnpricedGpuTypesAreDropped(t *testing.T) {
	var noticed string
	p := catalogueOnly(t)
	p.OnNotice = func(m string) { noticed += m + "; " }

	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offers {
		if o.PriceHr <= 0 {
			t.Errorf("offer %s came back at %v", o.OfferID, o.PriceHr)
		}
		if strings.Contains(o.GPUModel, "A5000") {
			t.Error("an unpriced type was offered as rentable")
		}
	}
	if !strings.Contains(noticed, "skipped") {
		t.Errorf("the operator was not told anything was skipped: %q", noticed)
	}
}

// The id must survive normalisation, because it is what POST /pods accepts.
// Normalising to the pretty display name would produce offers that cannot be
// bought.
func TestOfferIDIsThePurchasableGpuTypeID(t *testing.T) {
	offers, err := catalogueOnly(t).Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range offers {
		if o.OfferID == "NVIDIA GeForce RTX 4090" {
			found = true
			if o.GPUModel != "RTX 4090" {
				t.Errorf("display name = %q", o.GPUModel)
			}
		}
	}
	if !found {
		t.Error("the purchasable id did not survive into the offer")
	}
}

// A catalogue provider places the pod itself, so there is no host to name and
// none to score. Both must stay empty: a MachineID would make one failed pod
// exclude every machine of that type, and a reliability score would be
// invented (§5.4).
func TestCatalogueOffersNameNoHostAndScoreNone(t *testing.T) {
	offers, err := catalogueOnly(t).Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offers {
		if o.MachineID != "" {
			t.Errorf("offer %s claims machine %q; runpod does not name hosts", o.OfferID, o.MachineID)
		}
		if o.HasReliability() {
			t.Errorf("offer %s reports reliability %v, which runpod does not publish",
				o.OfferID, o.Reliability)
		}
	}
}

// The zero value of Interruptible forbids (Q-04). A spot price must never be
// offered to an operator who did not ask for one — it is cheaper because it
// can be taken away.
func TestSpotPricingIsOptIn(t *testing.T) {
	p := catalogueOnly(t)
	onDemand, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range onDemand {
		if o.Interruptible {
			t.Errorf("offer %s is interruptible without being asked for", o.OfferID)
		}
		if o.OfferID == "NVIDIA GeForce RTX 4090" && o.PriceHr != 0.69 {
			t.Errorf("on-demand price = %v, want the uninterruptable 0.69", o.PriceHr)
		}
	}
	spot, err := p.Search(context.Background(), core.Criteria{Interruptible: core.Allow})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range spot {
		if o.OfferID == "NVIDIA GeForce RTX 4090" && o.PriceHr != 0.34 {
			t.Errorf("spot price = %v, want the bid 0.34", o.PriceHr)
		}
	}
}

// The label is the only marker RunPod can carry — it has no tags — so a pod
// without one is a pod no reconciler could attribute.
func TestCreateRefusesAnUnlabelledPod(t *testing.T) {
	p := catalogueOnly(t)
	_, err := p.Create(context.Background(), core.Offer{OfferID: "x"}, provider.CreateSpec{})
	if err == nil {
		t.Fatal("created a pod with no label")
	}
	if !strings.Contains(err.Error(), "attributable") {
		t.Errorf("the error should say why: %v", err)
	}
}

// RunPod has no ssh key field; its images read PUBLIC_KEY at start-up.
func TestCreateSendsTheKeyAsPublicKeyEnv(t *testing.T) {
	var got createRequest
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			w.Write([]byte(catalogueJSON))
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"pod1","desiredStatus":"RUNNING","name":"larri:01X"}`))
	})
	_, err := p.Create(context.Background(),
		core.Offer{OfferID: "NVIDIA GeForce RTX 4090", GPUCount: 1, PriceHr: 0.69},
		provider.CreateSpec{Image: "img", Label: "larri:01X", DiskGB: 60,
			SSHPublicKey: "ssh-ed25519 AAAA larri"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Env["PUBLIC_KEY"] != "ssh-ed25519 AAAA larri" {
		t.Errorf("PUBLIC_KEY = %q", got.Env["PUBLIC_KEY"])
	}
	if got.Name != "larri:01X" {
		t.Errorf("name = %q; the label is the only marker runpod can carry", got.Name)
	}
	var hasSSH bool
	for _, p := range got.Ports {
		if p == "22/tcp" {
			hasSSH = true
		}
	}
	if !hasSSH {
		t.Error("no ssh port mapped; the tunnel is the only path in")
	}
}

// dockerStartCmd replaces the image's entrypoint rather than running beside
// it. A bare onstart would therefore replace whatever starts sshd, and the pod
// would come up unreachable.
func TestOnStartDoesNotStrandThePod(t *testing.T) {
	var got createRequest
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			w.Write([]byte(catalogueJSON))
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"pod1","desiredStatus":"RUNNING"}`))
	})
	_, err := p.Create(context.Background(), core.Offer{OfferID: "x", GPUCount: 1},
		provider.CreateSpec{Image: "img", Label: "larri:01X", OnStart: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got.DockerStartCmd, " ")
	if !strings.Contains(joined, "echo hi") {
		t.Errorf("the start command was dropped: %v", got.DockerStartCmd)
	}
	if !strings.Contains(joined, "sleep infinity") {
		t.Error("the start command would exit and take the pod with it")
	}
}

// EXITED is a pod that still exists and still bills for its volume. Reading it
// as absent is the mistake that journals a billing resource as destroyed.
func TestExitedPodIsPresentAndNotRunning(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"pod1","desiredStatus":"EXITED","costPerHr":0.69,
			"name":"larri:01X","volumeInGb":60}`))
	})
	inst, err := p.Get(context.Background(), "pod1")
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		t.Fatal("an EXITED pod read as absent; it still bills for its volume")
	}
	if inst.Running {
		t.Error("an EXITED pod reports Running")
	}
}

// SSH reaches the pod through a mapped port on the public address.
func TestSSHEndpointComesFromThePortMapping(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"pod1","desiredStatus":"RUNNING","publicIp":"1.2.3.4",
			"portMappings":{"22":10341,"8000":10342}}`))
	})
	inst, err := p.Get(context.Background(), "pod1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.SSHHost != "1.2.3.4" || inst.SSHPort != 10341 {
		t.Errorf("ssh endpoint = %s:%d, want 1.2.3.4:10341", inst.SSHHost, inst.SSHPort)
	}
}

// Weights go on the volume, which survives a stop; the container disk is
// wiped. Getting this backwards means re-downloading tens of gigabytes after
// an interruption, which costs more than the storage.
func TestWeightsGetThePersistentDisk(t *testing.T) {
	if volumeDisk(60) != 60 {
		t.Errorf("volume = %d, want the requested 60", volumeDisk(60))
	}
	if containerDisk(60) >= 60 {
		t.Error("the whole allowance went to the disk that gets wiped")
	}
	if volumeDisk(5) < containerDiskGB {
		t.Error("a tiny request left no room for the weights")
	}
}

// A provider's error body is untrusted text that reaches logs, journal entries
// and MCP results.
func TestErrorBodiesAreRedacted(t *testing.T) {
	c := NewClient(secret.New("rp-abcdefghijklmnopqrstuvwxyz"))
	got := c.redactBody([]byte(`{"error":"bad","api_key":"rp-abcdefghijklmnopqrstuvwxyz"}`))
	if strings.Contains(got, "rp-abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("the key survived: %s", got)
	}
	if !strings.Contains(got, "bad") {
		t.Errorf("redaction destroyed the diagnostic: %s", got)
	}
}

// GraphQL reports failure inside a 200, so the status code is not the check.
func TestGraphQLErrorsAreNotHiddenBehindA200(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"errors":[{"message":"rate limited"}]}`))
	})
	if _, err := p.Search(context.Background(), core.Criteria{}); err == nil {
		t.Fatal("a graphql error inside a 200 was read as success")
	}
}

// vLLM needs Volta or newer, and the check runs before spending. A wrong
// answer costs a rental.
func TestComputeCapabilityCoversWhatWeCanPlace(t *testing.T) {
	for _, c := range []struct {
		id   string
		want int
	}{
		{"NVIDIA H100 80GB HBM3", 900},
		{"NVIDIA A100 80GB PCIe", 800},
		{"NVIDIA GeForce RTX 4090", 890},
		{"NVIDIA RTX A5000", 860},
		{"Tesla V100-SXM2-16GB", 700},
		{"AMD Instinct MI300X OAM", 0}, // not a CUDA capability at all
		{"Something Unheard Of", 0},    // unknown, not unsuitable
	} {
		if got := computeCapability(c.id); got != c.want {
			t.Errorf("computeCapability(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}

// The catalogue and the create enum are not kept in sync: RunPod advertises a
// literal "unknown" type and several MIG partitions the create call rejects.
// Three are priced, and one of those ($0.50/hr for 32 GB) would rank well
// enough to be chosen.
func TestUnpurchasableTypesAreNotOffered(t *testing.T) {
	for _, id := range []string{
		"unknown",
		"NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb",
	} {
		if purchasable(id) {
			t.Errorf("%q was offered as rentable; POST /pods does not accept it", id)
		}
	}
	for _, id := range []string{
		"NVIDIA GeForce RTX 4090", "NVIDIA A100 80GB PCIe", "Tesla V100-SXM2-16GB",
		"AMD Instinct MI300X OAM", // "MI300X" is a card, not a MIG partition
	} {
		if !purchasable(id) {
			t.Errorf("%q was dropped but is a real rentable type", id)
		}
	}
}

// A GPU type that cannot be placed must fall back to the next offer, not end
// the run. The default for a 400 is model-attributable — "the next host fails
// identically" — which is right for a bad image and wrong here: the next
// *type* may be perfectly available.
func TestUnplaceableGpuTypeFallsBackInsteadOfAborting(t *testing.T) {
	for _, msg := range []string{
		"http 400: invalid gpuTypeIds",
		"http 400: there are no longer any instances available",
		"http 500: capacity unavailable in this data center",
	} {
		got := createClass(errs.Newf(errs.ClassModelFailure, "runpod.Create", "%s", msg))
		if errs.ClassOf(got) != errs.ClassHostFailure {
			t.Errorf("%q classified %s; the fallback would not engage",
				msg, errs.ClassOf(got))
		}
	}
	// And a genuinely model-attributable failure must stay that way, or the
	// fallback burns six hosts on a bad image.
	bad := errs.Newf(errs.ClassModelFailure, "runpod.Create", "http 400: image not found")
	if errs.ClassOf(createClass(bad)) != errs.ClassModelFailure {
		t.Error("a bad image was reclassified as a host failure; the fallback would " +
			"retry it on every host")
	}
}

// Stock status predicts whether a create succeeds, measured against the live
// API: an A40 (High) and an RTX 4090 (Medium) both created on request, while
// an RTX 3070 (Low) was refused with "there are no instances currently
// available".
//
// So a Low-stock type is not offered. It would otherwise be *chosen* — the
// RTX 3070 is the cheapest thing RunPod lists — and the operator would watch
// the cheapest option fail every time.
func TestOutOfStockTypesAreNotOffered(t *testing.T) {
	var noticed string
	p := catalogueOnly(t)
	p.OnNotice = func(m string) { noticed += m + "; " }

	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offers {
		if strings.Contains(o.GPUModel, "3070") {
			t.Error("a Low-stock type was offered; it is the cheapest listed and " +
				"would be selected, then refused at create")
		}
	}
	if !strings.Contains(noticed, "out of stock") {
		t.Errorf("the operator was not told why it vanished: %q", noticed)
	}
	// High and Medium both stay.
	var kinds []string
	for _, o := range offers {
		kinds = append(kinds, o.GPUModel)
	}
	if len(offers) != 3 {
		t.Errorf("kept %v; want the High and Medium stock types", kinds)
	}
}

func TestInStockAcceptsOnlyWhatCreates(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		in   *string
		want bool
	}{
		{str("High"), true}, {str("Medium"), true}, {str("medium"), true},
		{str("Low"), false}, {nil, false}, {str(""), false},
	} {
		if got := inStock(c.in); got != c.want {
			label := "nil"
			if c.in != nil {
				label = *c.in
			}
			t.Errorf("inStock(%q) = %v, want %v", label, got, c.want)
		}
	}
}

// A catalogue that lists nothing rentable must say so as an unsatisfiable
// criteria error, not return an empty slice that reads as a working search
// with no matches.
func TestNothingRentableIsAnError(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"gpuTypes":[{"id":"unknown","displayName":"x",
		  "memoryInGb":0,"lowestPrice":{"uninterruptablePrice":null,"stockStatus":null}}]}}`))
	})
	if _, err := p.Search(context.Background(), core.Criteria{}); err == nil {
		t.Fatal("an empty catalogue read as a successful search")
	}
}
