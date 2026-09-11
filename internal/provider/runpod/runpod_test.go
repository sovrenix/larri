// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/secret"
)

// The catalogue as RunPod actually returns it, including the entries with no
// price — about ten at any time, hardware listed but not placeable.
//
// One entry per GPU count, because that is the shape of the query: RunPod
// prices lowestPrice(gpuCount: N) separately for each N and reports stock
// separately too. The two disagree, which is why both are read — a live
// catalogue had H100 SXM at High for one card, Medium for two, Low for four,
// and the A5000 below is priced at two cards while unpriced at one, which is
// the case that would have made a whole type invisible.
const catalogueJSON = `{"data":{"gpuTypes":[
 {"id":"NVIDIA GeForce RTX 4090","displayName":"RTX 4090","memoryInGb":24,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":4,
  "price1":{"minimumBidPrice":0.34,"uninterruptablePrice":0.69,"stockStatus":"Medium"},
  "price2":{"minimumBidPrice":0.68,"uninterruptablePrice":1.38,"stockStatus":"Medium"},
  "price4":{"minimumBidPrice":1.36,"uninterruptablePrice":2.76,"stockStatus":"Low"},
  "price8":null},
 {"id":"NVIDIA A100 80GB PCIe","displayName":"A100 PCIe","memoryInGb":80,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "price1":{"minimumBidPrice":1.19,"uninterruptablePrice":1.19,"stockStatus":"High"},
  "price2":{"minimumBidPrice":2.38,"uninterruptablePrice":2.38,"stockStatus":"High"},
  "price4":{"minimumBidPrice":4.76,"uninterruptablePrice":4.76,"stockStatus":"Medium"},
  "price8":{"minimumBidPrice":9.52,"uninterruptablePrice":9.52,"stockStatus":"Low"}},
 {"id":"NVIDIA RTX A5000","displayName":"RTX A5000","memoryInGb":24,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "price1":{"minimumBidPrice":null,"uninterruptablePrice":null,"stockStatus":null},
  "price2":{"minimumBidPrice":null,"uninterruptablePrice":null,"stockStatus":null},
  "price4":{"minimumBidPrice":null,"uninterruptablePrice":null,"stockStatus":null},
  "price8":{"minimumBidPrice":null,"uninterruptablePrice":null,"stockStatus":null}},
 {"id":"NVIDIA GeForce RTX 3070","displayName":"RTX 3070","memoryInGb":8,
  "secureCloud":true,"communityCloud":true,"maxGpuCount":8,
  "price1":{"minimumBidPrice":0.13,"uninterruptablePrice":0.13,"stockStatus":"Low"},
  "price2":{"minimumBidPrice":0.26,"uninterruptablePrice":0.26,"stockStatus":"Low"},
  "price4":{"minimumBidPrice":0.52,"uninterruptablePrice":0.52,"stockStatus":"Low"},
  "price8":{"minimumBidPrice":1.04,"uninterruptablePrice":1.04,"stockStatus":"Low"}},
 {"id":"AMD Instinct MI300X OAM","displayName":"MI300X","memoryInGb":192,
  "secureCloud":true,"communityCloud":false,"maxGpuCount":8,
  "price1":{"minimumBidPrice":0.5,"uninterruptablePrice":0.5,"stockStatus":"High"},
  "price2":{"minimumBidPrice":1.0,"uninterruptablePrice":1.0,"stockStatus":"High"},
  "price4":{"minimumBidPrice":2.0,"uninterruptablePrice":2.0,"stockStatus":"High"},
  "price8":{"minimumBidPrice":4.0,"uninterruptablePrice":4.0,"stockStatus":"High"}}
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
//
// It now shares an offer id with the card count, because one catalogue entry
// is several purchases and the conformance suite requires offer ids to be
// unique. Both halves have to come back out: the type id is what the REST API
// takes, and the count is what decides how much VRAM was bought.
func TestOfferIDIsThePurchasableGpuTypeID(t *testing.T) {
	offers, err := catalogueOnly(t).Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range offers {
		if gpuTypeID(o.OfferID) == "NVIDIA GeForce RTX 4090" && o.GPUCount == 1 {
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

// A model too large for the biggest single card is the ordinary reason to
// want a multi-GPU host, and RunPod sells pods of up to eight. Pricing only
// the single-card size reported those models unsatisfiable on a provider that
// had the hardware — the whole of issue #2.
func TestMultiGPUOffersAreListedPerCount(t *testing.T) {
	offers, err := catalogueOnly(t).Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	byCount := map[int]core.Offer{}
	for _, o := range offers {
		if gpuTypeID(o.OfferID) == "NVIDIA A100 80GB PCIe" {
			byCount[o.GPUCount] = o
		}
	}
	// 1, 2 and 4 are in stock; 8 is Low and must not be offered.
	for _, n := range []int{1, 2, 4} {
		o, ok := byCount[n]
		if !ok {
			t.Fatalf("no %d-GPU offer for a type RunPod places up to 8 at a time", n)
		}
		if want := 80 * n; o.VRAMTotalGB() != want {
			t.Errorf("%d-GPU offer aggregates %dGB, want %dGB", n, o.VRAMTotalGB(), want)
		}
		if want := 1.19 * float64(n); o.PriceHr != want {
			t.Errorf("%d-GPU offer priced $%.2f, want $%.2f", n, o.PriceHr, want)
		}
	}
	if _, ok := byCount[8]; ok {
		t.Error("an eight-card size with Low stock was offered; stock is per count, " +
			"and a create at that size is refused")
	}
	// maxGpuCount is a real ceiling: the 4090 lists 4 and must not be offered
	// at 8, however the catalogue prices it.
	for _, o := range offers {
		if gpuTypeID(o.OfferID) == "NVIDIA GeForce RTX 4090" && o.GPUCount > 4 {
			t.Errorf("offered %d cards of a type RunPod caps at 4", o.GPUCount)
		}
	}
}

// A type unpriced at one card may still be priced at two. Reporting it as
// "no price" and skipping the whole entry is how a rentable size goes
// missing.
func TestACountThatIsPricedSurvivesOneThatIsNot(t *testing.T) {
	offers, err := catalogueOnly(t).Search(context.Background(), core.Criteria{
		MaxGPUCount: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offers {
		if o.GPUCount > 2 {
			t.Errorf("offer %s carries %d cards, above the ceiling of 2",
				o.OfferID, o.GPUCount)
		}
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
		if gpuTypeID(o.OfferID) == "NVIDIA GeForce RTX 4090" && o.GPUCount == 1 &&
			o.PriceHr != 0.69 {
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
	requireSpot, err := p.Search(context.Background(), core.Criteria{Interruptible: core.Require})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range requireSpot {
		if !o.Interruptible {
			t.Errorf("offer %s is on-demand under interruptible=require", o.OfferID)
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
	// High and Medium both stay. Counted by type rather than by offer: one
	// type is now several offers, one per card count it is in stock at.
	kinds := map[string]bool{}
	for _, o := range offers {
		kinds[o.GPUModel] = true
	}
	if len(kinds) != 3 {
		t.Errorf("kept %v; want the High and Medium stock types", kinds)
	}
	// And stock is per count, not per type: the 4090 is Low at four cards
	// while Medium at one and two, so only the first two sizes survive.
	var sizes []int
	for _, o := range offers {
		if o.GPUModel == "RTX 4090" {
			sizes = append(sizes, o.GPUCount)
		}
	}
	sort.Ints(sizes)
	if len(sizes) != 2 || sizes[0] != 1 || sizes[1] != 2 {
		t.Errorf("4090 offered at %v cards; want 1 and 2, the sizes it is in stock at", sizes)
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
		  "memoryInGb":0,"price1":{"uninterruptablePrice":null,"stockStatus":null}}]}}`))
	})
	if _, err := p.Search(context.Background(), core.Criteria{}); err == nil {
		t.Fatal("an empty catalogue read as a successful search")
	}
}

// RunPod supplies no usable SSH of its own: its terminal access is a proxy
// with no port forwarding, which is the one thing LARRI's tunnel is. So the
// pod must run a real sshd, and upstream engine images carry none —
// vllm/vllm-openai:latest has no sshd binary at all. The adapter installs one.
func TestStartCommandInstallsSSHBecauseRunpodDoesNot(t *testing.T) {
	got := startScript("")
	for _, want := range []string{"openssh-server", "authorized_keys", "$PUBLIC_KEY"} {
		if !strings.Contains(got, want) {
			t.Errorf("start command missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "service ssh start") && !strings.Contains(got, "sshd") {
		t.Errorf("nothing starts the daemon:\n%s", got)
	}
	// When the start command exits, the pod does.
	if !strings.HasSuffix(strings.TrimSpace(got), "sleep infinity") {
		t.Errorf("the pod would die as soon as ssh was ready:\n%s", got)
	}
}

// LARRI's own key installation runs after sshd exists, and its failure must
// not strand the pod — the key is already installed by then.
func TestStartCommandChainsOnStartWithoutStrandingThePod(t *testing.T) {
	got := startScript("echo larri-onstart")
	if !strings.Contains(got, "echo larri-onstart") {
		t.Errorf("onstart was dropped:\n%s", got)
	}
	if !strings.Contains(got, "|| true") {
		t.Error("a failing onstart would kill the pod after sshd was already up")
	}
	if strings.Index(got, "openssh-server") > strings.Index(got, "echo larri-onstart") {
		t.Error("onstart runs before sshd is installed")
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "sleep infinity") {
		t.Errorf("no keepalive after onstart:\n%s", got)
	}
}

// Without includeMachine, GET /pods/{id} omits publicIp and portMappings
// while GET /pods returns them for the same pod. Measured live: five
// consecutive single-pod reads showed no address, and LARRI waited for an
// endpoint the provider was already publishing on the other route.
func TestGetAsksForTheNetworkingFields(t *testing.T) {
	var path string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		w.Write([]byte(`{"id":"pod1","desiredStatus":"RUNNING","publicIp":"1.2.3.4",
			"portMappings":{"22":40068}}`))
	})
	inst, err := p.Get(context.Background(), "pod1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "includeMachine=true") {
		t.Errorf("Get requested %q; without includeMachine the address is omitted "+
			"and the rig never becomes reachable", path)
	}
	if inst.SSHPort != 40068 {
		t.Errorf("ssh port = %d", inst.SSHPort)
	}
}

// dockerStartCmd overrides CMD, not ENTRYPOINT. Setting only the command
// leaves the image's entrypoint in place and hands it the script as
// arguments — which on a vLLM image produced "vllm serve: error: argument
// --compilation-config", the engine rejecting a shell script as a flag. The
// pod died on repeat and sshd never ran.
func TestCreateOverridesTheEntrypointNotJustTheCommand(t *testing.T) {
	var got createRequest
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			w.Write([]byte(catalogueJSON))
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"pod1","desiredStatus":"RUNNING"}`))
	})
	if _, err := p.Create(context.Background(),
		core.Offer{OfferID: "NVIDIA GeForce RTX 4090", GPUCount: 1},
		provider.CreateSpec{Image: "vllm/vllm-openai:latest", Label: "larri:01X"}); err != nil {
		t.Fatal(err)
	}
	if len(got.DockerEntrypoint) == 0 {
		t.Fatal("the image's entrypoint was left in place; the script becomes its arguments")
	}
	if got.DockerEntrypoint[0] != "/bin/bash" {
		t.Errorf("entrypoint = %v", got.DockerEntrypoint)
	}
	if len(got.DockerStartCmd) != 1 || !strings.Contains(got.DockerStartCmd[0], "openssh-server") {
		t.Errorf("start command = %v", got.DockerStartCmd)
	}
}

// On RunPod the daemon is on a public IP — Vast fronts SSH with a proxy,
// RunPod maps port 22 straight onto a routable address. So the distribution
// default is not a detail; it is the configuration facing the internet.
func TestStartCommandHardensSSHBecauseItIsInternetFacing(t *testing.T) {
	got := startScript("")
	for _, want := range []string{
		"PasswordAuthentication no",
		"PermitEmptyPasswords no",
		"PermitRootLogin prohibit-password",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sshd not hardened, missing %q:\n%s", want, got)
		}
	}
	// Hardening has to land before the daemon starts, or the first window is
	// served with the defaults.
	if strings.Index(got, "PasswordAuthentication no") > strings.Index(got, "service ssh start") {
		t.Error("the daemon starts before it is hardened")
	}
}

// The disk the operator sizes is the volume at /workspace, while LARRI's data
// directory was on the 20 GB container disk — so a 111 GB download died at
// 20 GB whatever --disk said. The start script links LARRI's directory and
// vLLM's Hugging Face cache onto the volume, first, and without any way to
// stop the script before sshd starts.
func TestStartScriptPutsLarriDataOnTheVolume(t *testing.T) {
	s := startScript("")
	sshd := strings.Index(s, "service ssh start")
	if sshd < 0 {
		t.Fatal("the start script no longer starts sshd")
	}
	for _, link := range []string{
		"larri_link /workspace/.larri /root/.larri",
		"larri_link /workspace/.cache/huggingface /root/.cache/huggingface",
	} {
		i := strings.Index(s, link)
		if i < 0 {
			t.Fatalf("the start script does not run %q", link)
		}
		if apt := strings.Index(s, "apt-get"); i > apt {
			t.Errorf("%q runs after other setup; it must come first", link)
		}
	}
	// Nothing before sshd may end the script. A pod whose start script exits
	// here bills with nothing listening, until the stall limit notices.
	if strings.Contains(s[:sshd], "exit") {
		t.Error("the start script can exit before sshd starts")
	}
}

// The link function run for real, against every state the target can be in.
// It runs under set -e before sshd, so the property that matters is that it
// never fails — a pod that cannot be linked still serves anything that fits in
// 20 GB, and one whose script died here serves nothing and still bills.
func TestVolumeLinkNeverStopsTheScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	run := func(t *testing.T, root string, setup func(target string)) (reached bool, target string) {
		t.Helper()
		vol, target := filepath.Join(root, "vol", "d"), filepath.Join(root, "home", "d")
		if setup != nil {
			setup(target)
		}
		script := "set -e\n" + volumeLink + "larri_link " + vol + " " + target + "\necho reached\n"
		out, _ := exec.Command("bash", "-c", script).CombinedOutput()
		return strings.Contains(string(out), "reached"), target
	}
	linksToVolume := func(t *testing.T, target string) bool {
		t.Helper()
		dest, err := os.Readlink(target)
		return err == nil && strings.HasSuffix(dest, filepath.Join("vol", "d"))
	}

	t.Run("absent: linked", func(t *testing.T) {
		reached, target := run(t, t.TempDir(), nil)
		if !reached || !linksToVolume(t, target) {
			t.Errorf("reached=%v linked=%v", reached, linksToVolume(t, target))
		}
	})
	t.Run("existing link: re-pointed", func(t *testing.T) {
		reached, target := run(t, t.TempDir(), func(target string) {
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Symlink("/nonexistent", target)
		})
		if !reached || !linksToVolume(t, target) {
			t.Errorf("reached=%v linked=%v", reached, linksToVolume(t, target))
		}
	})
	t.Run("empty directory: replaced", func(t *testing.T) {
		reached, target := run(t, t.TempDir(), func(target string) { os.MkdirAll(target, 0o755) })
		if !reached || !linksToVolume(t, target) {
			t.Errorf("reached=%v linked=%v", reached, linksToVolume(t, target))
		}
	})
	t.Run("directory with contents: left alone, script continues", func(t *testing.T) {
		reached, target := run(t, t.TempDir(), func(target string) {
			os.MkdirAll(target, 0o755)
			os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o644)
		})
		if !reached {
			t.Fatal("an existing directory stopped the script before sshd")
		}
		if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
			t.Error("the existing directory's contents were disturbed")
		}
	})
	t.Run("volume cannot be created: script continues", func(t *testing.T) {
		root := t.TempDir()
		// A file where the volume's parent should be makes mkdir fail.
		os.WriteFile(filepath.Join(root, "vol"), []byte("x"), 0o644)
		reached, _ := run(t, root, nil)
		if !reached {
			t.Fatal("an unwritable volume stopped the script before sshd")
		}
	})
}

// Create rents Secure Cloud, so the catalogue must price Secure Cloud. Without
// the filter lowestPrice answers across both clouds, and a pod quoted at
// $2.78/hr billed $3.18/hr — above the figure it was ranked on and above the
// --max-price meant to cap it.
func TestCataloguePricesTheCloudCreateRentsFrom(t *testing.T) {
	if got := strings.Count(catalogueQuery, "secureCloud: true"); got != len(offerCounts) {
		t.Errorf("%d of %d price lookups filter on Secure Cloud; every one must, "+
			"or that size is quoted at a rate it will not be billed at", got, len(offerCounts))
	}
}
