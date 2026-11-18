package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLeaseSnapshotsIncludeRenewalsAndExplicitAbsence(t *testing.T) {
	now := time.Now().UTC()
	end := now.Add(time.Hour)
	keys := []string{"ssh-ed25519 first"}
	first := leaseEvent("lease-1", end, keys, now)
	keys[0] = "mutated"
	if first.Event != "lease_state" || first.State != "active" || first.Lease != "lease-1" ||
		first.EndsAt == nil || !first.EndsAt.Equal(end) || first.ConfirmedAt == nil || !first.ConfirmedAt.Equal(now) ||
		len(first.SSHKeys) != 1 || first.SSHKeys[0] != "ssh-ed25519 first" {
		t.Fatalf("incomplete snapshot: %+v", first)
	}
	renewed := leaseEvent("lease-1", end.Add(time.Hour), nil, now.Add(15*time.Second))
	if renewed.Lease != first.Lease || !renewed.EndsAt.After(*first.EndsAt) || !renewed.ConfirmedAt.After(*first.ConfirmedAt) {
		t.Fatal("a renewal must carry its extended deadline even when the lease ID did not change")
	}
	idle := leaseEvent("", time.Time{}, nil, now.Add(30*time.Second))
	raw, err := json.Marshal(idle)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["event"] != "lease_state" || wire["state"] != "idle" || wire["confirmed_at"] == nil || wire["lease"] != nil || wire["ends_at"] != nil {
		t.Fatalf("absence must be explicit, not an ambiguous missing event: %s", raw)
	}
}

func TestHeartbeatRequiresExplicitLeaseState(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"status":"active"}`, `{"lease":null}`,
		`{"status":"active","lease":{}}`,
		`{"status":"active","lease":{"id":"lease-1"}}`,
		`{"status":"active","lease":{"ends_at":"2026-11-18T10:00:00Z"}}`,
	} {
		var res heartbeatResponse
		if err := json.Unmarshal([]byte(raw), &res); err == nil {
			t.Errorf("incomplete reply must not authorize boot or erase a disk: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"status":"active","lease":null}`,
		`{"status":"active","lease":{"id":"lease-1","ends_at":"2026-11-18T10:00:00Z","ssh_keys":[]}}`,
	} {
		var res heartbeatResponse
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatalf("complete reply rejected: %s: %v", raw, err)
		}
	}
}

func TestHeartbeatCallRejectsEmptySuccessResponses(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", `{}`, `{"status":"active"}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(body))
			}))
			defer srv.Close()
			var res heartbeatResponse
			if err := call(context.Background(), srv.URL, priv, "POST", "/heartbeat", nil, &res); err == nil {
				t.Fatal("empty or incomplete success must not publish a lease confirmation")
			}
		})
	}
}
