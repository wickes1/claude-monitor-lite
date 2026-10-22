// credentials.go - Zero-paste auth by reusing Claude Code's existing login.
//
// We read the OAuth credential Claude Code stores after the user logs in and
// poll the same usage endpoint Claude Code uses. We never run our own login
// flow, never refresh the token, and never write to Claude Code's credential
// store. The token is cached in memory only, for this process's lifetime —
// never copied to our own config.
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
	// keychainServiceBase is the macOS Keychain item Claude Code stores its
	// credential under (see keychainService for the CLAUDE_CONFIG_DIR form).
	keychainServiceBase = "Claude Code-credentials"

	// tokenRefreshLeeway is how far before expiry a token is treated as stale,
	// so a poll never races the expiry boundary.
	tokenRefreshLeeway = 2 * time.Minute
)

var (
	// oauthTokenMu guards cachedOAuthCredential.
	oauthTokenMu          sync.Mutex
	cachedOAuthCredential *OAuthCredential

	// readCredential indirects ReadClaudeCodeCredential so tests can substitute
	// a fake without touching the real Keychain or credentials file.
	readCredential = ReadClaudeCodeCredential
)

// OAuthCredential is the part of Claude Code's claudeAiOauth block we use.
type OAuthCredential struct {
	AccessToken string `json:"accessToken"`
	ExpiresAt   int64  `json:"expiresAt"` // epoch milliseconds
}

// expired reports whether the access token is within the refresh leeway of (or
// past) its expiry. An unknown expiry (0) is treated as usable; a 401 then
// triggers a re-read.
func (c *OAuthCredential) expired() bool {
	if c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(tokenRefreshLeeway).After(time.UnixMilli(c.ExpiresAt))
}

// claudeConfigDir returns Claude Code's config directory, honoring the same
// CLAUDE_CONFIG_DIR override Claude Code itself respects — so running the
// monitor from a shell that has it set monitors that environment's login.
func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// keychainService mirrors how Claude Code names its Keychain item: the plain
// name when CLAUDE_CONFIG_DIR is unset, and
// "<name>-<sha256(cleaned absolute dir)[:8] hex>" whenever it is set — even if
// it points at ~/.claude. Reading the plain item for a CLAUDE_CONFIG_DIR
// environment would silently report the default account's usage instead.
func keychainService() string {
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		return keychainServiceBase
	}
	return keychainServiceBase + "-" + configDirHash()
}

// configDirHash is Claude Code's 8-hex-char hash of the cleaned absolute
// CLAUDE_CONFIG_DIR. It identifies the environment however the path was
// spelled (relative, trailing slash, ".." segments).
func configDirHash() string {
	sum := sha256.Sum256([]byte(absConfigDir()))
	return hex.EncodeToString(sum[:])[:8]
}

func absConfigDir() string {
	dir := claudeConfigDir()
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Clean(dir)
}

// ReadClaudeCodeCredential reads Claude Code's OAuth credential from where
// Claude Code keeps it: the Keychain on macOS, <config dir>/.credentials.json
// on Linux. A missing credential means Claude Code is not logged in.
func ReadClaudeCodeCredential() (*OAuthCredential, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", keychainService(), "-w").Output()
		if err != nil {
			return nil, fmt.Errorf("no Claude Code login in Keychain item %q — log in to Claude Code first", keychainService())
		}
		return parseCredential(bytes.TrimSpace(out))
	}
	path := filepath.Join(claudeConfigDir(), ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no Claude Code login at %s — log in to Claude Code first", path)
	}
	return parseCredential(data)
}

func parseCredential(data []byte) (*OAuthCredential, error) {
	var f struct {
		ClaudeAiOauth *OAuthCredential `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse credential: %w", err)
	}
	if f.ClaudeAiOauth == nil || f.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("credential has no claudeAiOauth.accessToken")
	}
	return f.ClaudeAiOauth, nil
}

// CurrentOAuthToken returns a currently-valid Claude Code OAuth access token:
// the cached one while it is unexpired, otherwise a fresh read of Claude
// Code's credential. Claude Code refreshes its own token in the background,
// so a fresh read is what keeps this working.
//
// If the fresh read is itself expired we report ErrTransient and wait. We
// never spend the refresh token: the OAuth server rotates refresh tokens on
// use, so refreshing here would consume Claude Code's grant and could force
// the user to sign back into Claude Code.
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
	if cred.expired() {
		return "", fmt.Errorf("%w: Claude Code's access token is expired; waiting for it to refresh", ErrTransient)
	}
	cachedOAuthCredential = cred
	return cred.AccessToken, nil
}

// InvalidateOAuthToken drops the cached credential. Call it after the server
// rejects the current token (a 401), so the next CurrentOAuthToken re-reads
// instead of handing out the token the server just refused.
func InvalidateOAuthToken() {
	oauthTokenMu.Lock()
	defer oauthTokenMu.Unlock()
	cachedOAuthCredential = nil
}
