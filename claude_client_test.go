package main

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
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

func TestIsTransientStatus(t *testing.T) {
	transient := []int{408, 429, 500, 502, 503, 504}
	for _, code := range transient {
		if !isTransientStatus(code) {
			t.Errorf("status %d should be transient", code)
		}
	}
	notTransient := []int{200, 400, 401, 403, 404}
	for _, code := range notTransient {
		if isTransientStatus(code) {
			t.Errorf("status %d should not be transient", code)
		}
	}
}
