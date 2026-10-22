package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// liveShapeBody is the usage response shape observed on 2026-10-17 (Max and
// Team accounts), trimmed to one of each kind of field: populated windows,
// null codename windows, extra_usage, and limits[] with an unscoped and a
// model-scoped entry.
const liveShapeBody = `{
	"five_hour": {"utilization": 47.0, "resets_at": "2026-07-02T03:00:00.000001+00:00"},
	"seven_day": {"utilization": 9.0, "resets_at": "2026-07-03T01:00:00.000001+00:00"},
	"seven_day_opus": null,
	"amber_gauge": null,
	"extra_usage": {"is_enabled": false, "utilization": null},
	"limits": [
		{"kind": "session", "percent": 47, "resets_at": "2026-07-02T03:00:00.000001+00:00", "scope": null},
		{"kind": "weekly_scoped", "percent": 12, "resets_at": "2026-07-03T01:00:00.000002+00:00",
		 "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}}
	],
	"spend": {"percent": 0},
	"member_dashboard_available": false
}`

// serveUsage returns a client whose token provider succeeds, pointed at a test
// server running handler.
func serveUsage(t *testing.T, handler http.HandlerFunc) *ClaudeUsageClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewUsageClient(func() (string, error) { return "provider-token", nil })
	c.url = srv.URL
	return c
}

func serveBody(t *testing.T, status int, body string) *ClaudeUsageClient {
	return serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// The live response must parse: populated windows with their reset times, and
// the scoped Fable entry the console shows. Unknown and null keys must not
// break decoding — the endpoint adds codename windows often.
func TestGetUsageLimits_LiveShape(t *testing.T) {
	limits, err := serveBody(t, http.StatusOK, liveShapeBody).GetUsageLimits()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if limits.FiveHour == nil || roundUtilization(limits.FiveHour.Utilization) != 47 || limits.FiveHour.ResetsAt.IsZero() {
		t.Errorf("FiveHour = %+v, want 47%% with a reset time", limits.FiveHour)
	}
	if limits.SevenDay == nil || limits.SevenDay.ResetsAt.IsZero() {
		t.Errorf("SevenDay = %+v, want a reset time", limits.SevenDay)
	}
	if extraUsageWindow(limits) != nil {
		t.Error("extra_usage with null utilization should report nothing")
	}
	if len(limits.Limits) != 2 || limits.Limits[1].Scope == nil || *limits.Limits[1].Scope.Model.DisplayName != "Fable" {
		t.Errorf("Limits = %+v, want the scoped Fable entry second", limits.Limits)
	}
}

// A null resets_at (a window with nothing to reset) must decode to the zero
// time, which the display treats as "no reset", not as a parse failure.
func TestGetUsageLimits_NullResetsAt(t *testing.T) {
	limits, err := serveBody(t, http.StatusOK, `{"five_hour":{"utilization":0,"resets_at":null}}`).GetUsageLimits()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !limits.FiveHour.ResetsAt.IsZero() {
		t.Errorf("ResetsAt = %v, want zero", limits.FiveHour.ResetsAt)
	}
}

// The request must carry the provider's token, the OAuth beta header the
// endpoint requires, and our own User-Agent (see the comment in
// GetUsageLimits on why it is never claude-code's).
func TestGetUsageLimits_SendsHeaders(t *testing.T) {
	var got http.Header
	c := serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.GetUsageLimits(); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if v := got.Get("Authorization"); v != "Bearer provider-token" {
		t.Errorf("Authorization = %q", v)
	}
	if v := got.Get("anthropic-beta"); v != oauthBetaHeader {
		t.Errorf("anthropic-beta = %q, want %q", v, oauthBetaHeader)
	}
	if v := got.Get("User-Agent"); v != "claude-monitor-lite/"+version {
		t.Errorf("User-Agent = %q, want claude-monitor-lite/%s", v, version)
	}
}

// Status classification drives the menu bar: 401 asks the user to log in,
// anything else that is not 200 keeps last-good data (transient), and a
// non-JSON 200 (edge interstitial, truncated body) is transient too.
func TestGetUsageLimits_StatusClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"401 is an auth failure", http.StatusUnauthorized, "", ErrAuthFailed},
		{"5xx is transient", http.StatusServiceUnavailable, "", ErrTransient},
		{"403 edge challenge is transient", http.StatusForbidden, "<!DOCTYPE html>", ErrTransient},
		{"non-JSON 200 is transient", http.StatusOK, "<html>Just a moment...</html>", ErrTransient},
		{"empty 200 is transient", http.StatusOK, "", ErrTransient},
	}
	for _, tc := range cases {
		_, err := serveBody(t, tc.status, tc.body).GetUsageLimits()
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if tc.want == ErrTransient && errors.Is(err, ErrAuthFailed) {
			t.Errorf("%s: transient error must not also read as an auth failure", tc.name)
		}
	}
}

// A token provider failure (Claude Code not logged in) is an auth failure and
// must never reach the network; a transient provider failure (token briefly
// expired) must stay transient so a logged-in user is not told to log in.
func TestGetUsageLimits_ProviderErrors(t *testing.T) {
	cases := []struct {
		providerErr error
		want        error
	}{
		{errors.New("no credential"), ErrAuthFailed},
		{ErrTransient, ErrTransient},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("no request may be sent when the token provider fails")
		}))
		c := NewUsageClient(func() (string, error) { return "", tc.providerErr })
		c.url = srv.URL
		_, err := c.GetUsageLimits()
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("provider error %v: got %v, want %v", tc.providerErr, err, tc.want)
		}
	}
}

// A 429 must come back as a *RateLimitError carrying the parsed Retry-After,
// so the caller can drive backoff off it — and still count as transient.
func TestGetUsageLimits_429(t *testing.T) {
	for retryAfter, want := range map[string]time.Duration{"42": 42 * time.Second, "": 0} {
		c := serveUsage(t, func(w http.ResponseWriter, r *http.Request) {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		})
		_, err := c.GetUsageLimits()
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("Retry-After %q: expected *RateLimitError, got %v (%T)", retryAfter, err, err)
		}
		if rl.RetryAfter != want {
			t.Errorf("Retry-After %q: RetryAfter = %v, want %v", retryAfter, rl.RetryAfter, want)
		}
		if !errors.Is(err, ErrTransient) || errors.Is(err, ErrAuthFailed) {
			t.Errorf("Retry-After %q: a 429 must be transient and never an auth failure", retryAfter)
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
		{"malformed", "soon", 0},
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
