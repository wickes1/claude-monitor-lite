// credentials.go - Zero-paste auth by reusing Claude Code's existing login.
//
// Claude Code (and the wider Claude CLI ecosystem) stores a subscription OAuth
// token on disk after the user logs in. We read that credential and poll the
// same usage endpoint Claude Code uses, so the user never has to copy a session
// key out of the browser by hand. We never run our own login flow, never proxy
// inference, and never write back to Claude Code's credential store.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// claudeOAuthClientID is the public client_id shared across the Claude Code
	// CLI ecosystem. It is used only to refresh an access token we already
	// obtained from Claude Code's own on-disk credential.
	claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"

	// keychainService is the macOS Keychain item Claude Code stores its
	// credential under on newer versions.
	keychainService = "Claude Code-credentials"

	// tokenRefreshLeeway is how far before expiry a token is treated as stale,
	// so a poll never races the expiry boundary.
	tokenRefreshLeeway = 2 * time.Minute
)

// OAuthCredential is the claudeAiOauth block Claude Code persists.
type OAuthCredential struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"` // epoch milliseconds
	SubscriptionType string `json:"subscriptionType,omitempty"`
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

// RefreshAccessToken exchanges a refresh token for a fresh access token via the
// public OAuth token endpoint. The result is persisted to OUR config only; we
// never write back to Claude Code's store. This is a last-resort path: callers
// prefer re-reading Claude Code's (self-refreshing) credential first, since a
// provider may rotate the refresh token and we must not invalidate Claude
// Code's own session.
func RefreshAccessToken(refreshToken string) (*OAuthCredential, error) {
	payload, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeOAuthClientID,
	})

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", claudeOAuthTokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-monitor-lite/"+version)

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrAuthFailed
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: token refresh status %d: %s", ErrTransient, resp.StatusCode, string(body))
	}

	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: decode refresh response: %v", ErrTransient, err)
	}
	if r.AccessToken == "" {
		return nil, fmt.Errorf("token refresh returned an empty access_token")
	}

	cred := &OAuthCredential{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli(),
	}
	// Some providers omit a rotated refresh token; keep the existing one so a
	// future refresh still has something to present.
	if cred.RefreshToken == "" {
		cred.RefreshToken = refreshToken
	}
	return cred, nil
}
