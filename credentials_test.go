package main

import (
	"errors"
	"testing"
	"time"
)

// saveOAuthTokenVars snapshots the package-level OAuth token state (the
// injectable credential reader and the in-memory cache) and restores it on
// cleanup, so each test can substitute a fake without leaking into others.
func saveOAuthTokenVars(t *testing.T) {
	t.Helper()
	origRead := readCredential
	origCached := cachedOAuthCredential
	t.Cleanup(func() {
		oauthTokenMu.Lock()
		readCredential = origRead
		cachedOAuthCredential = origCached
		oauthTokenMu.Unlock()
	})
}

// A fresh, non-expired credential from Claude Code's store is returned as-is.
func TestCurrentOAuthToken_FreshCredential_Returned(t *testing.T) {
	saveOAuthTokenVars(t)
	cachedOAuthCredential = nil

	readCredential = func() (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "fresh-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	token, err := CurrentOAuthToken()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if token != "fresh-token" {
		t.Errorf("token = %q, want %q", token, "fresh-token")
	}
}

// An expired credential must NOT be refreshed by us — spending Claude Code's
// refresh grant could rotate its token and force the user to sign back into
// Claude Code. We report transient and wait for Claude Code to refresh its own
// token, so the caller keeps last-good data instead of forcing a re-login.
func TestCurrentOAuthToken_ExpiredCredential_ReturnsTransient(t *testing.T) {
	saveOAuthTokenVars(t)
	cachedOAuthCredential = nil

	readCredential = func() (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken:  "stale-token",
			RefreshToken: "refresh-me",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(), // already expired
		}, nil
	}

	_, err := CurrentOAuthToken()
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient (must not spend Claude Code's refresh grant), got: %v", err)
	}
}

// After InvalidateOAuthToken, the next call must re-read rather than serve a
// cached token — and, within the cache window, a second call must NOT re-read.
func TestCurrentOAuthToken_Invalidate_ForcesReread(t *testing.T) {
	saveOAuthTokenVars(t)
	cachedOAuthCredential = nil

	reads := 0
	readCredential = func() (*OAuthCredential, error) {
		reads++
		return &OAuthCredential{
			AccessToken: "token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	if _, err := CurrentOAuthToken(); err != nil {
		t.Fatalf("first call: expected success, got: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads after first call = %d, want 1", reads)
	}

	// Within the cache window, a second call must reuse the cache.
	if _, err := CurrentOAuthToken(); err != nil {
		t.Fatalf("second call: expected success, got: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads after cached second call = %d, want 1 (cache should have been used)", reads)
	}

	InvalidateOAuthToken()

	if _, err := CurrentOAuthToken(); err != nil {
		t.Fatalf("third call: expected success, got: %v", err)
	}
	if reads != 2 {
		t.Fatalf("reads after invalidate = %d, want 2 (must re-read)", reads)
	}
}

// When Claude Code's credential store cannot be read at all (not installed,
// not logged in), the read error itself must propagate so the caller can
// report why.
func TestCurrentOAuthToken_ReadFails_PropagatesReadError(t *testing.T) {
	saveOAuthTokenVars(t)
	cachedOAuthCredential = nil

	wantErr := errors.New("no Claude Code credential found")
	readCredential = func() (*OAuthCredential, error) {
		return nil, wantErr
	}

	_, err := CurrentOAuthToken()
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the read error to propagate, got: %v", err)
	}
}

// A cached credential's own expiry must still be respected — an expired cache
// entry must not be served, it must fall through to re-reading.
func TestCurrentOAuthToken_ExpiredCache_ReReads(t *testing.T) {
	saveOAuthTokenVars(t)
	cachedOAuthCredential = &OAuthCredential{
		AccessToken: "old-cached-token",
		ExpiresAt:   time.Now().Add(-time.Hour).UnixMilli(), // expired
	}

	readCredential = func() (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "new-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	token, err := CurrentOAuthToken()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if token != "new-token" {
		t.Errorf("token = %q, want %q (expired cache must not be served)", token, "new-token")
	}
}

// InvalidateOAuthToken must be safe to call with no cached credential.
func TestInvalidateOAuthToken_NoCachedCredential_NoPanic(t *testing.T) {
	saveOAuthTokenVars(t)
	cachedOAuthCredential = nil

	InvalidateOAuthToken()

	if cachedOAuthCredential != nil {
		t.Error("cachedOAuthCredential should remain nil")
	}
}
