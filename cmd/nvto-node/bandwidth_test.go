package main

import (
	"encoding/json"
	"testing"
)

func TestHeartbeatBandwidth(t *testing.T) {
	var r heartbeatResponse
	if err := json.Unmarshal([]byte(`{"status":"active","lease":null,"machine_bandwidth":{"up_mbps":33.1,"down_mbps":240}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Bandwidth == nil || r.Bandwidth.UpMbps != 33.1 || r.Bandwidth.DownMbps != 240 {
		t.Errorf("bandwidth = %+v", r.Bandwidth)
	}
	// An older platform sends none: no limit, and the reply is still whole.
	r = heartbeatResponse{}
	if err := json.Unmarshal([]byte(`{"status":"active","lease":null}`), &r); err != nil || r.Bandwidth != nil {
		t.Errorf("without limits: %+v, %v", r.Bandwidth, err)
	}
	up, down := 0.0, 12.5
	b, _ := json.Marshal(event{Event: "bandwidth", UpMbps: &up, DownMbps: &down})
	if string(b) != `{"event":"bandwidth","up_mbps":0,"down_mbps":12.5}` {
		t.Errorf("event = %s", b)
	}
}
