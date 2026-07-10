package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetOAuthCache snapshots the package-level OAuth token state (the
// injectable credential reader and the in-memory per-dir cache) and restores
// it on cleanup, so each test can substitute a fake without leaking into
// others or into the real Keychain/credentials file.
func resetOAuthCache(t *testing.T) {
	t.Helper()
	oauthTokenMu.Lock()
	origRead := readCredential
	origCache := cachedCredentials
	cachedCredentials = make(map[string]*OAuthCredential)
	oauthTokenMu.Unlock()

	t.Cleanup(func() {
		oauthTokenMu.Lock()
		readCredential = origRead
		cachedCredentials = origCache
		oauthTokenMu.Unlock()
	})
}

const testProfileDir = "/tmp/claude-monitor-lite-test-profile-dir"

// A fresh, non-expired credential from Claude Code's store is returned as-is.
func TestCurrentOAuthTokenForDir_FreshCredential_Returned(t *testing.T) {
	resetOAuthCache(t)

	readCredential = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "fresh-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	token, err := CurrentOAuthTokenForDir(testProfileDir)
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
func TestCurrentOAuthTokenForDir_ExpiredCredential_ReturnsTransient(t *testing.T) {
	resetOAuthCache(t)

	readCredential = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken:  "stale-token",
			RefreshToken: "refresh-me",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(), // already expired
		}, nil
	}

	_, err := CurrentOAuthTokenForDir(testProfileDir)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient (must not spend Claude Code's refresh grant), got: %v", err)
	}
}

// After InvalidateOAuthTokenForDir, the next call must re-read rather than
// serve a cached token — and, within the cache window, a second call must
// NOT re-read.
func TestCurrentOAuthTokenForDir_Invalidate_ForcesReread(t *testing.T) {
	resetOAuthCache(t)

	reads := 0
	readCredential = func(dir string) (*OAuthCredential, error) {
		reads++
		return &OAuthCredential{
			AccessToken: "token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	if _, err := CurrentOAuthTokenForDir(testProfileDir); err != nil {
		t.Fatalf("first call: expected success, got: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads after first call = %d, want 1", reads)
	}

	// Within the cache window, a second call must reuse the cache.
	if _, err := CurrentOAuthTokenForDir(testProfileDir); err != nil {
		t.Fatalf("second call: expected success, got: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads after cached second call = %d, want 1 (cache should have been used)", reads)
	}

	InvalidateOAuthTokenForDir(testProfileDir)

	if _, err := CurrentOAuthTokenForDir(testProfileDir); err != nil {
		t.Fatalf("third call: expected success, got: %v", err)
	}
	if reads != 2 {
		t.Fatalf("reads after invalidate = %d, want 2 (must re-read)", reads)
	}
}

// When Claude Code's credential store cannot be read at all (not installed,
// not logged in), the read error itself must propagate so the caller can
// report why.
func TestCurrentOAuthTokenForDir_ReadFails_PropagatesReadError(t *testing.T) {
	resetOAuthCache(t)

	wantErr := errors.New("no Claude Code credential found")
	readCredential = func(dir string) (*OAuthCredential, error) {
		return nil, wantErr
	}

	_, err := CurrentOAuthTokenForDir(testProfileDir)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the read error to propagate, got: %v", err)
	}
}

// A cached credential's own expiry must still be respected — an expired cache
// entry must not be served, it must fall through to re-reading.
func TestCurrentOAuthTokenForDir_ExpiredCache_ReReads(t *testing.T) {
	resetOAuthCache(t)
	oauthTokenMu.Lock()
	cachedCredentials[cleanAbsoluteDirPath(testProfileDir)] = &OAuthCredential{
		AccessToken: "old-cached-token",
		ExpiresAt:   time.Now().Add(-time.Hour).UnixMilli(), // expired
	}
	oauthTokenMu.Unlock()

	readCredential = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken: "new-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	token, err := CurrentOAuthTokenForDir(testProfileDir)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if token != "new-token" {
		t.Errorf("token = %q, want %q (expired cache must not be served)", token, "new-token")
	}
}

// InvalidateOAuthTokenForDir must be safe to call with no cached credential.
func TestInvalidateOAuthTokenForDir_NoCachedCredential_NoPanic(t *testing.T) {
	resetOAuthCache(t)

	InvalidateOAuthTokenForDir(testProfileDir)

	oauthTokenMu.Lock()
	_, ok := cachedCredentials[cleanAbsoluteDirPath(testProfileDir)]
	oauthTokenMu.Unlock()
	if ok {
		t.Error("cachedCredentials should have no entry for testProfileDir")
	}
}

// Two different profile dirs must cache independently: reading one must
// never evict or return the other's credential.
func TestCurrentOAuthTokenForDir_PerDirCache_Independent(t *testing.T) {
	resetOAuthCache(t)

	dirA := "/tmp/claude-monitor-lite-test-profile-a"
	dirB := "/tmp/claude-monitor-lite-test-profile-b"

	reads := map[string]int{}
	readCredential = func(dir string) (*OAuthCredential, error) {
		reads[dir]++
		token := "token-for-" + dir
		return &OAuthCredential{
			AccessToken: token,
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	tokenA, err := CurrentOAuthTokenForDir(dirA)
	if err != nil {
		t.Fatalf("dirA: expected success, got: %v", err)
	}
	tokenB, err := CurrentOAuthTokenForDir(dirB)
	if err != nil {
		t.Fatalf("dirB: expected success, got: %v", err)
	}
	if tokenA == tokenB {
		t.Fatalf("dirA and dirB returned the same token %q, want independent tokens", tokenA)
	}

	// Re-reading dirA must be served from cache, not re-read, and must not
	// have been disturbed by dirB's read.
	if _, err := CurrentOAuthTokenForDir(dirA); err != nil {
		t.Fatalf("re-read of dirA failed: %v", err)
	}
	if reads[dirA] != 1 {
		t.Errorf("reads[dirA] = %d, want 1 (should be cached)", reads[dirA])
	}

	InvalidateOAuthTokenForDir(dirA)
	if _, err := CurrentOAuthTokenForDir(dirB); err != nil {
		t.Fatalf("dirB re-check after invalidating dirA failed: %v", err)
	}
	if reads[dirB] != 1 {
		t.Errorf("reads[dirB] = %d, want 1 (invalidating dirA must not evict dirB)", reads[dirB])
	}
}

// CurrentOAuthToken (no-arg) must delegate to the active profile's dir, and
// with zero profiles configured that is claudeConfigDir() — pre-feature
// behavior (backward-compat contract 7).
func TestCurrentOAuthToken_DelegatesToActiveProfileDir(t *testing.T) {
	resetOAuthCache(t)
	oldConfigPath := configPathOverride
	configPathOverride = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { configPathOverride = oldConfigPath })

	oldEnv, had := os.LookupEnv("CLAUDE_CONFIG_DIR")
	fakeDefaultDir := filepath.Join(t.TempDir(), "fake-default")
	os.Setenv("CLAUDE_CONFIG_DIR", fakeDefaultDir)
	t.Cleanup(func() {
		if had {
			os.Setenv("CLAUDE_CONFIG_DIR", oldEnv)
		} else {
			os.Unsetenv("CLAUDE_CONFIG_DIR")
		}
	})

	var gotDir string
	readCredential = func(dir string) (*OAuthCredential, error) {
		gotDir = dir
		return &OAuthCredential{
			AccessToken: "tok",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	if _, err := CurrentOAuthToken(); err != nil {
		t.Fatalf("CurrentOAuthToken failed: %v", err)
	}
	if gotDir != fakeDefaultDir {
		t.Errorf("CurrentOAuthToken read dir %q, want the default dir %q (no profiles configured)", gotDir, fakeDefaultDir)
	}
}

// --- ISC-2, 3: keychain service derivation + file fallback ordering ---

func TestReadClaudeCodeCredentialForDir_FileWins_KeychainNeverCalled(t *testing.T) {
	origFileReader := fileReaderForDir
	origKeychainReader := keychainReaderForDir
	t.Cleanup(func() {
		fileReaderForDir = origFileReader
		keychainReaderForDir = origKeychainReader
	})

	fileReaderForDir = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{AccessToken: "from-file"}, nil
	}
	keychainCalled := false
	keychainReaderForDir = func(dir string) (*OAuthCredential, error) {
		keychainCalled = true
		return nil, errors.New("keychain should not have been called")
	}

	cred, err := ReadClaudeCodeCredentialForDir("/some/profile/dir")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if cred.AccessToken != "from-file" {
		t.Errorf("AccessToken = %q, want %q", cred.AccessToken, "from-file")
	}
	if keychainCalled {
		t.Errorf("keychain reader was called even though the file read succeeded")
	}
}

// ISC-4: a read failure yields a typed "needs login" state via
// CredentialHealth, never a fatal error — and it must not depend on which
// step failed (file vs Keychain).
func TestReadClaudeCodeCredentialForDir_BothFail_TypedNeedsLogin(t *testing.T) {
	origFileReader := fileReaderForDir
	origKeychainReader := keychainReaderForDir
	origRead := readCredential
	t.Cleanup(func() {
		fileReaderForDir = origFileReader
		keychainReaderForDir = origKeychainReader
		readCredential = origRead
	})

	fileReaderForDir = func(dir string) (*OAuthCredential, error) {
		return nil, errors.New("no file")
	}
	keychainReaderForDir = func(dir string) (*OAuthCredential, error) {
		return nil, errors.New("no keychain item")
	}
	readCredential = ReadClaudeCodeCredentialForDir

	if _, err := ReadClaudeCodeCredentialForDir("/some/profile/dir"); err == nil {
		t.Fatal("expected an error when both file and keychain reads fail")
	}

	if got := CredentialHealth("/some/profile/dir"); got != CredentialNeedsLogin {
		t.Errorf("CredentialHealth = %q, want %q", got, CredentialNeedsLogin)
	}
}

func TestKeychainServiceForDir_NonDefaultDir_DerivedName(t *testing.T) {
	oldEnv, had := os.LookupEnv("CLAUDE_CONFIG_DIR")
	os.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "default"))
	t.Cleanup(func() {
		if had {
			os.Setenv("CLAUDE_CONFIG_DIR", oldEnv)
		} else {
			os.Unsetenv("CLAUDE_CONFIG_DIR")
		}
	})

	nonDefault := filepath.Join(t.TempDir(), "work")
	got := keychainServiceForDir(nonDefault)
	if got == keychainService {
		t.Errorf("keychainServiceForDir(non-default) = %q, want a derived, non-plain service name", got)
	}
	wantPrefix := keychainService + "-"
	if len(got) <= len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("keychainServiceForDir(non-default) = %q, want prefix %q", got, wantPrefix)
	}
}

// --- CredentialHealth ---

func TestCredentialHealth_States(t *testing.T) {
	origRead := readCredential
	t.Cleanup(func() { readCredential = origRead })

	tests := []struct {
		name string
		mock func(dir string) (*OAuthCredential, error)
		want CredentialStatus
	}{
		{
			name: "logged in",
			mock: func(dir string) (*OAuthCredential, error) {
				return &OAuthCredential{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
			},
			want: CredentialLoggedIn,
		},
		{
			name: "expired",
			mock: func(dir string) (*OAuthCredential, error) {
				return &OAuthCredential{AccessToken: "tok", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}, nil
			},
			want: CredentialExpired,
		},
		{
			name: "needs login",
			mock: func(dir string) (*OAuthCredential, error) {
				return nil, errors.New("not found")
			},
			want: CredentialNeedsLogin,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readCredential = tc.mock
			if got := CredentialHealth("/irrelevant/dir"); got != tc.want {
				t.Errorf("CredentialHealth() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- keychainServiceForDir path canonicalization ---

func TestCleanAbsoluteDirPath_Canonicalizes(t *testing.T) {
	rel := "./some/../some/dir"
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(abs)
	if got := cleanAbsoluteDirPath(rel); got != want {
		t.Errorf("cleanAbsoluteDirPath(%q) = %q, want %q", rel, got, want)
	}
}
