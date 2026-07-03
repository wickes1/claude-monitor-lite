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

type AuthSession struct {
	AuthMode string `json:"authMode,omitempty"`

	// OAuth credential (AuthModeOAuth).
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // epoch milliseconds

	// Cookie credential (AuthModeCookie).
	SessionKey     string `json:"sessionKey,omitempty"`
	OrganizationID string `json:"organizationId,omitempty"`

	SavedAt time.Time `json:"savedAt"`
}

func LoadAuthSession() (*AuthSession, error) {
	config := LoadConfig()

	mode := config.AuthMode
	if mode == "" {
		// Infer the mode for configs written before authMode existed.
		switch {
		case config.AccessToken != "":
			mode = AuthModeOAuth
		case config.SessionKey != "":
			mode = AuthModeCookie
		}
	}

	switch mode {
	case AuthModeOAuth:
		if config.AccessToken == "" {
			return nil, fmt.Errorf("no session found")
		}
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
		AccessToken:    config.AccessToken,
		RefreshToken:   config.RefreshToken,
		ExpiresAt:      config.ExpiresAt,
		SessionKey:     config.SessionKey,
		OrganizationID: config.OrganizationID,
		SavedAt:        savedAt,
	}, nil
}

func SaveAuthSession(session *AuthSession) error {
	session.SavedAt = time.Now()

	// Only update auth fields, preserving other settings (menuBarIndicator)
	return modifyAndSaveConfig(func(c *Config) {
		c.AuthMode = session.AuthMode
		c.AccessToken = session.AccessToken
		c.RefreshToken = session.RefreshToken
		c.ExpiresAt = session.ExpiresAt
		c.SessionKey = session.SessionKey
		c.OrganizationID = session.OrganizationID
		c.SavedAt = &session.SavedAt
	})
}

func ClearAuthSession() error {
	// Only clear auth-related fields, preserve other settings (menuBarIndicator)
	return modifyAndSaveConfig(func(c *Config) {
		c.AuthMode = ""
		c.AccessToken = ""
		c.RefreshToken = ""
		c.ExpiresAt = 0
		c.SessionKey = ""
		c.OrganizationID = ""
		c.SavedAt = nil
	})
}

// LoginWithClaudeCode reads Claude Code's existing OAuth credential and saves it
// as our session — the zero-paste path. Returns an error if Claude Code is not
// installed or not logged in, so callers can fall back to manual entry.
func LoginWithClaudeCode() (*AuthSession, error) {
	cred, err := ReadClaudeCodeCredential()
	if err != nil {
		return nil, err
	}

	session := &AuthSession{
		AuthMode:     AuthModeOAuth,
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		ExpiresAt:    cred.ExpiresAt,
	}
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
