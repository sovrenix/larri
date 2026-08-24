// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider/providertest"
	"go.sovrenix.com/larri/internal/secret"
)

// A stub of RunPod's two APIs, faithful enough to run the shared contract.
type stubAPI struct {
	mu   sync.Mutex
	next int
	pods map[string]map[string]any
}

func (s *stubAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/graphql"):
			w.Write([]byte(catalogueJSON))

		case path == "/pods" && r.Method == http.MethodPost:
			var body createRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.next++
			id := "pod" + string(rune('0'+s.next))
			s.pods[id] = map[string]any{
				"id": id, "name": body.Name, "desiredStatus": "RUNNING",
				"costPerHr": 0.69, "volumeInGb": body.VolumeInGb,
			}
			json.NewEncoder(w).Encode(s.pods[id])

		case path == "/pods" && r.Method == http.MethodGet:
			list := make([]map[string]any, 0, len(s.pods))
			for _, v := range s.pods {
				list = append(list, v)
			}
			json.NewEncoder(w).Encode(list)

		case strings.HasPrefix(path, "/pods/"):
			id := strings.TrimPrefix(path, "/pods/")
			if r.Method == http.MethodDelete {
				if _, ok := s.pods[id]; !ok {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"error":"pod not found"}`))
					return
				}
				delete(s.pods, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			p, ok := s.pods[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"pod not found"}`))
				return
			}
			json.NewEncoder(w).Encode(p)

		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestRunpodConformance(t *testing.T) {
	stub := &stubAPI{pods: map[string]map[string]any{}}
	p := testProvider(t, stub.handler(t))
	providertest.Run(t, providertest.Harness{
		Provider: p,
		AbsentID: "pod-does-not-exist",
		AnOffer: func(*testing.T) core.Offer {
			return core.Offer{Provider: "runpod", OfferID: "NVIDIA GeForce RTX 4090",
				GPUCount: 1, VRAMPerGPUGB: 24, PriceHr: 0.69}
		},
		// Reaching EXITED needs a real pod; the stub would only be asserting
		// its own behaviour.
	})
}

// The catalogue half of the contract, against the real API.
//
// RunPod's GraphQL catalogue answers unauthenticated, so the normalisation
// that feeds selection can be verified for free and without an account. That
// is the half most likely to drift — a renamed field or a null where a number
// was expected — and it is exactly what a stub cannot tell you.
//
// The pod lifecycle stays unverified until someone runs this with a key.
func TestRunpodCatalogueLive(t *testing.T) {
	if os.Getenv("LARRI_RUNPOD_LIVE") != "yes" {
		t.Skip("set LARRI_RUNPOD_LIVE=yes to query the public runpod catalogue")
	}
	p := New(secret.Secret{}) // unauthenticated: the catalogue does not require a key
	p.OnDrift = func(err error) { t.Errorf("SHAPE DRIFT: %v", err) }

	// Only the search contract: everything else needs a key, and running
	// those checks unauthenticated would fail on a 401 while proving nothing
	// about the adapter.
	providertest.RunSearchContract(t, providertest.Harness{
		Provider: p, Criteria: core.Criteria{},
	})

	// And a shape check the shared contract cannot make, because it is
	// specific to what this catalogue is: every offer must name a GPU type
	// that POST /pods will accept.
	offers, err := p.Search(context.Background(), core.Criteria{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	if len(offers) < 10 {
		t.Errorf("only %d offers; the catalogue normally lists dozens", len(offers))
	}
	for _, o := range offers {
		// Every offered id must be one POST /pods accepts. The two APIs are
		// not kept in sync — the catalogue advertises a literal "unknown" and
		// MIG partitions the create enum lacks — and an offer that cannot be
		// bought is one selection will choose and then fail on.
		if !purchasable(o.OfferID) {
			t.Errorf("offer id %q is not purchasable", o.OfferID)
		}
		if o.MachineID != "" || o.HasReliability() {
			t.Errorf("offer %s invented host detail runpod does not publish", o.OfferID)
		}
	}
	t.Logf("catalogue: %d rentable gpu types, cheapest $%.3f/hr (%s)",
		len(offers), offers[0].PriceHr, offers[0].GPUModel)
}
