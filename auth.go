package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Authentication modes.
const (
	AuthModeOAuth  = "oauth"  // reuse Claude Code's login (zero-paste, default)
	AuthModeCookie = "cookie" // legacy manual sessionKey paste
)

// AuthSession is an in-memory transfer struct passed between the login flow and
// the client; it is never serialized. Config (config.go) holds the on-disk
// contract, so these fields carry no json tags.
type AuthSession struct {
	AuthMode string

	// Cookie credential (AuthModeCookie).
	SessionKey     string
	OrganizationID string

	SavedAt time.Time
}

func LoadAuthSession() (*AuthSession, error) {
	config := LoadConfig()

	mode := config.AuthMode
	if mode == "" && config.SessionKey != "" {
		// Infer the mode for configs written before authMode existed.
		mode = AuthModeCookie
	}

	switch mode {
	case AuthModeOAuth:
		// Credential availability (Claude Code installed and logged in) is
		// checked lazily at fetch time via CurrentOAuthToken, not here — that
		// keeps this check cheap and keeps it from touching the Keychain just
		// to report whether a session exists.
	case AuthModeCookie:
		if config.SessionKey == "" {
			return nil, fmt.Errorf("no session found")
		}
	default:
		return nil, fmt.Errorf("no session found")
	}

	savedAt := time.Time{}
	if config.SavedAt != nil {
		savedAt = *config.SavedAt
	}

	return &AuthSession{
		AuthMode:       mode,
		SessionKey:     config.SessionKey,
		OrganizationID: config.OrganizationID,
		SavedAt:        savedAt,
	}, nil
}

func SaveAuthSession(session *AuthSession) error {
	session.SavedAt = time.Now()

	// Only update auth fields, preserving other settings (menuBarIndicator).
	// Deliberately no token fields: the OAuth access/refresh token is never
	// written to this config — see the AuthMode field doc on Config.
	return modifyAndSaveConfig(func(c *Config) {
		c.AuthMode = session.AuthMode
		c.SessionKey = session.SessionKey
		c.OrganizationID = session.OrganizationID
		c.SavedAt = &session.SavedAt
	})
}

func ClearAuthSession() error {
	// Only clear auth-related fields, preserve other settings (menuBarIndicator)
	return modifyAndSaveConfig(func(c *Config) {
		c.AuthMode = ""
		c.SessionKey = ""
		c.OrganizationID = ""
		c.SavedAt = nil
	})
}

// LoginWithClaudeCode verifies Claude Code has a usable login and records that
// we should use it — the zero-paste path. No token is stored: the OAuth
// access/refresh token stays in Claude Code's own credential store (Keychain
// on macOS, .credentials.json elsewhere) and is read fresh, in memory only,
// at fetch time via CurrentOAuthToken (credentials.go). Returns an error if
// Claude Code is not installed or not logged in, so callers can fall back to
// manual entry.
func LoginWithClaudeCode() (*AuthSession, error) {
	if _, err := ReadClaudeCodeCredential(); err != nil {
		return nil, err
	}

	session := &AuthSession{AuthMode: AuthModeOAuth}
	if err := SaveAuthSession(session); err != nil {
		return nil, fmt.Errorf("failed to save session: %w", err)
	}
	return session, nil
}

// LoginWithBrowser opens browser and guides user through manual session key extraction
func LoginWithBrowser() (*AuthSession, error) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║           Claude Monitor Lite - Authentication            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Print("Press Enter to open browser...")
	fmt.Scanln()

	return extractSessionManually()
}

// extractSessionManually guides user through manual extraction
func extractSessionManually() (*AuthSession, error) {
	// Open browser to Claude
	url := "https://claude.ai"
	var err error

	switch runtime.GOOS {
	case "darwin":
		err = exec.Command("open", url).Start()
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return nil, fmt.Errorf("unsupported platform")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to open browser: %w", err)
	}

	fmt.Println()
	fmt.Println("Browser opened. Please follow these steps:")
	fmt.Println()
	fmt.Println("  1. Login to Claude if not already logged in")
	fmt.Println("  2. Open DevTools (F12 or Cmd+Option+I on Mac)")
	fmt.Println("  3. Go to: Application tab → Cookies → https://claude.ai")
	fmt.Println("  4. Find the 'sessionKey' cookie")
	fmt.Println("  5. Double-click the Value to select it, then copy (Cmd+C)")
	fmt.Println()
	fmt.Print("Paste your sessionKey here: ")

	var sessionKey string
	if _, err := fmt.Scanln(&sessionKey); err != nil {
		return nil, fmt.Errorf("failed to read session key: %w", err)
	}

	if sessionKey == "" {
		return nil, fmt.Errorf("no session key provided")
	}

	// Clean up the session key (remove quotes, whitespace)
	sessionKey = strings.TrimSpace(sessionKey)
	sessionKey = strings.Trim(sessionKey, "\"'")

	session := &AuthSession{
		AuthMode:   AuthModeCookie,
		SessionKey: sessionKey,
	}

	if err := SaveAuthSession(session); err != nil {
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	fmt.Println()
	fmt.Println("✓ Session saved successfully!")
	fmt.Println()

	return session, nil
}
