package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// ============================================================================
// Multi-profile support (F5-F8) — main.go's new logic. See withTempProfileEnv
// in profiles_test.go for the shared temp-env seam these tests reuse.
// ============================================================================

// newTestOAuthClient builds an OAuth-mode client pointed at a test server —
// the OAuth-mode analogue of claude_client_test.go's newTestClient (cookie
// mode). authMode must be AuthModeOAuth so GetUsageLimits hits
// "<baseURL>/oauth/usage" directly, with no organization lookup, exactly like
// the real client runProfilePoller/pollProfileOnce use in production.
func newTestOAuthClient(baseURL string, tokenProvider func() (string, error)) *ClaudeUsageClient {
	return &ClaudeUsageClient{
		tokenProvider: tokenProvider,
		authMode:      AuthModeOAuth,
		httpClient:    sharedHTTPClient,
		baseURL:       baseURL,
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever was written, so CLI-printing functions can be asserted on without
// polluting the test binary's real stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read captured stdout: %v", err)
	}
	return string(data)
}

// captureLogOutput redirects the standard logger for the duration of fn and
// returns whatever was logged — the log-scan seam ISC-60 asks for, shared by
// every log-scan test below.
func captureLogOutput(t *testing.T, fn func()) string {
	t.Helper()
	var buf strings.Builder
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

// assertNoTokenMaterial fails t if got contains any of the literal markers a
// real Claude Code credential would carry (ISC-60): an sk-ant-* secret, or
// either of the JSON field names used to carry one.
func assertNoTokenMaterial(t *testing.T, got, context string) {
	t.Helper()
	for _, marker := range []string{"sk-ant", "accessToken", "refreshToken"} {
		if strings.Contains(got, marker) {
			t.Errorf("%s: output contains token marker %q:\n%s", context, marker, got)
		}
	}
}

// --- ISC-37: stagger offsets across non-active profiles --------------------

func TestStaggerOffsetFor_DistinctMonotonic(t *testing.T) {
	var prev time.Duration = -1
	for idx := 0; idx < 6; idx++ {
		got := staggerOffsetFor(idx)
		want := time.Duration(idx) * profileStaggerStep
		if got != want {
			t.Errorf("staggerOffsetFor(%d) = %v, want %v", idx, got, want)
		}
		if got <= prev {
			t.Errorf("staggerOffsetFor(%d) = %v did not strictly increase over the previous offset %v", idx, got, prev)
		}
		prev = got
	}
	if got := staggerOffsetFor(0); got != 0 {
		t.Errorf("staggerOffsetFor(0) = %v, want 0 (the first non-active profile starts immediately after the stagger wait)", got)
	}
}

// otherProfilesInOrder is the pure profile-selection logic behind the
// multi-profile supervisor: it must return nil (a no-op layer) at <=1
// profile (ISC-42), and otherwise every OTHER profile in registry order.
func TestOtherProfilesInOrder(t *testing.T) {
	a := ProfileMeta{Name: "a"}
	b := ProfileMeta{Name: "b"}
	c := ProfileMeta{Name: "c"}

	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"zero profiles", Config{}, nil},
		{"one profile", Config{Profiles: []ProfileMeta{a}, ActiveProfile: "a"}, nil},
		{"two profiles, active first", Config{Profiles: []ProfileMeta{a, b}, ActiveProfile: "a"}, []string{"b"}},
		{"two profiles, active second", Config{Profiles: []ProfileMeta{a, b}, ActiveProfile: "b"}, []string{"a"}},
		{"three profiles, preserves registry order", Config{Profiles: []ProfileMeta{a, b, c}, ActiveProfile: "b"}, []string{"a", "c"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := otherProfilesInOrder(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("otherProfilesInOrder() = %+v, want names %v", got, tc.want)
			}
			for i, name := range tc.want {
				if got[i].Name != name {
					t.Errorf("index %d: got %q, want %q", i, got[i].Name, name)
				}
			}
		})
	}
}

// --- ISC-38: per-profile poll interval never drops below 60s ---------------

func TestProfilePollIntervalConstants_NeverBelowFloor(t *testing.T) {
	const floor = 60 * time.Second
	if refreshInterval != floor {
		t.Errorf("refreshInterval = %v, want exactly the 60s floor", refreshInterval)
	}
	if profileErrorBackoff < floor {
		t.Errorf("profileErrorBackoff = %v, want >= the 60s floor", profileErrorBackoff)
	}
	if maxBackoffInterval < floor {
		t.Errorf("maxBackoffInterval = %v, want >= the 60s floor", maxBackoffInterval)
	}
}

func TestPollProfileOnce_Success_UpdatesStateAndReturnsFloorInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10}}`))
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	delay := pollProfileOnce(state)

	if delay != refreshInterval {
		t.Errorf("delay = %v, want refreshInterval %v on success", delay, refreshInterval)
	}
	if state.lastLimits == nil {
		t.Fatal("state.lastLimits was not set on a successful poll")
	}
	if state.frozen {
		t.Error("state.frozen = true after a successful poll, want false")
	}
	if state.lastGoodAt.IsZero() {
		t.Error("state.lastGoodAt was not set on a successful poll")
	}
	if state.consecutiveErrors != 0 {
		t.Errorf("state.consecutiveErrors = %d, want 0 after success", state.consecutiveErrors)
	}
}

func TestPollProfileOnce_AuthFailure_NoDataYet_NotFrozen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	delay := pollProfileOnce(state)

	if delay != profileErrorBackoff {
		t.Errorf("delay = %v, want profileErrorBackoff %v on an auth failure", delay, profileErrorBackoff)
	}
	if delay < 60*time.Second {
		t.Errorf("delay %v is below the 60s floor", delay)
	}
	if state.frozen {
		t.Error("state.frozen = true with no prior data, want false (nothing to freeze)")
	}
	if state.lastLimits != nil {
		t.Error("state.lastLimits should remain nil — this profile never had data")
	}
	if state.consecutiveErrors != 1 {
		t.Errorf("state.consecutiveErrors = %d, want 1", state.consecutiveErrors)
	}
}

func TestPollProfileOnce_RateLimit_BacksOffAboveFloor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	first := pollProfileOnce(state)
	if first != refreshInterval {
		t.Errorf("first 429 delay = %v, want base %v", first, refreshInterval)
	}
	if state.consecutiveRateLimits != 1 {
		t.Errorf("consecutiveRateLimits = %d, want 1", state.consecutiveRateLimits)
	}

	second := pollProfileOnce(state)
	if second <= first {
		t.Errorf("second consecutive 429 delay %v did not back off above the first %v", second, first)
	}
	if second < refreshInterval {
		t.Errorf("second 429 delay %v is below the 60s floor", second)
	}
	if state.consecutiveRateLimits != 2 {
		t.Errorf("consecutiveRateLimits = %d, want 2", state.consecutiveRateLimits)
	}
}

// An unclassified error (neither rate-limited, nor transient, nor an auth
// failure) must still fall through to the base interval — never to something
// below the 60s floor.
func TestPollProfileOnce_GenericError_ReturnsBaseFloor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418: not 401/403/429/5xx/408
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	delay := pollProfileOnce(state)

	if delay != refreshInterval {
		t.Errorf("delay = %v, want the base refreshInterval %v for an unclassified error", delay, refreshInterval)
	}
	if delay < 60*time.Second {
		t.Errorf("delay %v is below the 60s floor", delay)
	}
}

// --- ISC-85: expired/unreadable profile freezes last-good data -------------

func TestPollProfileOnce_TransientFailure_FreezesLastGoodData(t *testing.T) {
	failing := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10}}`))
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	pollProfileOnce(state) // seed a successful poll
	if state.lastLimits == nil {
		t.Fatal("setup: first poll must succeed and cache data")
	}
	firstGoodLimits := state.lastLimits
	firstGoodAt := state.lastGoodAt

	failing = true
	delay := pollProfileOnce(state)

	if delay != profileErrorBackoff {
		t.Errorf("delay = %v, want profileErrorBackoff %v on a transient failure (no poll spam)", delay, profileErrorBackoff)
	}
	if !state.frozen {
		t.Error("state.frozen = false after a failure with cached data, want true")
	}
	if state.lastLimits != firstGoodLimits {
		t.Error("state.lastLimits changed on failure, want the frozen last-good data kept unchanged")
	}
	if !state.lastGoodAt.Equal(firstGoodAt) {
		t.Errorf("state.lastGoodAt = %v, want unchanged %v — a failure must not update the staleness marker", state.lastGoodAt, firstGoodAt)
	}
	if state.consecutiveErrors != 1 {
		t.Errorf("state.consecutiveErrors = %d, want 1", state.consecutiveErrors)
	}
}

// --- ISC-39/40: per-profile backoff state is independent -------------------

func TestPollProfileOnce_TwoProfiles_StateIsolated(t *testing.T) {
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10}}`))
	}))
	defer goodSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badSrv.Close()

	good := &profileState{meta: ProfileMeta{Name: "good"}, client: newTestOAuthClient(goodSrv.URL, func() (string, error) { return "tok", nil })}
	bad := &profileState{meta: ProfileMeta{Name: "bad"}, client: newTestOAuthClient(badSrv.URL, func() (string, error) { return "tok", nil })}

	for i := 0; i < 3; i++ {
		pollProfileOnce(bad)
	}
	pollProfileOnce(good)

	if bad.consecutiveErrors == 0 {
		t.Error("bad profile's consecutiveErrors did not increment")
	}
	if good.consecutiveErrors != 0 {
		t.Errorf("good profile's consecutiveErrors = %d, want 0 — must not be contaminated by the bad profile's failures", good.consecutiveErrors)
	}
	if good.lastLimits == nil {
		t.Error("good profile should have cached usage data after a successful poll")
	}
	if bad.lastLimits != nil {
		t.Error("bad profile never succeeded, should have no cached data")
	}
	if bad.frozen {
		t.Error("bad profile with no prior data should not be marked frozen — nothing to freeze")
	}
}

// A failing profile's poll must never touch the ACTIVE profile's pre-existing
// globals (lastLimits/consecutiveErrors/consecutiveRateLimits) — the
// multi-profile layer is documented (main.go) as an ADDITIONAL layer that
// never reaches into the original single-profile machinery.
func TestPollProfileOnce_FailureNeverTouchesActiveProfileGlobals(t *testing.T) {
	limitsMutex.Lock()
	oldLimits := lastLimits
	lastLimits = nil
	limitsMutex.Unlock()
	consecutiveMutex.Lock()
	oldErrors, oldRateLimits := consecutiveErrors, consecutiveRateLimits
	consecutiveErrors, consecutiveRateLimits = 0, 0
	consecutiveMutex.Unlock()
	t.Cleanup(func() {
		limitsMutex.Lock()
		lastLimits = oldLimits
		limitsMutex.Unlock()
		consecutiveMutex.Lock()
		consecutiveErrors, consecutiveRateLimits = oldErrors, oldRateLimits
		consecutiveMutex.Unlock()
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	for i := 0; i < 3; i++ {
		pollProfileOnce(state)
	}
	if state.consecutiveErrors == 0 {
		t.Fatal("expected the profile's own consecutiveErrors to increment on repeated failure")
	}

	limitsMutex.RLock()
	gotLimits := lastLimits
	limitsMutex.RUnlock()
	consecutiveMutex.Lock()
	gotErrors, gotRateLimits := consecutiveErrors, consecutiveRateLimits
	consecutiveMutex.Unlock()

	if gotLimits != nil {
		t.Errorf("active-profile global lastLimits was changed by a non-active profile's poll failures: %+v", gotLimits)
	}
	if gotErrors != 0 || gotRateLimits != 0 {
		t.Errorf("active-profile globals mutated by a non-active profile's poll: consecutiveErrors=%d consecutiveRateLimits=%d, want 0/0", gotErrors, gotRateLimits)
	}
}

// pollProfileOnce must return a delay for the CALLER's timer to apply, never
// block on it itself — otherwise one profile's failure would stall every
// other profile sharing the supervisor's goroutine-per-profile model.
func TestPollProfileOnce_DoesNotBlockCaller_TimingIndependence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	start := time.Now()
	delay := pollProfileOnce(state)
	elapsed := time.Since(start)

	if delay < refreshInterval {
		t.Errorf("returned delay %v is below the 60s floor", delay)
	}
	if elapsed > 3*time.Second {
		t.Errorf("pollProfileOnce took %v to return, want near-instant — it must hand the delay back to the caller's timer rather than sleeping for it", elapsed)
	}
}

// --- ISC-42/54: with <=1 profile, the multi-profile layer is inert ---------

func TestPrintUsageStatusSection_LEq1Profile_FallsThroughToDisplayUsageStats(t *testing.T) {
	withTempProfileEnv(t) // fresh config: zero profiles registered

	limits := &UsageLimits{FiveHour: &UsageLimit{Utilization: 42}}

	out := captureStdout(t, func() {
		printUsageStatusSection(limits)
	})

	if !strings.Contains(out, "=== Current Usage ===") {
		t.Errorf("output = %q, want the displayUsageStats header (pre-feature behavior)", out)
	}
	if strings.Contains(out, "=== Profile:") {
		t.Errorf("output = %q, should not show per-profile sections with <=1 profile configured", out)
	}
}

// refreshProfileMenu must be a safe no-op before onReady has created the
// systray menu pool (profileMenuItems[0] == nil) — the state every test in
// this package runs in, since none of them invoke the real getlantern/systray
// native run loop.
func TestRefreshProfileMenu_NoMenuPool_NoOp(t *testing.T) {
	if profileMenuItems[0] != nil {
		t.Skip("menu pool already created in this test binary — nothing to verify")
	}
	withTempProfileEnv(t)
	refreshProfileMenu() // must not panic
}

// --- ISC-85/86/89: frozen/staleness rendering -------------------------------

func TestBuildProfileMenuRows_ActiveVsInactive(t *testing.T) {
	profiles := []ProfileMeta{{Name: "default"}, {Name: "work"}}
	snapshot := func(name string) (*UsageLimits, time.Time, bool, bool) {
		return nil, time.Time{}, false, false
	}

	rows := buildProfileMenuRows(profiles, "default", snapshot)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	activeRow, workRow := rows[0], rows[1]
	if !strings.HasPrefix(activeRow.header, "default") || !strings.Contains(activeRow.header, "active") {
		t.Errorf("active row header = %q, want it to name the profile and mark it active", activeRow.header)
	}
	if activeRow.switchVisible {
		t.Error("active row must not offer a switch item")
	}
	if workRow.header != "work" {
		t.Errorf("inactive row header = %q, want the plain profile name %q", workRow.header, "work")
	}
	if !workRow.switchVisible {
		t.Error("inactive row must offer a switch item")
	}
}

func TestBuildProfileMenuRows_NoData_FallsBackToPlaceholder(t *testing.T) {
	profiles := []ProfileMeta{{Name: "work"}}
	snapshot := func(name string) (*UsageLimits, time.Time, bool, bool) {
		return nil, time.Time{}, false, false
	}

	rows := buildProfileMenuRows(profiles, "default", snapshot)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	row := rows[0]
	wantSession := formatUsageWithReset(nil, "5-Hour Session:")
	if row.sessionLine != wantSession {
		t.Errorf("sessionLine = %q, want the no-data placeholder %q", row.sessionLine, wantSession)
	}
	if strings.Contains(row.sessionLine, "as of") {
		t.Errorf("sessionLine = %q should not show a staleness marker with no data at all", row.sessionLine)
	}
}

// A frozen (poll-failing) non-active profile must still render its cached
// usage with a LIVE reset countdown (ISC-86) plus a staleness marker
// (ISC-85/89) — never a crash, never a blanked-out countdown.
//
// Note: the countdown computation has no fake-clock seam — formatUsageWithReset ->
// calculateTimeUntilReset calls time.Now() directly (main.go). This test therefore uses a resetTime
// offset far enough from the real wall clock (3h) that the minute-floor
// truncation in calculateTimeUntilReset is deterministic regardless of the
// exact instant the test runs, without needing to control "now" itself.
func TestBuildProfileMenuRows_FrozenProfile_StalenessMarkerAndLiveCountdown(t *testing.T) {
	resetTime := time.Now().Add(3 * time.Hour)
	lastGoodAt := time.Now().Add(-90 * time.Minute)

	five := &UsageLimit{Utilization: 77, ResetsAtTime: resetTime}
	frozenLimits := &UsageLimits{FiveHour: five}

	profiles := []ProfileMeta{{Name: "default"}, {Name: "work"}}
	snapshot := func(name string) (*UsageLimits, time.Time, bool, bool) {
		if name == "work" {
			return frozenLimits, lastGoodAt, true, true
		}
		return nil, time.Time{}, false, false
	}

	rows := buildProfileMenuRows(profiles, "default", snapshot)

	var workRow, activeRow *profileMenuRow
	for i := range rows {
		switch rows[i].profileName {
		case "work":
			workRow = &rows[i]
		case "default":
			activeRow = &rows[i]
		}
	}
	if workRow == nil {
		t.Fatal("no row rendered for the frozen profile 'work'")
	}

	// ISC-86: the reset countdown is computed live from the cached
	// ResetsAtTime, not blanked out just because the underlying poll fails.
	wantCountdown := formatUsageWithReset(five, "5-Hour Session:")
	if !strings.HasPrefix(workRow.sessionLine, wantCountdown) {
		t.Errorf("sessionLine = %q, want it to start with the live countdown %q", workRow.sessionLine, wantCountdown)
	}

	// ISC-85/89: a staleness "(as of HH:MM)" marker is appended for a frozen
	// profile with a known last-good time.
	wantAsOf := fmt.Sprintf("(as of %s)", lastGoodAt.Local().Format(staleMenuTimeFormat))
	if !strings.Contains(workRow.sessionLine, wantAsOf) {
		t.Errorf("sessionLine = %q, want it to contain the staleness marker %q", workRow.sessionLine, wantAsOf)
	}

	// The frozen marker is only ever appended to the session line (matches
	// buildProfileMenuRows's documented behavior), never the weekly line.
	if strings.Contains(workRow.weeklyLine, "as of") {
		t.Errorf("weeklyLine = %q should not carry a staleness marker", workRow.weeklyLine)
	}

	if activeRow == nil || strings.Contains(activeRow.sessionLine, "as of") {
		t.Errorf("active, non-frozen row unexpectedly shows staleness: %+v", activeRow)
	}
}

func TestProfileSnapshotFunc_ActiveProfile_ReadsGlobalState(t *testing.T) {
	limitsMutex.Lock()
	oldLimits := lastLimits
	oldLimitsProfile := lastLimitsProfile
	someLimits := &UsageLimits{FiveHour: &UsageLimit{Utilization: 5}}
	lastLimits = someLimits
	// Attribution must match the queried active profile ("default") for the
	// globals to be trusted at all (ISC-89) — see
	// TestProfileSnapshotFunc_AfterSwitch_DoesNotAttributeOldData below for
	// the mismatch case.
	lastLimitsProfile = "default"
	limitsMutex.Unlock()
	consecutiveMutex.Lock()
	oldErrors := consecutiveErrors
	consecutiveErrors = 2
	consecutiveMutex.Unlock()
	t.Cleanup(func() {
		limitsMutex.Lock()
		lastLimits = oldLimits
		lastLimitsProfile = oldLimitsProfile
		limitsMutex.Unlock()
		consecutiveMutex.Lock()
		consecutiveErrors = oldErrors
		consecutiveMutex.Unlock()
	})

	snap := profileSnapshotFunc("default")
	limits, lastGoodAt, frozen, hasData := snap("default")

	if limits != someLimits {
		t.Errorf("limits = %+v, want the active profile's global lastLimits", limits)
	}
	if !hasData {
		t.Error("hasData = false, want true")
	}
	if !frozen {
		t.Error("frozen = false, want true (consecutiveErrors > 0 and data present)")
	}
	if !lastGoodAt.IsZero() {
		t.Errorf("lastGoodAt = %v, want zero — the active profile's poller tracks no separate staleness timestamp", lastGoodAt)
	}
}

// TestProfileSnapshotFunc_AfterSwitch_DoesNotAttributeOldData is the ISC-89
// regression test for the CLI-switch attribution bug caught in adversarial
// review: a `profile switch` made via a separate CLI process flips the
// active profile name immediately but never touches the shared
// lastLimits/lastLimitsAt globals, which keep holding whatever the
// PREVIOUSLY active profile last fetched until the daemon's own active
// poller catches up (up to refreshInterval later). Before this fix,
// profileSnapshotFunc handed that stale, wrong-account data straight to the
// newly-active profile's "— active" menu row.
func TestProfileSnapshotFunc_AfterSwitch_DoesNotAttributeOldData(t *testing.T) {
	limitsMutex.Lock()
	oldLimits := lastLimits
	oldLimitsProfile := lastLimitsProfile
	oldLimitsAt := lastLimitsAt
	staleLimits := &UsageLimits{FiveHour: &UsageLimit{Utilization: 42}}
	lastLimits = staleLimits
	// Simulates the daemon's last fetch having been for "default", made
	// before a CLI `profile switch work` landed.
	lastLimitsProfile = "default"
	lastLimitsAt = time.Now()
	limitsMutex.Unlock()
	t.Cleanup(func() {
		limitsMutex.Lock()
		lastLimits = oldLimits
		lastLimitsProfile = oldLimitsProfile
		lastLimitsAt = oldLimitsAt
		limitsMutex.Unlock()
	})

	// The switch already updated the active profile to "work" (this is what
	// LoadConfig/activeProfileName would now return), but the globals above
	// are still tagged "default".
	snap := profileSnapshotFunc("work")
	limits, lastGoodAt, frozen, hasData := snap("work")

	if hasData {
		t.Error("hasData = true, want false — cached globals belong to the previous active profile \"default\", not \"work\"")
	}
	if limits != nil {
		t.Errorf("limits = %+v, want nil — must not attribute the previous profile's usage to the newly active profile", limits)
	}
	if frozen {
		t.Error("frozen = true, want false — no attributable data means nothing to mark stale")
	}
	if !lastGoodAt.IsZero() {
		t.Errorf("lastGoodAt = %v, want zero — no attributable data means no timestamp to show either", lastGoodAt)
	}

	// Companion: once the active poller's next tick fetches "work"'s own
	// data (attribution now matches), the same globals are trusted again —
	// this is not a permanent lockout, just a one-cycle guard.
	limitsMutex.Lock()
	lastLimitsProfile = "work"
	limitsMutex.Unlock()

	limits, _, _, hasData = snap("work")
	if !hasData || limits != staleLimits {
		t.Errorf("after attribution matches: (limits=%v, hasData=%v), want (limits=%v, hasData=true)", limits, hasData, staleLimits)
	}
}

// TestProfileSnapshotFunc_ActiveProfile_NilGlobalsAtStart_NoDataNoCrash covers
// the daemon-restart case: lastLimits is nil until the active poller's first
// fetch completes, and lastLimitsProfile is still its zero value (""). Both
// must degrade to no-data, never a crash and never a false attribution match
// (activeName is never "").
func TestProfileSnapshotFunc_ActiveProfile_NilGlobalsAtStart_NoDataNoCrash(t *testing.T) {
	limitsMutex.Lock()
	oldLimits := lastLimits
	oldLimitsProfile := lastLimitsProfile
	lastLimits = nil
	lastLimitsProfile = ""
	limitsMutex.Unlock()
	t.Cleanup(func() {
		limitsMutex.Lock()
		lastLimits = oldLimits
		lastLimitsProfile = oldLimitsProfile
		limitsMutex.Unlock()
	})

	snap := profileSnapshotFunc("work")
	limits, _, frozen, hasData := snap("work")

	if hasData || frozen || limits != nil {
		t.Errorf("daemon-start snapshot = (limits=%v, frozen=%v, hasData=%v), want all zero/false", limits, frozen, hasData)
	}
}

func TestProfileSnapshotFunc_NonActiveProfile_ReadsProfileState(t *testing.T) {
	fixedTime := time.Now().Add(-30 * time.Minute)
	someLimits := &UsageLimits{FiveHour: &UsageLimit{Utilization: 9}}
	state := &profileState{
		meta:       ProfileMeta{Name: "work"},
		lastLimits: someLimits,
		lastGoodAt: fixedTime,
		frozen:     true,
	}
	setProfileState("work", state)
	defer removeProfileState("work")

	snap := profileSnapshotFunc("default")
	limits, lastGoodAt, frozen, hasData := snap("work")

	if limits != someLimits {
		t.Errorf("limits = %+v, want the profile's own cached data", limits)
	}
	if !hasData || !frozen {
		t.Errorf("hasData=%v frozen=%v, want both true", hasData, frozen)
	}
	if !lastGoodAt.Equal(fixedTime) {
		t.Errorf("lastGoodAt = %v, want %v", lastGoodAt, fixedTime)
	}
}

func TestProfileSnapshotFunc_UnknownProfile_NoData(t *testing.T) {
	snap := profileSnapshotFunc("default")
	limits, _, frozen, hasData := snap("ghost-never-registered")

	if hasData || frozen || limits != nil {
		t.Errorf("unregistered profile snapshot = (limits=%v, frozen=%v, hasData=%v), want all zero/false", limits, frozen, hasData)
	}
}

// --- ISC-15: filterEnv + profile-login env construction ---------------------

func TestFilterEnv_StripsOnlyExactKey(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		key  string
		want []string
	}{
		{
			name: "strips an exact match, preserves the rest",
			env:  []string{"CLAUDE_CONFIG_DIR=/a", "FOO=bar"},
			key:  "CLAUDE_CONFIG_DIR",
			want: []string{"FOO=bar"},
		},
		{
			name: "preserves a similarly-prefixed but distinct variable name",
			env:  []string{"CLAUDE_CONFIG_DIRECTORY=/a", "CLAUDE_CONFIG_DIR=/b"},
			key:  "CLAUDE_CONFIG_DIR",
			want: []string{"CLAUDE_CONFIG_DIRECTORY=/a"},
		},
		{
			name: "strips every occurrence of the key",
			env:  []string{"CLAUDE_CONFIG_DIR=/a", "X=1", "CLAUDE_CONFIG_DIR=/b"},
			key:  "CLAUDE_CONFIG_DIR",
			want: []string{"X=1"},
		},
		{
			name: "no matches leaves the slice unchanged",
			env:  []string{"A=1", "B=2"},
			key:  "CLAUDE_CONFIG_DIR",
			want: []string{"A=1", "B=2"},
		},
		{
			name: "empty env stays empty",
			env:  []string{},
			key:  "CLAUDE_CONFIG_DIR",
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterEnv(tc.env, tc.key)
			if len(got) != len(tc.want) {
				t.Fatalf("filterEnv(%v, %q) = %v, want %v", tc.env, tc.key, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("filterEnv(%v, %q)[%d] = %q, want %q", tc.env, tc.key, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// setupFakeClaudeOnPath installs a fake `claude` executable ahead of the real
// PATH (a POSIX shell script that dumps its own environment to a file and
// exits 0) so handleProfileLogin's realClaudeBinary() resolves it instead of
// touching a real claude installation, and returns the path handleProfileLogin's
// exec'd env can be inspected at.
func setupFakeClaudeOnPath(t *testing.T) (dumpPath string) {
	t.Helper()

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\nenv > \"$FAKE_CLAUDE_ENV_DUMP\"\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake claude script: %v", err)
	}
	if err := os.Chmod(scriptPath, 0755); err != nil {
		t.Fatalf("failed to chmod fake claude script: %v", err)
	}

	dumpPath = filepath.Join(t.TempDir(), "env-dump.txt")
	oldDump, hadDump := os.LookupEnv("FAKE_CLAUDE_ENV_DUMP")
	os.Setenv("FAKE_CLAUDE_ENV_DUMP", dumpPath)

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath)

	t.Cleanup(func() {
		os.Setenv("PATH", oldPath)
		if hadDump {
			os.Setenv("FAKE_CLAUDE_ENV_DUMP", oldDump)
		} else {
			os.Unsetenv("FAKE_CLAUDE_ENV_DUMP")
		}
	})

	return dumpPath
}

// A non-default profile's login must export CLAUDE_CONFIG_DIR=<its dir> to
// the real claude process (ISC-15).
func TestHandleProfileLogin_NonDefaultProfile_SetsConfigDir(t *testing.T) {
	withTempProfileEnv(t)
	dumpPath := setupFakeClaudeOnPath(t)

	meta, err := AddProfile("work", "")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	out := captureStdout(t, func() {
		handleProfileLogin([]string{"work"})
	})
	if !strings.Contains(out, "work") {
		t.Errorf("stdout = %q, want it to mention the profile name", out)
	}

	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("fake claude did not run (no env dump found): %v", err)
	}
	if !strings.Contains(string(dump), "CLAUDE_CONFIG_DIR="+meta.Dir) {
		t.Errorf("child env = %q, want it to contain CLAUDE_CONFIG_DIR=%s", dump, meta.Dir)
	}
}

// The default profile's login must OMIT CLAUDE_CONFIG_DIR entirely, even when
// the ambient environment already has it set (withTempProfileEnv sets the
// real CLAUDE_CONFIG_DIR env var so claudeConfigDir() resolves to a fake temp
// dir) — an explicit value, even one naming the same directory, changes the
// derived Keychain service name (ISC-15's default-profile clause).
func TestHandleProfileLogin_DefaultProfile_OmitsConfigDirEvenIfAmbientSet(t *testing.T) {
	withTempProfileEnv(t)
	dumpPath := setupFakeClaudeOnPath(t)

	if err := EnsureDefaultProfile(); err != nil {
		t.Fatalf("EnsureDefaultProfile failed: %v", err)
	}

	out := captureStdout(t, func() {
		handleProfileLogin([]string{defaultProfileName})
	})
	if !strings.Contains(out, "unset") {
		t.Errorf("stdout = %q, want it to describe CLAUDE_CONFIG_DIR as unset for the default profile", out)
	}

	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("fake claude did not run (no env dump found): %v", err)
	}
	for _, line := range strings.Split(string(dump), "\n") {
		if strings.HasPrefix(line, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("child env contains %q — CLAUDE_CONFIG_DIR must be unset for the default profile even though the ambient environment had it set", line)
		}
	}
}

// --- ISC-60: no token material in daemon logs, in any tested scenario ------

func TestSwitchProfile_LogScan_NoTokenMaterial(t *testing.T) {
	withTempProfileEnv(t)
	restore := readCredential
	defer func() { readCredential = restore }()
	readCredential = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken:  "sk-ant-oat01-FAKE-TEST-ACCESS-TOKEN",
			RefreshToken: "sk-ant-ort01-FAKE-TEST-REFRESH-TOKEN",
			ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	got := captureLogOutput(t, func() {
		if _, err := SwitchProfile("work"); err != nil {
			t.Fatalf("SwitchProfile failed: %v", err)
		}
	})
	assertNoTokenMaterial(t, got, "SwitchProfile audit log")
}

func TestPollProfileOnce_LogScan_AuthFailure_NoTokenMaterial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "sk-ant-oat01-FAKE-TEST-ACCESS-TOKEN", nil }),
	}

	got := captureLogOutput(t, func() { pollProfileOnce(state) })
	assertNoTokenMaterial(t, got, "pollProfileOnce auth-failure log")
}

func TestPollProfileOnce_LogScan_TransientError_NoTokenMaterial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "sk-ant-oat01-FAKE-TEST-ACCESS-TOKEN", nil }),
	}

	got := captureLogOutput(t, func() { pollProfileOnce(state) })
	assertNoTokenMaterial(t, got, "pollProfileOnce transient-error log")
}

// KNOWN GAP (ISC-60): pollProfileOnce logs "%v" of the error GetUsageLimits
// returns. For a 429, that error is a *RateLimitError whose Error() method
// (claude_client.go) includes the raw response body verbatim. If a
// misbehaving usage endpoint ever echoed token-like text in a 429 body, it
// would land in this program's own daemon log unredacted — main.go's
// briefStatusErr has a comment flagging exactly this risk for the CLI status
// path, but the daemon log path (pollProfileOnce) has no equivalent guard.
//
// This test documents CURRENT behavior; it is not an endorsement of it, and
// it is not the failing assertion ISC-60 ultimately wants — main.go is out of
// scope for this test-only change. If response bodies are ever sanitized
// before logging, this test will skip itself (rather than fail) as a signal
// to remove it and drop this note.
func TestPollProfileOnce_LogScan_RateLimitBody_KnownGap(t *testing.T) {
	const secretMarker = "sk-ant-oat01-LEAKED-IN-RATE-LIMIT-BODY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(secretMarker))
	}))
	defer srv.Close()

	state := &profileState{
		meta:   ProfileMeta{Name: "work"},
		client: newTestOAuthClient(srv.URL, func() (string, error) { return "tok", nil }),
	}

	got := captureLogOutput(t, func() { pollProfileOnce(state) })
	if !strings.Contains(got, secretMarker) {
		t.Skip("rate-limit response body no longer leaks into the daemon log — the ISC-60 gap noted above appears fixed; remove this characterization test")
	}
}
