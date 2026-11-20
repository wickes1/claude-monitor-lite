// credentials.go - Zero-paste auth by reusing Claude Code's existing login.
//
// We read the OAuth credential Claude Code stores after the user logs in and
// poll the same usage endpoint Claude Code uses. We never run our own login
// flow, never refresh the token, and never write to Claude Code's credential
// store.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// OAuthCredential is the part of Claude Code's claudeAiOauth block we use.
type OAuthCredential struct {
	AccessToken string   `json:"accessToken"`
	ExpiresAt   int64    `json:"expiresAt"` // epoch milliseconds
	Scopes      []string `json:"scopes"`
}

// profileScope is the OAuth scope the usage endpoint requires. A claude.ai
// subscription login has it; an inference-only token does not.
const profileScope = "user:profile"

// expired reports whether the access token is within the refresh leeway of (or
// past) its expiry. An unknown expiry (0) is treated as usable; a 401 then
// triggers a re-read.
func (c *OAuthCredential) expired() bool {
	if c.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(tokenRefreshLeeway).After(time.UnixMilli(c.ExpiresAt))
}

// readCredential reads Claude Code's OAuth credential for this profile from
// where Claude Code keeps it: the Keychain on macOS, <config dir>/.credentials.json
// on Linux. A missing credential means Claude Code is not logged in there.
func (p profile) readCredential() (*OAuthCredential, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", p.keychainService(), "-w").Output()
		if err != nil {
			return nil, fmt.Errorf("no Claude Code login in Keychain item %q — log in to Claude Code first", p.keychainService())
		}
		return parseCredential(bytes.TrimSpace(out))
	}
	path := filepath.Join(p.configDir(), ".credentials.json")
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
		return nil, fmt.Errorf("Claude Code is not logged in with a Claude subscription — an API key or Console login has no plan limits to show")
	}
	// Without the profile scope the login cannot read plan usage; saying so
	// beats polling into a permission error.
	if !slices.Contains(f.ClaudeAiOauth.Scopes, profileScope) {
		return nil, fmt.Errorf("Claude Code's login lacks the %s scope, so it cannot read plan usage — log in to Claude Code with your Claude subscription", profileScope)
	}
	return f.ClaudeAiOauth, nil
}

// ErrTokenExpired is the transient failure of an environment Claude Code has
// not run in for a while: its access token ran out and nothing has renewed it.
var ErrTokenExpired = fmt.Errorf("%w: Claude Code's access token is expired; waiting for it to refresh", ErrTransient)

// credentialSource hands out one profile's access token, cached in memory
// only, for this process's lifetime — never copied to our own config.
type credentialSource struct {
	mu     sync.Mutex
	cached *OAuthCredential
	// read fetches Claude Code's credential; a field so tests can substitute a
	// fake without touching the real Keychain or credentials file.
	read func() (*OAuthCredential, error)
}

// token returns a currently-valid Claude Code OAuth access token: the cached
// one while it is unexpired, otherwise a fresh read of Claude Code's
// credential. Claude Code refreshes its own token in the background, so a
// fresh read is what keeps this working.
//
// If the fresh read is itself expired we report ErrTokenExpired and wait. We
// never spend the refresh token: the OAuth server rotates refresh tokens on
// use, so refreshing here would consume Claude Code's grant and could force
// the user to sign back into Claude Code.
func (s *credentialSource) token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && !s.cached.expired() {
		return s.cached.AccessToken, nil
	}

	cred, err := s.read()
	if err != nil {
		return "", err
	}
	if cred.expired() {
		return "", ErrTokenExpired
	}
	s.cached = cred
	return cred.AccessToken, nil
}

// invalidate drops the cached credential. Call it after the server rejects
// the current token (a 401), so the next token re-reads instead of handing
// out the token the server just refused.
func (s *credentialSource) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = nil
}
