package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// event is one line of `run -events`: a change of state that a supervising
// program (the macOS app) acts on. The log on standard error is for people;
// these lines are the interface, and their fields are only ever added to.
//
//	{"event":"node","state":"active","node":"<id>"}
//	{"event":"gate","state":"open","addr":"100.64.0.3"}      state: open | suspended
//	{"event":"lease_started","lease":"<id>","ends_at":"..."}
//	{"event":"lease_keys","lease":"<id>","ssh_keys":["ssh-ed25519 ..."]}   the renter's keys, whenever they change
//	{"event":"lease_ended","lease":"<id>"}
//	{"event":"lease_state","state":"active","lease":"<id>","ends_at":"...","confirmed_at":"...","ssh_keys":[...]}
//	{"event":"lease_state","state":"idle","confirmed_at":"..."}
//
// lease_state is a complete, authoritative snapshot on EVERY successful
// heartbeat, including renewals and an explicit absence of a lease. A VM
// supervisor must not infer authorization from gate status or old disk data.
//
//	{"event":"sessions","sessions":<n>}   (when the number of renter connections changes)
//	{"event":"bandwidth","up_mbps":<n>,"down_mbps":<n>}   (when the machine's limits change; 0 is none)
type event struct {
	Event       string     `json:"event"`
	State       string     `json:"state,omitempty"`
	Node        string     `json:"node,omitempty"`
	Addr        string     `json:"addr,omitempty"`
	Lease       string     `json:"lease,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	// SSHKeys is the complete key set on lease_keys and active lease_state;
	// an omitted/empty set revokes every renter key.
	SSHKeys []string `json:"ssh_keys,omitempty"`
	// Sessions is the number of renter connections open, on sessions
	// only: the app installs updates while nobody is connected.
	Sessions *int `json:"sessions,omitempty"`
	// UpMbps and DownMbps limit the rented machine's internet traffic, on
	// bandwidth events; zero means no limit.
	UpMbps   *float64 `json:"up_mbps,omitempty"`
	DownMbps *float64 `json:"down_mbps,omitempty"`
}

func leaseEvent(id string, endsAt time.Time, keys []string, confirmedAt time.Time) event {
	e := event{Event: "lease_state", State: "idle", ConfirmedAt: &confirmedAt}
	if id != "" {
		e.State, e.Lease, e.EndsAt = "active", id, &endsAt
		e.SSHKeys = append([]string{}, keys...)
	}
	return e
}

// eventWriter returns the function that reports events: to standard output
// as JSON lines when on, nowhere otherwise.
func eventWriter(on bool) func(event) {
	if !on {
		return func(event) {}
	}
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	return func(e event) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(e)
	}
}
