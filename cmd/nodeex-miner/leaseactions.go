package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Two ways the node ends its own lease at once, each a one-shot command the
// app runs with the node's device key, like enroll:
//
//	abuse       the machine's network helper cut the machine off for scanning
//	            or flooding from the owner's address; the platform ends the
//	            lease (POST /v1/nodes/{id}/abuse)
//	stop-lease  the owner's emergency stop; the renter is refunded the
//	            period and mining is switched off (POST /v1/nodes/{id}/stop-lease)
//
// Each prints the platform's reply as one JSON line on standard output. The
// exit status tells the app what happened.
const (
	exitFailed = 1 // not done: the platform could not be reached, or failed
	// exitLeaseNotCurrent: the lease is not the one the platform says this
	// node serves; it has already ended.
	exitLeaseNotCurrent = 3
	// exitRefused: the platform refused the request (any other 4xx).
	exitRefused = 4
)

// exitError is an error with the exit status it should end the program with.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitCode(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return exitFailed
}

// What the platform takes (internal/api/abuse.go there).
var abuseReasons = []string{"fan_out", "port_scan", "half_open", "sustained_new_flows", "sustained_udp"}

const (
	maxAbuseDetail = 1000
	maxStopReason  = 500
)

type abuseRequest struct {
	LeaseID string `json:"lease_id"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
}

type abuseReply struct {
	LeaseID string    `json:"lease_id"`
	Reason  string    `json:"reason"`
	EndedAt time.Time `json:"ended_at"`
	// Paid is the owner's share of the rent for the time the lease ran in
	// its last period.
	Paid string `json:"paid"`
}

type stopLeaseRequest struct {
	LeaseID string `json:"lease_id"`
	Reason  string `json:"reason"`
}

type stopLeaseReply struct {
	LeaseID     string     `json:"lease_id"`
	EndedAt     time.Time  `json:"ended_at"`
	Refunded    string     `json:"refunded"`
	Mining      bool       `json:"mining"`
	PausedUntil *time.Time `json:"paused_until,omitempty"`
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// newAbuseRequest checks what the platform would refuse, so that a report
// is not lost to a 400. A detail too long is cut short rather than refused.
func newAbuseRequest(lease, reason, detail string) (abuseRequest, error) {
	if lease == "" {
		return abuseRequest{}, &exitError{2, errors.New("abuse needs -lease")}
	}
	if !slices.Contains(abuseReasons, reason) {
		return abuseRequest{}, &exitError{2, fmt.Errorf("abuse -reason must be one of %s", strings.Join(abuseReasons, ", "))}
	}
	return abuseRequest{LeaseID: lease, Reason: reason, Detail: truncate(detail, maxAbuseDetail)}, nil
}

// poster sends one device-signed request to the platform.
type poster func(ctx context.Context, method, path string, in, out any) error

// permanent reports whether the platform's answer settles the request: any
// 4xx but 408 and 429, which say to try again.
func permanent(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.Status >= 400 && se.Status < 500 && se.Status != 408 && se.Status != 429
}

// verdict gives a failed request its exit status.
func verdict(err error) error {
	var se *statusError
	switch {
	case errors.As(err, &se) && se.Code == "lease_not_current":
		return &exitError{exitLeaseNotCurrent, err}
	case permanent(err):
		return &exitError{exitRefused, err}
	}
	return err
}

// Backoff between abuse report attempts.
const (
	abuseFirstRetry = 2 * time.Second
	abuseMaxRetry   = time.Minute
)

// reportAbuse sends the report until the platform answers it: a 200, or a
// refusal that trying again cannot change (409 lease_not_current: the lease
// already ended). Anything else (no connection, 5xx, 408, 429) is tried
// again, 2 s later, then 4, 8 … up to a minute apart, until ctx ends.
func reportAbuse(ctx context.Context, post poster, nodeID string, req abuseRequest, sleep func(context.Context, time.Duration) error) (abuseReply, error) {
	var reply abuseReply
	wait := abuseFirstRetry
	for attempt := 1; ; attempt++ {
		err := post(ctx, "POST", "/v1/nodes/"+nodeID+"/abuse", req, &reply)
		if err == nil {
			return reply, nil
		}
		if permanent(err) {
			return reply, verdict(err)
		}
		log.Printf("abuse report, attempt %d: %v; trying again in %s", attempt, err, wait)
		if sleep(ctx, wait) != nil {
			return reply, fmt.Errorf("abuse report not delivered after %d attempts: %w", attempt, err)
		}
		wait = min(wait*2, abuseMaxRetry)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// devicePoster is call() with this node's identity.
func devicePoster(id identity) (poster, error) {
	priv, err := id.key()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, method, path string, in, out any) error {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return call(cctx, id.API, priv, method, path, in, out)
	}, nil
}

func printJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func cmdAbuse(args []string) error {
	fs := flag.NewFlagSet("abuse", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	lease := fs.String("lease", "", "the lease whose machine was cut off")
	reason := fs.String("reason", "", "what the network helper saw: "+strings.Join(abuseReasons, ", "))
	detail := fs.String("detail", "", "the helper's description")
	within := fs.Duration("for", 30*time.Minute, "how long to keep trying to reach the platform")
	fs.Parse(args)
	req, err := newAbuseRequest(*lease, *reason, *detail)
	if err != nil {
		return err
	}
	id, err := loadIdentity(*dir)
	if err != nil {
		return err
	}
	post, err := devicePoster(id)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, *within)
	defer cancel()
	reply, err := reportAbuse(ctx, post, id.NodeID, req, sleepCtx)
	if err != nil {
		return err
	}
	printJSON(reply)
	return nil
}

func cmdStopLease(args []string) error {
	fs := flag.NewFlagSet("stop-lease", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	lease := fs.String("lease", "", "the lease to stop")
	reason := fs.String("reason", "", "why, in the owner's words (optional)")
	fs.Parse(args)
	if *lease == "" {
		return &exitError{2, errors.New("stop-lease needs -lease")}
	}
	id, err := loadIdentity(*dir)
	if err != nil {
		return err
	}
	post, err := devicePoster(id)
	if err != nil {
		return err
	}
	reply, err := stopLease(context.Background(), post, id.NodeID, *lease, *reason)
	if err != nil {
		return err
	}
	printJSON(reply)
	return nil
}

// stopLease asks once: the owner is waiting, and can ask again.
func stopLease(ctx context.Context, post poster, nodeID, lease, reason string) (stopLeaseReply, error) {
	var reply stopLeaseReply
	req := stopLeaseRequest{LeaseID: lease, Reason: truncate(reason, maxStopReason)}
	if err := post(ctx, "POST", "/v1/nodes/"+nodeID+"/stop-lease", req, &reply); err != nil {
		return reply, verdict(err)
	}
	return reply, nil
}
