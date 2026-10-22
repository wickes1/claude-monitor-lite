package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// The indicators table is the single source of truth for menu items, config
// validation and selection — every key must be unique and non-empty, with a
// getter, or those layers silently break.
func TestIndicatorsTableConsistent(t *testing.T) {
	if len(indicators) == 0 {
		t.Fatal("indicators table is empty")
	}
	seen := make(map[string]bool)
	for _, ind := range indicators {
		if ind.key == "" || ind.label == "" || ind.get == nil {
			t.Errorf("indicator %q has an empty field: %+v", ind.key, ind)
		}
		if seen[ind.key] {
			t.Errorf("duplicate indicator key %q", ind.key)
		}
		seen[ind.key] = true
		if _, ok := findIndicator(ind.key); !ok {
			t.Errorf("findIndicator(%q) not found for a table entry", ind.key)
		}
	}
	if _, ok := findIndicator("bogusKey"); ok {
		t.Error("findIndicator accepted an unknown key")
	}
}

// Each key must route to its own window, and an unknown key (e.g. a metric
// that was removed) must fall back to the 5-hour window.
func TestFindIndicatorRouting(t *testing.T) {
	extra := 3.0
	limits := &UsageLimits{
		FiveHour:   &UsageLimit{Utilization: 1},
		SevenDay:   &UsageLimit{Utilization: 2},
		ExtraUsage: &ExtraUsageInfo{Utilization: &extra},
	}
	cases := map[string]float64{
		"currentSession": 1,
		"weeklyAll":      2,
		"extraUsage":     3,
		"bogusKey":       1,
	}
	for key, want := range cases {
		ind, _ := findIndicator(key)
		if got := ind.get(limits); got == nil || got.Utilization != want {
			t.Errorf("findIndicator(%q).get = %+v, want utilization %v", key, got, want)
		}
	}
}

// The scoped-limit display path must label a model-scoped weekly entry with
// its display name and show its percent, for the live shape's Fable window.
func TestFormatScopedLimitLine_ModelScoped(t *testing.T) {
	limits, err := serveBody(t, http.StatusOK, liveShapeBody).GetUsageLimits()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	var scopedLines []string
	for _, entry := range limits.Limits {
		if entry.Scope != nil {
			scopedLines = append(scopedLines, formatScopedLimitLine(entry))
		}
	}
	if len(scopedLines) != 1 {
		t.Fatalf("got %d scoped lines, want 1: %v", len(scopedLines), scopedLines)
	}
	line := scopedLines[0]
	if !strings.Contains(line, "Fable") {
		t.Errorf("scoped line %q does not contain %q", line, "Fable")
	}
	if !strings.Contains(line, "12%") {
		t.Errorf("scoped line %q does not contain %q", line, "12%")
	}
}

// extra_usage only adapts to a window once pay-as-you-go is reporting a
// utilization; otherwise it reports nothing (nil -> "--" in the menu).
func TestExtraUsageWindow(t *testing.T) {
	if extraUsageWindow(&UsageLimits{}) != nil {
		t.Error("extraUsageWindow(no extra_usage) should be nil")
	}
	if extraUsageWindow(&UsageLimits{ExtraUsage: &ExtraUsageInfo{}}) != nil {
		t.Error("extraUsageWindow(nil utilization) should be nil")
	}
	util := 25.0
	w := extraUsageWindow(&UsageLimits{ExtraUsage: &ExtraUsageInfo{Utilization: &util}})
	if w == nil || w.Utilization != 25.0 {
		t.Errorf("extraUsageWindow(enabled 25%%) = %+v, want utilization 25", w)
	}
}

// The status command shows what the daemon last fetched, read back from the
// usage cache; the round trip must keep everything the display uses.
func TestUsageCacheRoundTrip(t *testing.T) {
	limits, err := serveBody(t, http.StatusOK, liveShapeBody).GetUsageLimits()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	var got *UsageLimits
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	render := func(u *UsageLimits) string {
		var b strings.Builder
		for _, ind := range indicators {
			b.WriteString(formatConsoleUsage(ind.get(u), ind.label, ""))
		}
		for _, entry := range u.Limits {
			b.WriteString(formatScopedLimitLine(entry))
		}
		return b.String()
	}
	if got, want := render(got), render(limits); got != want {
		t.Errorf("round trip changed the displayed usage:\n got %s\nwant %s", got, want)
	}
}
