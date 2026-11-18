package main

import (
	"encoding/json"
	"errors"
	"time"
)

type heartbeatLease struct {
	ID      string    `json:"id"`
	EndsAt  time.Time `json:"ends_at"`
	SSHKeys []string  `json:"ssh_keys"`
}

type heartbeatResponse struct {
	Status string
	Lease  *heartbeatLease
}

// Missing fields are not an idle confirmation: treating a partial reply as
// lease:null would destroy the renter's disk. Only explicit null means idle.
func (r *heartbeatResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Status string          `json:"status"`
		Lease  json.RawMessage `json:"lease"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Status == "" || len(wire.Lease) == 0 {
		return errors.New("incomplete heartbeat response")
	}
	var lease *heartbeatLease
	if err := json.Unmarshal(wire.Lease, &lease); err != nil {
		return err
	}
	if lease != nil && (lease.ID == "" || lease.EndsAt.IsZero()) {
		return errors.New("incomplete heartbeat lease")
	}
	r.Status, r.Lease = wire.Status, lease
	return nil
}
