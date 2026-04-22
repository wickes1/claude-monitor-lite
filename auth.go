package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type AuthSession struct {
	SessionKey     string    `json:"sessionKey"`
	OrganizationID string    `json:"organizationId,omitempty"`
	SavedAt        time.Time `json:"savedAt"`
}

func LoadAuthSession() (*AuthSession, error) {
	config := LoadConfig()
	if config.SessionKey == "" {
		return nil, fmt.Errorf("no session found")
	}

	savedAt := time.Time{}
	if config.SavedAt != nil {
		savedAt = *config.SavedAt
	}

	return &AuthSession{
		SessionKey:     config.SessionKey,
		OrganizationID: config.OrganizationID,
		SavedAt:        savedAt,
	}, nil
}

func SaveAuthSession(session *AuthSession) error {
	session.SavedAt = time.Now()

	// Only update auth fields, preserving other settings (menuBarIndicator)
	return modifyAndSaveConfig(func(c *Config) {
		c.SessionKey = session.SessionKey
		c.OrganizationID = session.OrganizationID
		c.SavedAt = &session.SavedAt
	})
}

func ClearAuthSession() error {
	// Only clear auth-related fields, preserve other settings (menuBarIndicator)
	return modifyAndSaveConfig(func(c *Config) {
		c.SessionKey = ""
		c.OrganizationID = ""
		c.SavedAt = nil
	})
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
