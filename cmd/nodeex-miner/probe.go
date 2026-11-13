package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// probeInterval is how often the Miner has the platform re-measure its link.
const probeInterval = 6 * time.Hour

// probeTimeout bounds each direction of a probe. A link too slow to move
// the payload in this time goes unmeasured.
const probeTimeout = 75 * time.Second

// repeatReader yields the same random block over and over. The data only
// needs to be incompressible, not unpredictable.
type repeatReader struct {
	block []byte
	off   int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := copy(p, r.block[r.off:])
	r.off = (r.off + n) % len(r.block)
	return n, nil
}

type probeTicket struct {
	Ticket string `json:"ticket"`
	// ProbeURL is set when probes run against an edge Worker rather than
	// the platform itself.
	ProbeURL      string `json:"probe_url"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
}

// runProbe has this node's uplink and downlink measured. The Miner only
// moves the bytes; the timing is done by the other end.
func runProbe(ctx context.Context, id identity, priv ed25519.PrivateKey) (string, error) {
	var ticket probeTicket
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := call(tctx, id.API, priv, "POST", "/v1/nodes/"+id.NodeID+"/probe", nil, &ticket)
	cancel()
	if err != nil {
		return "", err
	}
	if ticket.ProbeURL != "" {
		return probeWorker(ctx, ticket)
	}
	return probePlatform(ctx, id, ticket)
}

// workerCall makes one request to the probe Worker and decodes its JSON reply.
func workerCall(ctx context.Context, method, url, ticket, stamp string, body io.Reader, length int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("X-Probe-Ticket", ticket)
	if stamp != "" {
		req.Header.Set("X-Probe-Stamp", stamp)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d", method, url, res.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// probeWorker runs a probe against the edge Worker. Each direction starts
// with a stamp from the Worker, which is where its clock starts.
func probeWorker(ctx context.Context, t probeTicket) (string, error) {
	base := t.ProbeURL
	type begin struct {
		Stamp string `json:"stamp"`
		Bytes int64  `json:"bytes"`
	}

	// Uplink: the Worker stops the clock when the whole body has arrived.
	uctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var up begin
	if err := workerCall(uctx, "GET", base+"/begin?dir=up", t.Ticket, "", nil, 0, &up); err != nil {
		return "", fmt.Errorf("uplink probe: %w", err)
	}
	block := make([]byte, 1<<20)
	rand.Read(block)
	var upRes struct {
		Mbps float64 `json:"uplink_mbps"`
	}
	err := workerCall(uctx, "PUT", base+"/upload", t.Ticket, up.Stamp,
		io.LimitReader(&repeatReader{block: block}, up.Bytes), up.Bytes, &upRes)
	if err != nil {
		return "", fmt.Errorf("uplink probe: %w", err)
	}

	// Downlink: the Miner proves it received everything by returning the
	// payload's last bytes, and the Worker stops the clock on that.
	dctx, cancel2 := context.WithTimeout(ctx, probeTimeout)
	defer cancel2()
	var down begin
	if err := workerCall(dctx, "GET", base+"/begin?dir=down", t.Ticket, "", nil, 0, &down); err != nil {
		return "", fmt.Errorf("downlink probe: %w", err)
	}
	req, err := http.NewRequestWithContext(dctx, "GET", base+"/download", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Probe-Ticket", t.Ticket)
	req.Header.Set("X-Probe-Stamp", down.Stamp)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downlink probe: %w", err)
	}
	tail, err := lastBytes(res.Body, 32)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downlink probe: HTTP %d, %v", res.StatusCode, err)
	}
	body, _ := json.Marshal(map[string]string{"tail": hex.EncodeToString(tail)})
	var downRes struct {
		Mbps float64 `json:"downlink_mbps"`
	}
	if err := workerCall(dctx, "POST", base+"/done", t.Ticket, down.Stamp, bytes.NewReader(body), int64(len(body)), &downRes); err != nil {
		return "", fmt.Errorf("downlink probe: %w", err)
	}
	return fmt.Sprintf("uplink %.1f Mbps, downlink %.1f Mbps", upRes.Mbps, downRes.Mbps), nil
}

// lastBytes drains r and returns its final n bytes.
func lastBytes(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, 256<<10)
	var tail []byte
	for {
		k, err := r.Read(buf)
		if k > 0 {
			tail = append(tail, buf[:k]...)
			if len(tail) > n {
				tail = append(tail[:0], tail[len(tail)-n:]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(tail) < n {
		return nil, fmt.Errorf("payload shorter than %d bytes", n)
	}
	return tail, nil
}

// probePlatform runs a probe against the platform's own endpoints, which
// time the transfers directly.
func probePlatform(ctx context.Context, id identity, t probeTicket) (string, error) {
	// The server stops reading after its own time limit, so a slow link
	// ends with a closed connection; whatever arrived is what is measured.
	block := make([]byte, 1<<20)
	rand.Read(block)
	uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	req, err := http.NewRequestWithContext(uctx, "PUT", id.API+"/v1/probe/"+t.Ticket+"/upload",
		io.LimitReader(&repeatReader{block: block}, t.UploadBytes))
	if err != nil {
		cancel()
		return "", err
	}
	req.ContentLength = t.UploadBytes
	var up struct {
		UplinkMbps float64 `json:"uplink_mbps"`
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&up)
		res.Body.Close()
	}
	cancel()
	if err != nil {
		return "", fmt.Errorf("uplink probe: %w", err)
	}

	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err = http.NewRequestWithContext(dctx, "GET", id.API+"/v1/probe/"+t.Ticket+"/download", nil)
	if err != nil {
		return "", err
	}
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downlink probe: %w", err)
	}
	got, _ := io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return fmt.Sprintf("uplink %.1f Mbps; downlink test received %d MB", up.UplinkMbps, got>>20), nil
}
