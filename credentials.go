// credentials.go - Zero-paste auth by reusing Claude Code's existing login.
//
// Claude Code (and the wider Claude CLI ecosystem) stores a subscription OAuth
// token on disk after the user logs in. We read that credential and poll the
// same usage endpoint Claude Code uses, so the user never has to copy a session
// key out of the browser by hand. We never run our own login flow, never proxy
// inference, and never write back to Claude Code's credential store.
//
// The token itself is never persisted to our own config either: CurrentOAuthToken
// below reads it fresh from Claude Code's store on demand and caches it in
// memory only, for this process's lifetime. Writing a copy of Claude Code's
// broad-scope credential into our plaintext dotfile would be a downgrade from
// the Keychain and an unnecessary second copy of the same secret.
package main

import (
	"bytes"
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
	// credential under on newer versions.
	keychainService = "Claude Code-credentials"

	// tokenRefreshLeeway is how far before expiry a token is treated as stale,
	// so a poll never races the expiry boundary.
	tokenRefreshLeeway = 2 * time.Minute
)

// oauthTokenMu guards cachedOAuthCredential. CurrentOAuthToken and
// InvalidateOAuthToken are the only functions that may touch it. The cache
// lives in memory for this process only — it is never marshaled to our
// config file, so a compromised dotfile can no longer impersonate Claude
// Code's own broad-scope subscription credential.
var (
	oauthTokenMu          sync.Mutex
	cachedOAuthCredential *OAuthCredential

	// readCredential indirects ReadClaudeCodeCredential so tests can substitute
	// a fake without touching the real Keychain or credentials file.
	readCredential = ReadClaudeCodeCredential
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

// claudeConfigDir returns Claude Code's config directory, honoring the same
// CLAUDE_CONFIG_DIR override Claude Code itself respects.
func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// ReadClaudeCodeCredential reads Claude Code's existing OAuth credential. It
// tries the credentials file first (Linux, Windows, and older macOS), then the
// macOS Keychain. A missing credential means Claude Code is not installed or
// not logged in, which callers treat as a cue to fall back to manual entry.
func ReadClaudeCodeCredential() (*OAuthCredential, error) {
	if cred, err := readCredentialFile(); err == nil {
		return cred, nil
	}
	if runtime.GOOS == "darwin" {
		if cred, err := readCredentialKeychain(); err == nil {
			return cred, nil
		}
	}
	return nil, fmt.Errorf("no Claude Code credential found — install Claude Code and log in once")
}

func readCredentialFile() (*OAuthCredential, error) {
	path := filepath.Join(claudeConfigDir(), ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCredential(data)
}

// readCredentialKeychain shells out to `security`, matching how the rest of the
// macOS ecosystem reads this item. The first read shows a Keychain prompt; once
// the user clicks "Always Allow" this binary is added to the item's ACL and
// subsequent reads are silent.
func readCredentialKeychain() (*OAuthCredential, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
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

// CurrentOAuthToken returns a currently-valid Claude Code OAuth access token
// for the calling request. It never touches our own config file — the token
// is cached in memory only, for this process's lifetime.
//
// Resolution order:
//  1. A cached, non-expired credential is returned as-is.
//  2. Otherwise, Claude Code's own on-disk credential is re-read. Claude Code
//     refreshes its own token in the background, so a fresh read is what keeps
//     this working — we never spend Claude Code's refresh grant ourselves,
//     because rotating its refresh token could force the user to sign back
//     into Claude Code.
//  3. If that re-read credential is itself expired, we report a transient
//     error and wait for Claude Code to refresh it, rather than refreshing on
//     its behalf.
//
// Returns ErrTransient when Claude Code's credential is present but expired
// (Claude Code will refresh it shortly); returns the underlying read error if
// Claude Code's credential store could not be read at all (not installed, not
// logged in).
func CurrentOAuthToken() (string, error) {
	oauthTokenMu.Lock()
	defer oauthTokenMu.Unlock()

	if cachedOAuthCredential != nil && !cachedOAuthCredential.expired() {
		return cachedOAuthCredential.AccessToken, nil
	}

	cred, err := readCredential()
	if err != nil {
		return "", err
	}
	if !cred.expired() {
		cachedOAuthCredential = cred
		return cred.AccessToken, nil
	}

	// Claude Code's own access token is expired. We deliberately do NOT spend
	// its refresh token: the OAuth server rotates refresh tokens on use, so
	// refreshing here would consume Claude Code's grant and could force the
	// user to sign back into Claude Code. Wait for Claude Code to refresh its
	// own credential and report transient so the caller keeps last-good data.
	return "", fmt.Errorf("%w: Claude Code's access token is expired; waiting for it to refresh", ErrTransient)
}

// InvalidateOAuthToken drops the in-memory cached credential. Call it after an
// API request rejects the current token (a 401), so the next CurrentOAuthToken
// call re-reads (and refreshes, if needed) instead of handing out the same
// token the server just refused.
func InvalidateOAuthToken() {
	oauthTokenMu.Lock()
	defer oauthTokenMu.Unlock()
	cachedOAuthCredential = nil
}
