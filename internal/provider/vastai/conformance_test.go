// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vastai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider/providertest"
)

// A stub of the Vast API, faithful enough to run the shared contract. It is
// the same suite the fake runs, which is what would have caught the label
// normalisation drifting apart in the first place.
type stubAPI struct {
	mu        sync.Mutex
	next      int64
	instances map[string]map[string]any
}

func (s *stubAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v0/bundles"):
			fmt.Fprint(w, `{"offers":[]}`)

		case strings.HasPrefix(r.URL.Path, "/api/v0/asks/"):
			var body createRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.next++
			id := s.next
			s.instances[strconv.FormatInt(id, 10)] = map[string]any{
				"id": id, "actual_status": "running", "label": body.Label,
				"dph_total": 1.29, "storage_cost": 0.01,
			}
			fmt.Fprintf(w, `{"success":true,"new_contract":%d}`, id)

		case strings.HasPrefix(r.URL.Path, "/api/v1/instances"):
			list := make([]map[string]any, 0, len(s.instances))
			for _, v := range s.instances {
				list = append(list, v)
			}
			b, _ := json.Marshal(map[string]any{
				"success": true, "instances": list, "next_token": nil,
			})
			w.Write(b)

		case strings.HasPrefix(r.URL.Path, "/api/v0/instances/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v0/instances/"), "/")
			delete(s.instances, id)
			fmt.Fprint(w, `{"success":true}`)

		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestVastaiConformance(t *testing.T) {
	stub := &stubAPI{instances: map[string]map[string]any{}}
	p := testProvider(t, stub.handler(t))
	providertest.Run(t, providertest.Harness{
		Provider: p,
		AnOffer: func(*testing.T) core.Offer {
			return core.Offer{Provider: "vastai", OfferID: "9182736", PriceHr: 1.29}
		},
	})
}
