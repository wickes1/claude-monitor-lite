package main

import (
	"encoding/json"
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
	"time"
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

// haveLastLimits must report whether a cached usage response exists, so a
// rate-limited poll knows whether it can serve stale data instead of
// falling through to the error state.
func TestHaveLastLimits(t *testing.T) {
	limitsMutex.Lock()
	old := lastLimits
	lastLimits = nil
	limitsMutex.Unlock()
	defer func() {
		limitsMutex.Lock()
		lastLimits = old
		limitsMutex.Unlock()
	}()

	if haveLastLimits() {
		t.Error("haveLastLimits() = true with nil cache, want false")
	}

	limitsMutex.Lock()
	lastLimits = &UsageLimits{}
	limitsMutex.Unlock()

	if !haveLastLimits() {
		t.Error("haveLastLimits() = false with cached limits, want true")
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

// The scoped-limit display path must label a model-scoped weekly entry with
// its display name and show its percent, for the live fixture's Fable window.
func TestFormatScopedLimitLine_ModelScoped(t *testing.T) {
	body := liveFixtureBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	limits, err := newTestClient(srv.URL, "test-org").GetUsageLimits()
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
	if extraUsageWindow(&UsageLimits{ExtraUsage: &ExtraUsageInfo{IsEnabled: false}}) != nil {
		t.Error("extraUsageWindow(disabled, nil utilization) should be nil")
	}
	util := 25.0
	w := extraUsageWindow(&UsageLimits{ExtraUsage: &ExtraUsageInfo{IsEnabled: true, Utilization: &util}})
	if w == nil || w.Utilization != 25.0 {
		t.Errorf("extraUsageWindow(enabled 25%%) = %+v, want utilization 25", w)
	}
}

// migrateStripLegacySecrets must rewrite an old plaintext config that still
// carries a Claude Code access/refresh token, dropping the secret fields
// while preserving unrelated settings.
func TestMigrateStripLegacySecrets_RemovesLegacyTokenFields(t *testing.T) {
	path := withTempConfigPath(t)

	legacyJSON := `{
		"authMode": "oauth",
		"accessToken": "sk-ant-oat01-legacy-secret",
		"refreshToken": "sk-ant-ort01-legacy-secret",
		"expiresAt": 1234567890000,
		"menuBarIndicator": "weeklyAll"
	}`
	if err := os.WriteFile(path, []byte(legacyJSON), 0600); err != nil {
		t.Fatalf("failed to seed legacy config: %v", err)
	}

	migrateStripLegacySecrets()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read migrated config: %v", err)
	}
	got := string(raw)

	for _, secret := range []string{"accessToken", "refreshToken", "expiresAt"} {
		if strings.Contains(got, secret) {
			t.Errorf("migrated config still contains %q:\n%s", secret, got)
		}
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("migrated config is not valid JSON: %v", err)
	}
	if c.AuthMode != "oauth" {
		t.Errorf("AuthMode = %q, want preserved %q", c.AuthMode, "oauth")
	}
	if c.MenuBarIndicator != "weeklyAll" {
		t.Errorf("MenuBarIndicator = %q, want preserved %q", c.MenuBarIndicator, "weeklyAll")
	}
}

// A config file that is already clean (no legacy secret fields) must not be
// rewritten at all.
func TestMigrateStripLegacySecrets_NoopOnCleanConfig(t *testing.T) {
	path := withTempConfigPath(t)

	cleanJSON := `{"authMode":"oauth","menuBarIndicator":"weeklyAll"}`
	if err := os.WriteFile(path, []byte(cleanJSON), 0600); err != nil {
		t.Fatalf("failed to seed config: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	migrateStripLegacySecrets()

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("migrateStripLegacySecrets rewrote an already-clean config file")
	}
}

// A missing config file (fresh install, never run before) must be a silent
// no-op — no panic, no file created.
func TestMigrateStripLegacySecrets_NoopOnMissingFile(t *testing.T) {
	path := withTempConfigPath(t) // points at a path that does not exist yet

	migrateStripLegacySecrets()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no config file to be created, stat err = %v", err)
	}
}
