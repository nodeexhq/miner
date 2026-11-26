package main

import (
	"encoding/json"
	"testing"
)

func TestHeartbeatRemeasure(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{`{"status":"active","lease":null}`, false},
		{`{"status":"active","lease":null,"remeasure":true}`, true},
	} {
		var res heartbeatResponse
		if err := json.Unmarshal([]byte(c.body), &res); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if res.Remeasure != c.want {
			t.Errorf("%s: remeasure = %v, want %v", c.body, res.Remeasure, c.want)
		}
	}
}
