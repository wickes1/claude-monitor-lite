package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClassifyUpdate(t *testing.T) {
	authErr := fmt.Errorf("%w: detail", ErrAuthFailed)
	transientErr := fmt.Errorf("%w: detail", ErrTransient)
	unknownErr := errors.New("something unexpected")

	cases := []struct {
		name  string
		err   error
		count int
		want  updateAction
	}{
		{"auth failure shows login immediately", authErr, 1, actionShowAuth},
		{"auth failure outranks transient count", authErr, 99, actionShowAuth},
		{"first transient keeps stale data", transientErr, 1, actionKeepStale},
		{"transient at tolerance boundary keeps stale", transientErr, maxTransientErrors, actionKeepStale},
		{"transient past tolerance shows error", transientErr, maxTransientErrors + 1, actionShowError},
		{"unknown error shows error immediately", unknownErr, 1, actionShowError},
	}

	for _, tc := range cases {
		if got := classifyUpdate(tc.err, tc.count); got != tc.want {
			t.Errorf("%s: classifyUpdate(%v, %d) = %v, want %v",
				tc.name, tc.err, tc.count, got, tc.want)
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

// A transient blip during login validation must not reject a valid key.
func TestValidateSessionRetriesTransient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/organizations") {
			_, _ = w.Write([]byte(`[{"uuid":"org-1"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10}}`))
	}))
	defer srv.Close()

	if err := validateSession(newTestClient(srv.URL, ""), 3, 0); err != nil {
		t.Fatalf("validateSession should succeed after transient blips, got: %v", err)
	}
}

// A real auth failure must be reported immediately, without retrying.
func TestValidateSessionExpiredNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := validateSession(newTestClient(srv.URL, ""), 3, 0)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d HTTP calls, want 1 (must not retry a real auth failure)", n)
	}
}

func TestValidateSessionGivesUpAfterAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := validateSession(newTestClient(srv.URL, ""), 3, 0); err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
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
		if !isValidIndicator(ind.key) {
			t.Errorf("isValidIndicator(%q) = false for a table entry", ind.key)
		}
	}
	if isValidIndicator("bogusKey") {
		t.Error("isValidIndicator accepted an unknown key")
	}
}

// getSelectedLimit must route each key to the matching window and fall back to
// the 5-hour window for an unknown key.
func TestGetSelectedLimitRouting(t *testing.T) {
	limits := &UsageLimits{
		FiveHour:         &UsageLimit{Utilization: 1},
		SevenDay:         &UsageLimit{Utilization: 2},
		SevenDayOpus:     &UsageLimit{Utilization: 4},
		SevenDaySonnet:   &UsageLimit{Utilization: 5},
		SevenDayOmelette: &UsageLimit{Utilization: 7},
	}
	cases := map[string]float64{
		"currentSession": 1,
		"weeklyAll":      2,
		"weeklyOpus":     4,
		"weeklySonnet":   5,
		"weeklyDesign":   7,
		"bogusKey":       1, // unknown -> default five_hour
	}
	for key, want := range cases {
		got := getSelectedLimit(limits, key)
		if got == nil || got.Utilization != want {
			t.Errorf("getSelectedLimit(%q) = %+v, want utilization %v", key, got, want)
		}
	}
}

// extra_usage only adapts to a window once pay-as-you-go is reporting a
// utilization; otherwise it reports nothing (nil -> "--" in the menu).
func TestExtraUsageWindow(t *testing.T) {
	if extraUsageWindow(&UsageLimits{}) != nil {
		t.Error("extraUsageWindow(no extra_usage) should be nil")
	}
	if extraUsageWindow(&UsageLimits{ExtraUsage: &ExtraUsageInfo{IsEnabled: false}}) != nil {
		t.Error("extraUsageWindow(disabled, nil utilization) should be nil")
	}
	util := 25.0
	w := extraUsageWindow(&UsageLimits{ExtraUsage: &ExtraUsageInfo{IsEnabled: true, Utilization: &util}})
	if w == nil || w.Utilization != 25.0 {
		t.Errorf("extraUsageWindow(enabled 25%%) = %+v, want utilization 25", w)
	}
}
