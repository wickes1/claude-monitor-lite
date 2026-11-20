package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A fresh, non-expired credential from Claude Code's store is returned as-is.
func TestToken_FreshCredential_Returned(t *testing.T) {
	src := &credentialSource{read: func() (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "fresh-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}}

	token, err := src.token()
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
func TestToken_ExpiredCredential_ReturnsTransient(t *testing.T) {
	src := &credentialSource{read: func() (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "stale-token",
			ExpiresAt:   time.Now().Add(-time.Hour).UnixMilli(), // already expired
		}, nil
	}}

	_, err := src.token()
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient (must not spend Claude Code's refresh grant), got: %v", err)
	}
	// The menu tells this wait apart from an outage: it is the normal state of
	// an environment Claude Code is not running in.
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got: %v", err)
	}
}

// After invalidate, the next call must re-read rather than serve a
// cached token — and, within the cache window, a second call must NOT re-read.
func TestToken_Invalidate_ForcesReread(t *testing.T) {
	reads := 0
	src := &credentialSource{read: func() (*OAuthCredential, error) {
		reads++
		return &OAuthCredential{
			AccessToken: "token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}}

	if _, err := src.token(); err != nil {
		t.Fatalf("first call: expected success, got: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads after first call = %d, want 1", reads)
	}

	// Within the cache window, a second call must reuse the cache.
	if _, err := src.token(); err != nil {
		t.Fatalf("second call: expected success, got: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads after cached second call = %d, want 1 (cache should have been used)", reads)
	}

	src.invalidate()

	if _, err := src.token(); err != nil {
		t.Fatalf("third call: expected success, got: %v", err)
	}
	if reads != 2 {
		t.Fatalf("reads after invalidate = %d, want 2 (must re-read)", reads)
	}
}

// When Claude Code's credential store cannot be read at all (not installed,
// not logged in), the read error itself must propagate so the caller can
// report why.
func TestToken_ReadFails_PropagatesReadError(t *testing.T) {
	wantErr := errors.New("no Claude Code credential found")
	src := &credentialSource{read: func() (*OAuthCredential, error) {
		return nil, wantErr
	}}

	_, err := src.token()
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the read error to propagate, got: %v", err)
	}
}

// A cached credential's own expiry must still be respected — an expired cache
// entry must not be served, it must fall through to re-reading.
func TestToken_ExpiredCache_ReReads(t *testing.T) {
	src := &credentialSource{read: func() (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "new-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}}
	src.cached = &OAuthCredential{
		AccessToken: "old-cached-token",
		ExpiresAt:   time.Now().Add(-time.Hour).UnixMilli(), // expired
	}

	token, err := src.token()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if token != "new-token" {
		t.Errorf("token = %q, want %q (expired cache must not be served)", token, "new-token")
	}
}

// Two profiles are two accounts: one's token must never be handed out for
// the other, or its group would show the wrong account's usage.
func TestTokenIsPerProfile(t *testing.T) {
	source := func(token string) *credentialSource {
		return &credentialSource{read: func() (*OAuthCredential, error) {
			return &OAuthCredential{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
		}}
	}
	personal, work := source("personal-token"), source("work-token")
	for _, tc := range []struct {
		src  *credentialSource
		want string
	}{{personal, "personal-token"}, {work, "work-token"}, {personal, "personal-token"}} {
		if got, err := tc.src.token(); err != nil || got != tc.want {
			t.Errorf("token() = %q, %v, want %q", got, err, tc.want)
		}
	}
}

// A CLAUDE_CONFIG_DIR environment must read its own Keychain item. Reading the
// plain item instead silently reports the default account's usage under the
// other environment's label. The hash follows Claude Code's own naming,
// checked against an item it created in a real Keychain.
func TestKeychainService(t *testing.T) {
	if got := newProfile("").keychainService(); got != keychainServiceBase {
		t.Errorf("default env: got %q, want plain %q", got, keychainServiceBase)
	}

	const want = "Claude Code-credentials-be865d75"
	for _, dir := range []string{
		"/Users/alice/.claude-work",
		"/Users/alice/.claude-work/",
		"/Users/alice/x/../.claude-work",
	} {
		if got := newProfile(dir).keychainService(); got != want {
			t.Errorf("config dir %q: got %q, want %q", dir, got, want)
		}
	}
}

// Only a Claude subscription login can read plan usage. Anything else must be
// turned away with a message that says why, instead of polling into a
// permission error and showing "Error" forever.
func TestParseCredentialRequiresSubscriptionLogin(t *testing.T) {
	cases := []struct {
		name    string
		blob    string
		wantErr string // "" for a usable credential
	}{
		{"subscription login", `{"claudeAiOauth":{"accessToken":"t","scopes":["user:inference","user:profile"]}}`, ""},
		{"inference-only token", `{"claudeAiOauth":{"accessToken":"t","scopes":["user:inference"]}}`, "user:profile"},
		{"no scopes at all", `{"claudeAiOauth":{"accessToken":"t"}}`, "user:profile"},
		{"API key or Console login: no subscription block", `{"mcpOAuth":{}}`, "Claude subscription"},
	}
	for _, tc := range cases {
		cred, err := parseCredential([]byte(tc.blob))
		switch {
		case tc.wantErr == "" && (err != nil || cred.AccessToken != "t"):
			t.Errorf("%s: got %+v, %v; want a usable credential", tc.name, cred, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.wantErr)
		}
	}
}
