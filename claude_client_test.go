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
