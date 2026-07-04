package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestClient builds a client pointed at a test server. It is given its own
// cookie jar so refreshSessionFromJar does not panic.
func newTestClient(baseURL, orgID string) *ClaudeUsageClient {
	jar, _ := cookiejar.New(nil)
	return &ClaudeUsageClient{
		sessionKey:     "test-session-key",
		organizationID: orgID,
		httpClient:     &http.Client{Jar: jar, Timeout: requestTimeout},
		baseURL:        baseURL,
	}
}

// A 200 OK whose body is not JSON (a Cloudflare interstitial / edge error page)
// must be treated as transient so the menu bar tolerates it under the 3-strike
// window instead of immediately flashing "Error".
func TestGetUsageLimits_NonJSON200_IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!DOCTYPE html><html>Just a moment...</html>"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "test-org")
	_, err := c.GetUsageLimits()
	if err == nil {
		t.Fatal("expected an error for non-JSON 200 response, got nil")
	}
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

// An empty 200 body (truncated response / network hiccup) must also be transient.
func TestGetUsageLimits_Empty200_IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "test-org")
	_, err := c.GetUsageLimits()
	if err == nil {
		t.Fatal("expected an error for empty 200 response, got nil")
	}
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

// A 5xx while fetching the organization ID must surface as transient, not as
// an unknown hard error that flashes "Error" on the first failure.
func TestGetUsageLimits_OrgFetch5xx_IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "") // empty org ID forces fetchOrganizationID
	_, err := c.GetUsageLimits()
	if err == nil {
		t.Fatal("expected an error when org ID fetch fails, got nil")
	}
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

// A 401 while fetching the organization ID is a real auth failure and must be
// reported as such (so the menu bar shows "Login", not "Error").
func TestGetUsageLimits_OrgFetch401_IsAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "")
	_, err := c.GetUsageLimits()
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got: %v", err)
	}
}

// Regression guard: a well-formed response still parses correctly.
func TestGetUsageLimits_ValidJSON_Succeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-05-20T03:00:00Z"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "test-org")
	limits, err := c.GetUsageLimits()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if limits.FiveHour == nil || roundUtilization(limits.FiveHour.Utilization) != 43 {
		t.Fatalf("unexpected limits: %+v", limits)
	}
}

// The struct must capture every field the live usage API returns so a response
// is never silently truncated. This body is the full 2026-05 response shape.
func TestGetUsageLimits_FullAPIResponse_AllFieldsParsed(t *testing.T) {
	body := `{"five_hour":{"utilization":68.0,"resets_at":"2026-05-20T20:00:01.190481+00:00"},` +
		`"seven_day":{"utilization":55.0,"resets_at":"2026-05-22T01:00:00.190511+00:00"},` +
		`"seven_day_oauth_apps":null,"seven_day_opus":null,` +
		`"seven_day_sonnet":{"utilization":38.0,"resets_at":"2026-05-22T01:00:00.190521+00:00"},` +
		`"seven_day_cowork":null,"seven_day_omelette":{"utilization":0.0,"resets_at":null},` +
		`"tangelo":null,"iguana_necktie":null,"omelette_promotional":null,` +
		`"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null,` +
		`"utilization":null,"currency":null,"disabled_reason":null}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	limits, err := newTestClient(srv.URL, "test-org").GetUsageLimits()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	// Active windows: present, with utilization and a parsed reset time.
	if limits.FiveHour == nil || roundUtilization(limits.FiveHour.Utilization) != 68 {
		t.Errorf("FiveHour = %+v, want utilization 68", limits.FiveHour)
	}
	if limits.FiveHour.ResetsAtTime.IsZero() {
		t.Error("FiveHour.ResetsAtTime was not parsed")
	}
	if limits.SevenDay == nil || roundUtilization(limits.SevenDay.Utilization) != 55 {
		t.Errorf("SevenDay = %+v, want utilization 55", limits.SevenDay)
	}
	if limits.SevenDaySonnet == nil || roundUtilization(limits.SevenDaySonnet.Utilization) != 38 {
		t.Errorf("SevenDaySonnet = %+v, want utilization 38", limits.SevenDaySonnet)
	}
	// A present window with a null resets_at must still parse.
	if limits.SevenDayOmelette == nil || limits.SevenDayOmelette.ResetsAt != nil {
		t.Errorf("SevenDayOmelette = %+v, want present with null resets_at", limits.SevenDayOmelette)
	}

	// Placeholder windows returned as null must unmarshal to nil pointers.
	if limits.SevenDayOAuthApps != nil || limits.SevenDayOpus != nil ||
		limits.SevenDayCowork != nil || limits.Tangelo != nil ||
		limits.IguanaNecktie != nil || limits.OmelettePromotional != nil {
		t.Error("null placeholder windows should unmarshal to nil")
	}

	// Pay-as-you-go object: present, disabled, all detail fields null.
	if limits.ExtraUsage == nil {
		t.Fatal("ExtraUsage should be present")
	}
	if limits.ExtraUsage.IsEnabled {
		t.Error("ExtraUsage.IsEnabled = true, want false")
	}
	if limits.ExtraUsage.MonthlyLimit != nil || limits.ExtraUsage.UsedCredits != nil ||
		limits.ExtraUsage.Utilization != nil || limits.ExtraUsage.Currency != nil ||
		limits.ExtraUsage.DisabledReason != nil {
		t.Errorf("ExtraUsage detail fields should be nil when disabled: %+v", limits.ExtraUsage)
	}
}

// liveUsageResponseBody mirrors, key for key, a response captured live from
// the usage endpoint on 2026-07-01 — the full current shape: dollar fields on
// windows, the nimbus_quill/cinder_cove/amber_ladder placeholder windows, the
// extra_usage decimal_places/daily/weekly additions, the limits[] array with a
// model-scoped entry, and the spend object. Utilization values and timestamps
// are synthetic; only the shape is real.
const liveUsageResponseBody = `{
  "five_hour": {"utilization": 47.0, "resets_at": "2026-07-02T03:00:00.000001+00:00", "limit_dollars": null, "used_dollars": null, "remaining_dollars": null},
  "seven_day": {"utilization": 9.0, "resets_at": "2026-07-03T01:00:00.000001+00:00", "limit_dollars": null, "used_dollars": null, "remaining_dollars": null},
  "seven_day_oauth_apps": null,
  "seven_day_opus": null,
  "seven_day_sonnet": null,
  "seven_day_cowork": null,
  "seven_day_omelette": null,
  "tangelo": null,
  "iguana_necktie": null,
  "omelette_promotional": null,
  "nimbus_quill": null,
  "cinder_cove": null,
  "amber_ladder": null,
  "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null, "currency": null, "decimal_places": null, "disabled_reason": null, "daily": null, "weekly": null},
  "limits": [
    {"kind": "session", "group": "session", "percent": 47, "severity": "normal", "resets_at": "2026-07-02T03:00:00.000001+00:00", "scope": null, "is_active": true},
    {"kind": "weekly_all", "group": "weekly", "percent": 9, "severity": "normal", "resets_at": "2026-07-03T01:00:00.000001+00:00", "scope": null, "is_active": false},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 12, "severity": "normal", "resets_at": "2026-07-03T01:00:00.000002+00:00", "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
  ],
  "spend": {
    "used": {"amount_minor": 0, "currency": "USD", "exponent": 2},
    "limit": null,
    "percent": 0,
    "severity": "normal",
    "enabled": false,
    "disabled_reason": null,
    "cap": null,
    "balance": null,
    "auto_reload": null,
    "disclaimer": "Usage credits cover you when you hit your plan limits. [Learn more](https://support.claude.com/articles/12429409)",
    "can_purchase_credits": false,
    "can_toggle": false
  },
  "member_dashboard_available": false
}`

// liveFixtureBody returns the full-shape usage response body shared by the
// parse and display tests.
func liveFixtureBody(t *testing.T) []byte {
	t.Helper()
	return []byte(liveUsageResponseBody)
}

// The live 2026-07-01 fixture exercises the full current response shape:
// dollar fields on windows, the nimbus_quill/amber_ladder placeholders, the
// extra_usage decimal_places/daily/weekly additions, the limits[] array
// (including a model-scoped entry), and the spend object.
func TestGetUsageLimits_LiveFixture_AllFieldsParsed(t *testing.T) {
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

	// five_hour: utilization 47, all three dollar fields nil.
	if limits.FiveHour == nil || roundUtilization(limits.FiveHour.Utilization) != 47 {
		t.Errorf("FiveHour = %+v, want utilization 47", limits.FiveHour)
	}
	if limits.FiveHour != nil && (limits.FiveHour.LimitDollars != nil ||
		limits.FiveHour.UsedDollars != nil || limits.FiveHour.RemainingDollars != nil) {
		t.Errorf("FiveHour dollar fields should be nil: %+v", limits.FiveHour)
	}
	if limits.FiveHour.ResetsAtTime.IsZero() {
		t.Error("FiveHour.ResetsAtTime was not parsed")
	}
	if limits.SevenDay == nil || limits.SevenDay.ResetsAtTime.IsZero() {
		t.Error("SevenDay.ResetsAtTime was not parsed")
	}

	// All null windows, including the new placeholders, unmarshal to nil.
	if limits.SevenDayOAuthApps != nil || limits.SevenDayOpus != nil ||
		limits.SevenDaySonnet != nil || limits.SevenDayCowork != nil ||
		limits.SevenDayOmelette != nil || limits.Tangelo != nil ||
		limits.IguanaNecktie != nil || limits.OmelettePromotional != nil ||
		limits.CinderCove != nil || limits.NimbusQuill != nil || limits.AmberLadder != nil {
		t.Error("null windows (including nimbus_quill, amber_ladder, cinder_cove) should unmarshal to nil")
	}

	// extra_usage: disabled, decimal_places nil.
	if limits.ExtraUsage == nil {
		t.Fatal("ExtraUsage should be present")
	}
	if limits.ExtraUsage.IsEnabled {
		t.Error("ExtraUsage.IsEnabled = true, want false")
	}
	if limits.ExtraUsage.DecimalPlaces != nil {
		t.Errorf("ExtraUsage.DecimalPlaces = %v, want nil", *limits.ExtraUsage.DecimalPlaces)
	}

	// limits[]: 3 entries, kinds session/weekly_all/weekly_scoped.
	if len(limits.Limits) != 3 {
		t.Fatalf("len(Limits) = %d, want 3", len(limits.Limits))
	}
	wantKinds := []string{"session", "weekly_all", "weekly_scoped"}
	for i, want := range wantKinds {
		if limits.Limits[i].Kind != want {
			t.Errorf("Limits[%d].Kind = %q, want %q", i, limits.Limits[i].Kind, want)
		}
		if limits.Limits[i].ResetsAtTime.IsZero() {
			t.Errorf("Limits[%d].ResetsAtTime was not parsed", i)
		}
	}

	scoped := limits.Limits[2]
	if roundUtilization(scoped.Percent) != 12 {
		t.Errorf("weekly_scoped Percent = %v, want 12", scoped.Percent)
	}
	if scoped.Severity != "normal" {
		t.Errorf("weekly_scoped Severity = %q, want %q", scoped.Severity, "normal")
	}
	if scoped.IsActive {
		t.Error("weekly_scoped IsActive = true, want false")
	}
	if scoped.Scope == nil || scoped.Scope.Model == nil {
		t.Fatal("weekly_scoped Scope.Model should be present")
	}
	if scoped.Scope.Model.DisplayName == nil || *scoped.Scope.Model.DisplayName != "Fable" {
		t.Errorf("weekly_scoped Scope.Model.DisplayName = %v, want \"Fable\"", scoped.Scope.Model.DisplayName)
	}
	if scoped.Scope.Model.ID != nil {
		t.Errorf("weekly_scoped Scope.Model.ID = %v, want nil", *scoped.Scope.Model.ID)
	}

	// spend: parsed, disabled.
	if limits.Spend == nil {
		t.Fatal("Spend should be present")
	}
	if limits.Spend.Enabled {
		t.Error("Spend.Enabled = true, want false")
	}
	if limits.Spend.Percent != 0 {
		t.Errorf("Spend.Percent = %v, want 0", limits.Spend.Percent)
	}
	if limits.Spend.Used == nil {
		t.Fatal("Spend.Used should be present")
	}
	if limits.Spend.Used.AmountMinor != 0 {
		t.Errorf("Spend.Used.AmountMinor = %d, want 0", limits.Spend.Used.AmountMinor)
	}
	if limits.Spend.Used.Currency != "USD" {
		t.Errorf("Spend.Used.Currency = %q, want %q", limits.Spend.Used.Currency, "USD")
	}
	if limits.Spend.Used.Exponent != 2 {
		t.Errorf("Spend.Used.Exponent = %d, want 2", limits.Spend.Used.Exponent)
	}
	if limits.Spend.CanPurchaseCredits {
		t.Error("Spend.CanPurchaseCredits = true, want false")
	}

	if limits.MemberDashboardAvailable {
		t.Error("MemberDashboardAvailable = true, want false")
	}
}

// unknownTopLevelKeys mirrors the drift check performed inside GetUsageLimits
// so it can be exercised directly against arbitrary bodies.
func unknownTopLevelKeys(body []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	var unknown []string
	for key := range raw {
		if !usageResponseKnownKeys[key] {
			unknown = append(unknown, key)
		}
	}
	return unknown
}

func TestUnknownTopLevelKeys_DetectsInjectedKey(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":1},"zebra_lantern":"surprise"}`)
	got := unknownTopLevelKeys(body)
	if len(got) != 1 || got[0] != "zebra_lantern" {
		t.Errorf("unknownTopLevelKeys = %v, want [\"zebra_lantern\"]", got)
	}
}

func TestUnknownTopLevelKeys_LiveFixtureHasNone(t *testing.T) {
	body := liveFixtureBody(t)
	if got := unknownTopLevelKeys(body); len(got) != 0 {
		t.Errorf("unknownTopLevelKeys(live fixture) = %v, want none", got)
	}
}

// NewOAuthUsageClient must send the token its provider returns as a Bearer
// header, alongside the OAuth beta header the endpoint requires.
func TestNewOAuthUsageClient_SendsProviderTokenAndBetaHeader(t *testing.T) {
	var gotAuth, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10}}`))
	}))
	defer srv.Close()

	c := NewOAuthUsageClient(func() (string, error) { return "provider-token", nil })
	c.baseURL = srv.URL

	if _, err := c.GetUsageLimits(); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if gotAuth != "Bearer provider-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer provider-token")
	}
	if gotBeta != oauthBetaHeader {
		t.Errorf("anthropic-beta header = %q, want %q", gotBeta, oauthBetaHeader)
	}
}

// A token provider failure (e.g. Claude Code not installed, or its refresh
// token exhausted) must surface as ErrAuthFailed and must never reach the
// network — the request is never sent.
func TestNewOAuthUsageClient_ProviderError_WrapsErrAuthFailedWithoutRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP request should not be made when the token provider fails")
	}))
	defer srv.Close()

	c := NewOAuthUsageClient(func() (string, error) { return "", errors.New("no credential") })
	c.baseURL = srv.URL

	_, err := c.GetUsageLimits()
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got: %v", err)
	}
}

func TestIsTransientStatus(t *testing.T) {
	transient := []int{408, 500, 502, 503, 504}
	for _, code := range transient {
		if !isTransientStatus(code) {
			t.Errorf("status %d should be transient", code)
		}
	}
	// 429 is deliberately excluded: it is intercepted earlier in
	// GetUsageLimits and returned as a *RateLimitError instead, so it must not
	// also be reported as a generic transient status here.
	notTransient := []int{200, 400, 401, 403, 404, 429}
	for _, code := range notTransient {
		if isTransientStatus(code) {
			t.Errorf("status %d should not be transient", code)
		}
	}
}

// parseRetryAfter must accept delta-seconds and HTTP-date forms, and must
// never return a negative duration for an absent, malformed, zero, negative,
// or already-past value.
func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"zero seconds", "0", 0},
		{"negative seconds", "-5", 0},
		{"positive seconds", "7", 7 * time.Second},
		{"padded with whitespace", "  10  ", 10 * time.Second},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("%s: parseRetryAfter(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}

	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 {
		t.Errorf("parseRetryAfter(future HTTP-date %q) = %v, want > 0", future, got)
	}

	past := time.Now().Add(-2 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(past HTTP-date %q) = %v, want 0", past, got)
	}
}

// RateLimitError must satisfy errors.Is(err, ErrTransient) so it flows through
// existing transient handling (classifyUpdate, the 3-strike tolerance), while
// remaining distinct from a real auth failure.
func TestRateLimitErrorIsTransient(t *testing.T) {
	err := &RateLimitError{StatusCode: 429, Body: "rate limited"}
	if !errors.Is(err, ErrTransient) {
		t.Error("errors.Is(RateLimitError, ErrTransient) = false, want true")
	}
	if errors.Is(err, ErrAuthFailed) {
		t.Error("errors.Is(RateLimitError, ErrAuthFailed) = true, want false")
	}
}

// A live 429 response must come back as a *RateLimitError carrying the
// parsed Retry-After hint, not a generic ErrTransient string, so the caller
// can drive backoff off it.
func TestGetUsageLimits_429_ReturnsRateLimitErrorWithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "test-org")
	_, err := c.GetUsageLimits()

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got: %v (%T)", err, err)
	}
	if rl.StatusCode != http.StatusTooManyRequests {
		t.Errorf("RateLimitError.StatusCode = %d, want %d", rl.StatusCode, http.StatusTooManyRequests)
	}
	if rl.RetryAfter != 42*time.Second {
		t.Errorf("RateLimitError.RetryAfter = %v, want %v", rl.RetryAfter, 42*time.Second)
	}
	if !errors.Is(err, ErrTransient) {
		t.Error("expected a 429 error to satisfy errors.Is(err, ErrTransient)")
	}
}

// A 429 with no (or an unusable) Retry-After header must still classify as a
// RateLimitError, just with RetryAfter == 0 — the caller's own backoff curve
// takes over instead of a server hint.
func TestGetUsageLimits_429_NoRetryAfter_ZeroHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "test-org")
	_, err := c.GetUsageLimits()

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got: %v (%T)", err, err)
	}
	if rl.RetryAfter != 0 {
		t.Errorf("RateLimitError.RetryAfter = %v, want 0", rl.RetryAfter)
	}
}
