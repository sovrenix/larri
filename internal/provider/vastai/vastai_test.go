// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vastai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/secret"
)

func testProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(secret.New("test-key"))
	c.BaseURL = srv.URL
	p := NewWithClient(c)
	p.OnDrift = func(err error) { t.Logf("drift: %v", err) }
	p.OnNotice = func(m string) { t.Logf("notice: %s", m) }
	return p
}

// An offer as Vast actually returns it: many more fields than LARRI models.
// Strict decoding would reject this, which is why validation lives in
// normalise() and targets the fields we depend on rather than the ones we
// ignore.
const realisticOffer = `{
  "id": 9182736, "ask_contract_id": 9182736, "gpu_name": "A100 SXM4",
  "num_gpus": 1, "gpu_ram": 81920, "dph_total": 1.29, "reliability": 0.987,
  "cuda_max_good": 12.4, "inet_down": 940.5, "disk_space": 220.0,
  "cpu_cores": 32, "cpu_ram": 258048, "geolocation": "US, California",
  "machine_id": 44221, "verified": true, "rentable": true, "is_bid": false,
  "driver_version": "550.90.07",
  "hosting_type": 1, "gpu_frac": 1.0, "dlperf": 42.7, "cpu_name": "EPYC 7413",
  "inet_up": 880.1, "min_bid": 0.51, "external": false, "webpage": null,
  "logo": "/static/logo.png", "compute_cap": 800, "total_flops": 19.5
}`

func TestSearchNormalisesARealisticOffer(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/bundles/" {
			t.Errorf("search path = %s, want /api/v0/bundles/", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		fmt.Fprintf(w, `{"offers":[%s]}`, realisticOffer)
	})

	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	o := offers[0]
	// The unit conversion that would otherwise make an A100 look like an
	// 80 TB card and pass every sizing check.
	if o.VRAMPerGPUGB != 80 {
		t.Errorf("VRAM = %d GB, want 80 (gpu_ram arrives in MiB)", o.VRAMPerGPUGB)
	}
	if o.PriceHr != 1.29 {
		t.Errorf("price = %v, want 1.29", o.PriceHr)
	}
	if o.GPUModel != "A100 SXM4" || o.GPUCount != 1 {
		t.Errorf("gpu = %s x%d", o.GPUModel, o.GPUCount)
	}
	if o.Reliability != 0.987 {
		t.Errorf("reliability = %v", o.Reliability)
	}
	if !o.Certified {
		t.Error("verified offers map to Certified, the only trust signal the marketplace offers")
	}
	if o.CUDAVersion != "12.4" {
		t.Errorf("cuda = %q, want 12.4 — image variant selection is a search filter", o.CUDAVersion)
	}
	if o.RAMGB != 252 {
		t.Errorf("cpu_ram = %d GB, want 252 (also MiB)", o.RAMGB)
	}
}

// R-02 in its most expensive form: a renamed or re-united field must fail
// loudly rather than produce a confident wrong number.
func TestImplausibleFieldsAreRefused(t *testing.T) {
	cases := map[string]string{
		"gpu_ram in GB not MiB": `{"id":1,"gpu_name":"A100","num_gpus":1,"gpu_ram":80,"dph_total":1.29}`,
		"dph_total absent":      `{"id":1,"gpu_name":"A100","num_gpus":1,"gpu_ram":81920}`,
		"gpu_name absent":       `{"id":1,"num_gpus":1,"gpu_ram":81920,"dph_total":1.29}`,
		"negative price":        `{"id":1,"gpu_name":"A100","num_gpus":1,"gpu_ram":81920,"dph_total":-1}`,
	}
	for name, body := range cases {
		var o wireOffer
		if err := json.Unmarshal([]byte(body), &o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := o.normalise(); err == nil {
			t.Errorf("%s: must be refused, not normalised", name)
		}
	}
}

// The pagination hazard. The endpoint caps at 25 per page, so an adapter that
// read one page would silently miss every orphan past the twenty-fifth —
// R-01 arriving through a default.
func TestListPaginatesToExhaustion(t *testing.T) {
	const total = 63
	var pages int
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/instances") {
			t.Errorf("list path = %s, want /api/v1/instances", r.URL.Path)
		}
		pages++
		after := r.URL.Query().Get("after_token")
		start := 0
		if after != "" {
			fmt.Sscanf(after, "tok%d", &start)
		}
		end := start + listPageMax
		if end > total {
			end = total
		}
		var items []string
		for i := start; i < end; i++ {
			items = append(items, fmt.Sprintf(
				`{"id":%d,"actual_status":"running","dph_total":1.0}`, 1000+i))
		}
		next := "null"
		if end < total {
			next = fmt.Sprintf(`"tok%d"`, end)
		}
		fmt.Fprintf(w, `{"success":true,"instances":[%s],"next_token":%s}`,
			strings.Join(items, ","), next)
	})

	got, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("got %d instances across %d pages, want %d — "+
			"an unpaginated List hides every orphan past the first page", len(got), pages, total)
	}
	if pages < 3 {
		t.Errorf("expected at least 3 pages at %d per page, got %d", listPageMax, pages)
	}
}

// FR-DEL-03: List must surface non-running instances. Filtering to running
// would hide a stopped container that still bills for storage, and teardown
// would read absence as proof of destruction (R-13).
func TestListIncludesStoppedInstances(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("select_filters"); q != "" {
			t.Errorf("List must not filter by status, got select_filters=%s", q)
		}
		fmt.Fprint(w, `{"success":true,"next_token":null,"instances":[
		  {"id":1,"actual_status":"running","dph_total":1.0,"storage_cost":0.02},
		  {"id":2,"actual_status":"exited","dph_total":1.0,"storage_cost":0.02},
		  {"id":3,"actual_status":"offline","dph_total":1.0,"storage_cost":0.02}
		]}`)
	})
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d instances, want 3 including the stopped ones", len(got))
	}
	running := 0
	for _, i := range got {
		if i.Running {
			running++
		}
		if i.StorageHr == 0 {
			t.Error("storage cost must be carried: a stopped container still bills")
		}
	}
	if running != 1 {
		t.Errorf("running = %d, want 1; exited and offline are not running", running)
	}
}

// R-07: a transport failure on a mutation is an unknown outcome, never a
// failure. Classifying it as retryable is how one instance becomes two.
func TestCreateTransportFailureIsUnknownOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := NewClient(secret.New("k"))
	c.BaseURL = "http://127.0.0.1:1" // refused
	srv.Close()
	p := NewWithClient(c)

	_, err := p.Create(context.Background(), core.Offer{OfferID: "1"}, provider.CreateSpec{Label: "larri:X"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errs.Is(err, errs.ClassProviderUnknownOutcome) {
		t.Fatalf("class = %s, want provider-unknown-outcome", errs.ClassOf(err))
	}
	if errs.Retryable(err) {
		t.Fatal("a mutation with an unknown outcome must never be retryable")
	}
}

// FR-SEC-15: only SSH is mapped. A -p for the runtime port would publish an
// inference endpoint on a shared public IP.
func TestCreateRequestsNoRuntimePortMapping(t *testing.T) {
	var body createRequest
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/instances") {
			fmt.Fprint(w, `{"success":true,"instances":[],"next_token":null}`)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"success":true,"new_contract":14872213}`)
	})
	_, err := p.Create(context.Background(),
		core.Offer{OfferID: "9182736", PriceHr: 1.29},
		provider.CreateSpec{Image: "img", DiskGB: 200, Label: "larri:01J9Z"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Env, "-p 8000") {
		t.Errorf("env must not map the runtime port, got %q", body.Env)
	}
	if body.RunType != "ssh" {
		t.Errorf("runtype = %q, want ssh", body.RunType)
	}
	if body.Label != "larri:01J9Z" {
		t.Errorf("label = %q; the rig marker is what makes orphans attributable", body.Label)
	}
	if body.TargetState != "running" {
		t.Errorf("target_state = %q", body.TargetState)
	}
}

func TestGetAbsentInstanceReturnsNilNotError(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such instance"}`)
	})
	inst, err := p.Get(context.Background(), "14872213")
	if err != nil {
		t.Fatalf("absence is not an error: %v", err)
	}
	if inst != nil {
		t.Fatal("absent means nil, which is the only proof of destruction")
	}
}

// The two routes disagree in shape, and the disagreement is load-bearing: the
// list route returns an array under "instances", the single-instance route
// returns one object under the same key. Confirmed against the live API rather
// than assumed, because guessing here yields a decode error at exactly the
// moment supervision needs an answer.
func TestGetReadsTheSingleInstanceShape(t *testing.T) {
	var path string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{"instances":{"id":14872213,"actual_status":"running",
		  "dph_total":1.29,"storage_cost":0.01,"ssh_host":"ssh5.vast.ai",
		  "ssh_port":25982,"label":"larri:01J9ZTESTRIGIDTESTRIGIDXX"}}`)
	})
	inst, err := p.Get(context.Background(), "14872213")
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		t.Fatal("should have found the instance")
	}
	if path != "/api/v0/instances/14872213/" {
		t.Errorf("path = %s; Get must not enumerate the account", path)
	}
	if !inst.Running || inst.SSHHost != "ssh5.vast.ai" || inst.SSHPort != 25982 {
		t.Errorf("instance not normalised: %+v", inst)
	}
	if id, ok := inst.RigID(); !ok || id != "01J9ZTESTRIGIDTESTRIGIDXX" {
		t.Errorf("label not decoded: %q", id)
	}
}

func TestRateLimitIsRetryable(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"slow down"}`)
	})
	_, err := p.Search(context.Background(), core.Criteria{})
	if !errs.Retryable(err) {
		t.Fatalf("429 should be retryable, class = %s", errs.ClassOf(err))
	}
}

func TestBadKeyIsNotRetryable(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := p.Search(context.Background(), core.Criteria{})
	if errs.Retryable(err) {
		t.Fatal("a bad key does not improve with retries")
	}
	if !strings.Contains(err.Error(), "VASTAI_API_KEY") {
		t.Errorf("the message should name the credential to check: %v", err)
	}
}

// Q-04: interruptible offers are opt-in, so the default search must not ask
// for bid contracts.
func TestSearchDefaultsToOnDemand(t *testing.T) {
	var req searchRequest
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprint(w, `{"offers":[]}`)
	})
	if _, err := p.Search(context.Background(), core.Criteria{}); err != nil {
		t.Fatal(err)
	}
	if req.Type != "on-demand" {
		t.Errorf("default search type = %q, want on-demand", req.Type)
	}
	if req.Rentable == nil || !req.Rentable.Eq {
		t.Error("search should ask only for rentable offers")
	}
}

// A live run returned exactly 500 offers for a broad query, which is the
// request limit. The API carries no truncation flag, so a full page is the
// only signal that more matched — and since the server sorts by price
// ascending, the ones dropped are the more expensive, better-fitting cards a
// value-weighted ranking most needs to consider.
func TestSearchReportsTruncation(t *testing.T) {
	var notices []string
	full := make([]string, searchLimit)
	for i := range full {
		full[i] = fmt.Sprintf(
			`{"id":%d,"gpu_name":"V100","num_gpus":1,"gpu_ram":32768,"dph_total":0.02}`, i)
	}
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"offers":[%s]}`, strings.Join(full, ","))
	})
	p.OnNotice = func(m string) { notices = append(notices, m) }

	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != searchLimit {
		t.Fatalf("got %d offers, want %d", len(offers), searchLimit)
	}
	if len(notices) == 0 {
		t.Fatal("a full page must be reported as truncated, not passed off as complete")
	}
	if !strings.Contains(notices[0], "truncated") {
		t.Errorf("notice should name the problem: %q", notices[0])
	}
}

func TestSearchBelowLimitReportsNothing(t *testing.T) {
	var notices []string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"offers":[%s]}`, realisticOffer)
	})
	p.OnNotice = func(m string) { notices = append(notices, m) }
	if _, err := p.Search(context.Background(), core.Criteria{}); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 0 {
		t.Errorf("a partial page is complete and must not warn: %v", notices)
	}
}

// The behaviour that makes this endpoint dangerous to trust by status code:
// Vast answers a failed attach with HTTP 200 and success:false. Verified
// against the live API — a bogus instance id returns 200 carrying an internal
// error, where an absent route returns 404.
//
// If this check regresses, adopt reports a key installed that is not, and the
// operator sees an authentication failure against a host that never received
// it — a confusing symptom two layers from its cause.
func TestAttachSSHKeyTreats200WithSuccessFalseAsFailure(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success": false, "error": "add_ssh_to_instance",
			"msg": "Error adding SSH key to instance"}`))
	})
	err := p.AttachSSHKey(context.Background(), "999", "ssh-ed25519 AAAA test")
	if err == nil {
		t.Fatal("a declined attach reported success")
	}
	if !strings.Contains(err.Error(), "Error adding SSH key") {
		t.Errorf("error should carry the provider's reason, got: %v", err)
	}
}

func TestAttachSSHKeySendsTheKeyToTheInstanceRoute(t *testing.T) {
	var path, body string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"success": true, "msg": "SSH key attached successfully"}`))
	})
	if err := p.AttachSSHKey(context.Background(), "48429759", "ssh-ed25519 AAAAC3 larri"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// The trailing slash is not cosmetic: Vast 301s without it, and a
	// redirected POST arrives without its body.
	if path != "/api/v0/instances/48429759/ssh/" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(body, `"ssh_key"`) || !strings.Contains(body, "AAAAC3") {
		t.Errorf("body = %q", body)
	}
}

// Providers log the User-Agent, and a version in their log is what turns "some
// client is hammering the offers endpoint" into a specific release with a
// specific bug — including for them, when they need to tell us.
func TestRequestsIdentifyLarriAndItsVersion(t *testing.T) {
	var ua string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"offers":[]}`))
	})
	if _, err := p.Search(context.Background(), core.Criteria{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ua, "larri/") {
		t.Errorf("User-Agent = %q; requests do not identify larri", ua)
	}
	if ua == "larri/" {
		t.Error("User-Agent carries no version")
	}
}
