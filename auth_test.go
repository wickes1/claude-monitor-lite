package main

import (
	"os"
	"strings"
	"testing"
)

// Security regression: an OAuth session must never write a token to disk. The
// access/refresh token lives in Claude Code's own credential store (Keychain
// on macOS, .credentials.json elsewhere) and is read at fetch time in memory
// only — see CurrentOAuthToken in credentials.go. Duplicating it into our
// plaintext config would be a downgrade from the Keychain and an unnecessary
// second copy of a broad-scope credential.
func TestSaveAuthSession_OAuth_NeverPersistsToken(t *testing.T) {
	path := withTempConfigPath(t)

	session := &AuthSession{AuthMode: AuthModeOAuth}
	if err := SaveAuthSession(session); err != nil {
		t.Fatalf("SaveAuthSession failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	got := string(raw)

	for _, secret := range []string{"accessToken", "refreshToken", "expiresAt"} {
		if strings.Contains(got, secret) {
			t.Errorf("saved config contains %q, want no token fields:\n%s", secret, got)
		}
	}

	// Confirm the save actually happened (not a false-positive from an empty
	// or unwritten file) and that the metadata we DO want persisted is there.
	if !strings.Contains(got, `"authMode"`) || !strings.Contains(got, "oauth") {
		t.Errorf("saved config is missing authMode metadata:\n%s", got)
	}
	if !strings.Contains(got, `"savedAt"`) {
		t.Errorf("saved config is missing savedAt metadata:\n%s", got)
	}
}

// LoadAuthSession must accept a persisted OAuth session on authMode alone —
// with no token fields on Config to check, credential availability is left to
// CurrentOAuthToken at fetch time (lazily), not to LoadAuthSession.
func TestLoadAuthSession_OAuth_RoundTrips(t *testing.T) {
	withTempConfigPath(t)

	if err := SaveAuthSession(&AuthSession{AuthMode: AuthModeOAuth}); err != nil {
		t.Fatalf("SaveAuthSession failed: %v", err)
	}

	session, err := LoadAuthSession()
	if err != nil {
		t.Fatalf("LoadAuthSession failed: %v", err)
	}
	if session.AuthMode != AuthModeOAuth {
		t.Errorf("AuthMode = %q, want %q", session.AuthMode, AuthModeOAuth)
	}
}

// ClearAuthSession must remove authMode/sessionKey/organizationId/savedAt but
// leave unrelated settings (menuBarIndicator) untouched.
func TestClearAuthSession_PreservesMenuBarIndicator(t *testing.T) {
	withTempConfigPath(t)

	if err := SaveAuthSession(&AuthSession{AuthMode: AuthModeCookie, SessionKey: "k"}); err != nil {
		t.Fatalf("SaveAuthSession failed: %v", err)
	}
	if err := SaveConfigPreservingSession("weeklyAll"); err != nil {
		t.Fatalf("SaveConfigPreservingSession failed: %v", err)
	}

	if err := ClearAuthSession(); err != nil {
		t.Fatalf("ClearAuthSession failed: %v", err)
	}

	if _, err := LoadAuthSession(); err == nil {
		t.Error("LoadAuthSession succeeded after ClearAuthSession, want an error")
	}
	if got := LoadConfig().MenuBarIndicator; got != "weeklyAll" {
		t.Errorf("MenuBarIndicator = %q, want preserved %q", got, "weeklyAll")
	}
}
