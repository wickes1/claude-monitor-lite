// credentials.go - Zero-paste auth by reusing Claude Code's existing login.
//
// Claude Code (and the wider Claude CLI ecosystem) stores a subscription OAuth
// token on disk after the user logs in. We read that credential and poll the
// same usage endpoint Claude Code uses, so the user never has to copy a session
// key out of the browser by hand. We never run our own login flow, never proxy
// inference, and never write back to Claude Code's credential store.
//
// The token itself is never persisted to our own config either: the
// CurrentOAuthToken family below reads it fresh from Claude Code's store on
// demand and caches it in memory only, for this process's lifetime. Writing a
// copy of Claude Code's broad-scope credential into our plaintext dotfile
// would be a downgrade from the Keychain and an unnecessary second copy of
// the same secret.
//
// Multi-profile support parameterizes every read by config directory (a
// profile IS a config directory — see profiles.go), because Claude Code
// derives a different Keychain service name per CLAUDE_CONFIG_DIR. The
// original zero-argument functions (ReadClaudeCodeCredential,
// CurrentOAuthToken, InvalidateOAuthToken) are kept as thin wrappers so
// existing callers (auth.go, main.go) are unaffected; they resolve to the
// default config dir / active profile respectively.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	// keychainService is the macOS Keychain item Claude Code stores its
	// credential under for the default config dir (~/.claude, i.e.
	// CLAUDE_CONFIG_DIR unset) on newer versions.
	keychainService = "Claude Code-credentials"

	// tokenRefreshLeeway is how far before expiry a token is treated as stale,
	// so a poll never races the expiry boundary.
	tokenRefreshLeeway = 2 * time.Minute
)

// oauthTokenMu guards cachedCredentials. The CurrentOAuthToken* and
// InvalidateOAuthToken* functions are the only code that may touch it. The
// cache lives in memory for this process only — it is never marshaled to our
// config file, so a compromised dotfile can no longer impersonate Claude
// Code's own broad-scope subscription credential.
var (
	oauthTokenMu sync.Mutex

	// cachedCredentials caches one credential per config dir, keyed by its
	// cleaned absolute path. A per-dir cache (rather than a single global
	// credential) is required once more than one profile is configured:
	// each profile's token is independent and must not evict another's.
	cachedCredentials = make(map[string]*OAuthCredential)

	// readCredential indirects ReadClaudeCodeCredentialForDir so tests can
	// substitute a fake without touching the real Keychain or credentials
	// file for any directory.
	readCredential = ReadClaudeCodeCredentialForDir

	// fileReaderForDir and keychainReaderForDir further indirect the two
	// underlying resolution steps, so tests can exercise ISC-3 (file
	// fallback) and ISC-4 (typed failure) independently without invoking a
	// real `security` process or touching a real credentials file.
	fileReaderForDir     = readCredentialFileAt
	keychainReaderForDir = readCredentialKeychainAt
)

// OAuthCredential is the claudeAiOauth block Claude Code persists.
type OAuthCredential struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // epoch milliseconds
}

// expired reports whether the access token is within the refresh leeway of (or
// past) its expiry. An unknown expiry (0) is treated as usable; a 401 will
// trigger a reactive refresh if it has in fact expired.
func (c *OAuthCredential) expired() bool {
	if c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(tokenRefreshLeeway).After(time.UnixMilli(c.ExpiresAt))
}

type claudeCredentialsFile struct {
	ClaudeAiOauth *OAuthCredential `json:"claudeAiOauth"`
}

// claudeConfigDir returns Claude Code's default config directory, honoring
// the same CLAUDE_CONFIG_DIR override Claude Code itself respects. This is
// the "default" profile's directory (see profiles.go): it MUST be reached
// with CLAUDE_CONFIG_DIR unset in normal operation, because an explicitly set
// value changes the derived Keychain service name and would break the user's
// existing login.
func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// cleanAbsoluteDirPath resolves dir to a canonical absolute, cleaned form so
// two different spellings of the same path (relative vs absolute, trailing
// slash, ".." segments) hash and compare identically. If the path cannot be
// made absolute (unusual — e.g. no working directory), dir is cleaned as-is;
// this only affects service-name derivation, never credential contents.
func cleanAbsoluteDirPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return filepath.Clean(abs)
}

// isDefaultConfigDir reports whether dir refers to the same directory as
// claudeConfigDir() (the default profile), after canonicalization.
func isDefaultConfigDir(dir string) bool {
	return cleanAbsoluteDirPath(dir) == cleanAbsoluteDirPath(claudeConfigDir())
}

// keychainServiceForDir derives the macOS Keychain service name Claude Code
// uses for the credential belonging to config directory dir. This mirrors
// Claude Code's own behavior (research-confirmed; see the derivation gist
// linked in the README's Profiles section):
//   - the default dir (~/.claude, CLAUDE_CONFIG_DIR unset) uses the plain
//     "Claude Code-credentials" service name unchanged;
//   - any other dir uses "Claude Code-credentials-<sha256(dir)[:8] hex>",
//     where dir is the cleaned absolute path.
//
// A derivation mismatch (e.g. a future Claude Code version changes its
// scheme) degrades to "needs login" via the file-fallback + typed-failure
// path in ReadClaudeCodeCredentialForDir — it never crashes.
func keychainServiceForDir(dir string) string {
	if isDefaultConfigDir(dir) {
		return keychainService
	}
	clean := cleanAbsoluteDirPath(dir)
	sum := sha256.Sum256([]byte(clean))
	return fmt.Sprintf("%s-%s", keychainService, hex.EncodeToString(sum[:])[:8])
}

// ReadClaudeCodeCredential reads Claude Code's existing OAuth credential for
// the default profile (~/.claude, or CLAUDE_CONFIG_DIR if set). Kept as a
// thin wrapper over ReadClaudeCodeCredentialForDir so existing callers
// (auth.go's LoginWithClaudeCode) are unaffected by multi-profile support.
func ReadClaudeCodeCredential() (*OAuthCredential, error) {
	return ReadClaudeCodeCredentialForDir(claudeConfigDir())
}

// ReadClaudeCodeCredentialForDir reads the OAuth credential belonging to the
// profile whose Claude Code config directory is dir. It tries the credentials
// file first (Linux, Windows, and older macOS), then the macOS Keychain using
// the service name derived from dir. A missing credential means Claude Code
// has not been logged in under this config dir, which callers treat as a
// typed "needs login" state — never a fatal error, and never a reason to
// abort other profiles' reads (ISC-4).
func ReadClaudeCodeCredentialForDir(dir string) (*OAuthCredential, error) {
	if cred, err := fileReaderForDir(dir); err == nil {
		return cred, nil
	}
	if runtime.GOOS == "darwin" {
		if cred, err := keychainReaderForDir(dir); err == nil {
			return cred, nil
		}
	}
	return nil, fmt.Errorf("no Claude Code credential found for %s — install Claude Code and log in once", dir)
}

func readCredentialFileAt(dir string) (*OAuthCredential, error) {
	path := filepath.Join(dir, ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCredential(data)
}

// readCredentialKeychainAt shells out to `security`, matching how the rest of
// the macOS ecosystem reads this item. The first read shows a Keychain
// prompt; once the user clicks "Always Allow" this binary is added to the
// item's ACL and subsequent reads are silent. Read-only: this never writes,
// updates, or deletes a Keychain item.
func readCredentialKeychainAt(dir string) (*OAuthCredential, error) {
	service := keychainServiceForDir(dir)
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return nil, err
	}
	return parseCredential(bytes.TrimSpace(out))
}

func parseCredential(data []byte) (*OAuthCredential, error) {
	var f claudeCredentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse credential: %w", err)
	}
	if f.ClaudeAiOauth == nil || f.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("credential has no claudeAiOauth.accessToken")
	}
	return f.ClaudeAiOauth, nil
}

// CredentialStatus is a coarse, typed summary of a profile's credential
// health — shared by the switch engine, the CLI, and (later) the poller —
// so no caller has to interpret a raw error to decide what to show the user.
type CredentialStatus string

const (
	// CredentialLoggedIn means a credential was read and is not expired.
	CredentialLoggedIn CredentialStatus = "logged-in"
	// CredentialExpired means a credential was read but is expired or within
	// the refresh leeway of expiring. Claude Code, not this program, will
	// refresh it; this is not an error condition for us.
	CredentialExpired CredentialStatus = "expired"
	// CredentialNeedsLogin means no credential could be read at all (never
	// logged in under this config dir, or Claude Code is not installed).
	CredentialNeedsLogin CredentialStatus = "needs-login"
)

// CredentialHealth reports dir's credential status without ever erroring —
// a read failure becomes CredentialNeedsLogin, never a propagated error. It
// performs no writes and is safe to call for any profile, active or not.
func CredentialHealth(dir string) CredentialStatus {
	cred, err := readCredential(dir)
	if err != nil {
		return CredentialNeedsLogin
	}
	if cred.expired() {
		return CredentialExpired
	}
	return CredentialLoggedIn
}

// CurrentOAuthToken returns a currently-valid Claude Code OAuth access token
// for the active profile (the default profile's dir when no profiles are
// configured). It is kept as a zero-argument function so it can still be
// passed directly as main.go's NewOAuthUsageClient token provider.
func CurrentOAuthToken() (string, error) {
	return CurrentOAuthTokenForDir(activeProfileDir())
}

// CurrentOAuthTokenForDir is CurrentOAuthToken parameterized by profile
// config dir. It never touches our own config file — the token is cached in
// memory only, for this process's lifetime, keyed per dir.
//
// Resolution order:
//  1. A cached, non-expired credential for dir is returned as-is.
//  2. Otherwise, that profile's own on-disk credential is re-read. Claude
//     Code refreshes its own token in the background, so a fresh read is
//     what keeps this working — we never spend Claude Code's refresh grant
//     ourselves, because rotating its refresh token could force the user to
//     sign back into Claude Code.
//  3. If that re-read credential is itself expired, we report a transient
//     error and wait for Claude Code to refresh it, rather than refreshing
//     on its behalf.
//
// Returns ErrTransient when the profile's credential is present but expired
// (Claude Code will refresh it shortly); returns the underlying read error if
// the profile's credential store could not be read at all (not installed,
// not logged in under that config dir).
func CurrentOAuthTokenForDir(dir string) (string, error) {
	oauthTokenMu.Lock()
	defer oauthTokenMu.Unlock()

	key := cleanAbsoluteDirPath(dir)

	if cred, ok := cachedCredentials[key]; ok && cred != nil && !cred.expired() {
		return cred.AccessToken, nil
	}

	cred, err := readCredential(dir)
	if err != nil {
		return "", err
	}
	if !cred.expired() {
		cachedCredentials[key] = cred
		return cred.AccessToken, nil
	}

	// This profile's own access token is expired. We deliberately do NOT
	// spend its refresh token: the OAuth server rotates refresh tokens on
	// use, so refreshing here would consume Claude Code's grant and could
	// force the user to sign back into Claude Code for that profile. Wait
	// for Claude Code to refresh its own credential and report transient so
	// the caller keeps last-good data.
	return "", fmt.Errorf("%w: Claude Code's access token is expired; waiting for it to refresh", ErrTransient)
}

// InvalidateOAuthToken drops the in-memory cached credential for the active
// profile. Call it after an API request rejects the current token (a 401),
// so the next CurrentOAuthToken call re-reads (and refreshes, if needed)
// instead of handing out the same token the server just refused.
func InvalidateOAuthToken() {
	InvalidateOAuthTokenForDir(activeProfileDir())
}

// InvalidateOAuthTokenForDir is InvalidateOAuthToken parameterized by profile
// config dir.
func InvalidateOAuthTokenForDir(dir string) {
	oauthTokenMu.Lock()
	defer oauthTokenMu.Unlock()
	delete(cachedCredentials, cleanAbsoluteDirPath(dir))
}
