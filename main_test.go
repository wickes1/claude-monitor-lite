package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// classifyUpdate encodes what the menu bar may show after a failed fetch. The
// key case: with cached data, a transient failure that persists (an expired
// token over a weekend, an outage) must eventually show "Error" — otherwise the
// menu bar presents frozen numbers as current. Only a 429 keeps stale data
// indefinitely, because its backoff recovers on its own.
func TestClassifyUpdate(t *testing.T) {
	authErr := fmt.Errorf("%w: detail", ErrAuthFailed)
	transientErr := fmt.Errorf("%w: detail", ErrTransient)
	rateLimitErr := &RateLimitError{}
	unknownErr := errors.New("something unexpected")
	past := maxTransientErrors + 1

	cases := []struct {
		name      string
		err       error
		count     int
		haveCache bool
		want      updateAction
	}{
		{"auth failure shows login immediately", authErr, 1, true, actionShowAuth},
		{"auth failure outranks transient count", authErr, 99, true, actionShowAuth},
		{"first transient keeps stale data", transientErr, 1, true, actionKeepStale},
		{"transient at tolerance boundary keeps stale", transientErr, maxTransientErrors, true, actionKeepStale},
		{"persistent transient shows error despite cache", transientErr, past, true, actionShowError},
		{"persistent 429 with cache keeps stale", rateLimitErr, 99, true, actionKeepStale},
		{"persistent 429 on cold start shows error", rateLimitErr, past, false, actionShowError},
		{"unknown error shows error immediately", unknownErr, 1, true, actionShowError},
	}

	for _, tc := range cases {
		if got := classifyUpdate(tc.err, tc.count, tc.haveCache); got != tc.want {
			t.Errorf("%s: classifyUpdate(%v, %d, %v) = %v, want %v",
				tc.name, tc.err, tc.count, tc.haveCache, got, tc.want)
		}
	}
}

// nextPollDelay is the pure, deterministic core of the polling backoff: base
// interval normally, doubling from base toward max on consecutive 429s, with
// a usable server Retry-After overriding only upward.
func TestNextPollDelay(t *testing.T) {
	const base = 60 * time.Second
	const max = 300 * time.Second

	cases := []struct {
		name              string
		rateLimitFailures int
		retryAfter        time.Duration
		want              time.Duration
	}{
		{"not rate limited", 0, 0, 60 * time.Second},
		{"first 429 stays at base", 1, 0, 60 * time.Second},
		{"second 429 doubles", 2, 0, 120 * time.Second},
		{"third 429 doubles again", 3, 0, 240 * time.Second},
		{"fourth 429 caps at max", 4, 0, 300 * time.Second},
		{"far past cap stays at max", 10, 0, 300 * time.Second},
		{"server retry-after overrides upward", 1, 600 * time.Second, 600 * time.Second},
		{"server retry-after ignored when smaller than computed backoff", 2, 1 * time.Second, 120 * time.Second},
	}

	for _, tc := range cases {
		if got := nextPollDelay(base, max, tc.rateLimitFailures, tc.retryAfter); got != tc.want {
			t.Errorf("%s: nextPollDelay(%v, %v, %d, %v) = %v, want %v",
				tc.name, base, max, tc.rateLimitFailures, tc.retryAfter, got, tc.want)
		}
	}
}

// withJitter must keep its output within ±10% of the input (with a small
// margin for floating-point rounding) and must pass non-positive delays
// through unchanged — a "poll now" signal must never be perturbed negative.
func TestWithJitter(t *testing.T) {
	if got := withJitter(0); got != 0 {
		t.Errorf("withJitter(0) = %v, want 0", got)
	}
	if got := withJitter(-5 * time.Second); got != -5*time.Second {
		t.Errorf("withJitter(-5s) = %v, want unchanged -5s", got)
	}

	d := 100 * time.Second
	margin := d / 100 // 1% slack to absorb float64 rounding at the boundary
	lo := d - d/10 - margin
	hi := d + d/10 + margin
	for range 200 {
		got := withJitter(d)
		if got < lo || got > hi {
			t.Fatalf("withJitter(%v) = %v, want within [%v, %v]", d, got, lo, hi)
		}
	}
}

func TestIsOurProcess(t *testing.T) {
	// The running test binary is "claude-monitor-lite.test" — our process.
	if !isOurProcess(os.Getpid()) {
		t.Error("isOurProcess(self) = false, want true")
	}
	if isOurProcess(-1) {
		t.Error("isOurProcess(-1) = true, want false")
	}
	if isOurProcess(0) {
		t.Error("isOurProcess(0) = true, want false")
	}
	// A process that is definitely not claude-monitor-lite.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	if isOurProcess(cmd.Process.Pid) {
		t.Error("isOurProcess(non-claude process) = true, want false")
	}
}

func TestCreatePIDFileExclusive(t *testing.T) {
	old := pidFile
	pidFile = filepath.Join(t.TempDir(), "test.pid")
	defer func() { pidFile = old }()

	if err := createPIDFile(); err != nil {
		t.Fatalf("first createPIDFile failed: %v", err)
	}
	if err := createPIDFile(); err == nil {
		t.Error("second createPIDFile succeeded, want error (O_EXCL must refuse an existing file)")
	}
}
