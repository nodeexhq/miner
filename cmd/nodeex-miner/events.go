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
//	{"event":"lease_ended","lease":"<id>"}
type event struct {
	Event  string     `json:"event"`
	State  string     `json:"state,omitempty"`
	Node   string     `json:"node,omitempty"`
	Addr   string     `json:"addr,omitempty"`
	Lease  string     `json:"lease,omitempty"`
	EndsAt *time.Time `json:"ends_at,omitempty"`
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
