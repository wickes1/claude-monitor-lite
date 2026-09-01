package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	configFilePermissions = 0600 // Owner read/write only
)

// configWriteMutex serializes config read-modify-write cycles so concurrent
// writers cannot lose each other's updates or interleave their file writes.
var configWriteMutex sync.Mutex

type Config struct {
	// AuthMode selects how we authenticate: "oauth" (reuse Claude Code's login,
	// the zero-paste default) or "cookie" (legacy manual sessionKey paste).
	//
	// The OAuth access/refresh token itself is never stored here. It lives in
	// Claude Code's own credential store (Keychain on macOS, .credentials.json
	// elsewhere) and is read fresh, in memory only, at fetch time — see
	// CurrentOAuthToken in credentials.go. Persisting a copy here would
	// duplicate a broad-scope credential into a plaintext dotfile, a downgrade
	// from the Keychain.
	AuthMode string `json:"authMode,omitempty"`

	// Legacy cookie auth.
	SessionKey     string `json:"sessionKey,omitempty"`
	OrganizationID string `json:"organizationId,omitempty"`

	SavedAt          *time.Time `json:"savedAt,omitempty"`
	MenuBarIndicator string     `json:"menuBarIndicator"`
}

// configPathOverride, when non-empty, is used instead of the default
// ~/.claude-monitor-lite.json path. Tests set this to a temp file so they
// never read or write the real config.
var configPathOverride string

func GetConfigPath() string {
	if configPathOverride != "" {
		return configPathOverride
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".claude-monitor-lite.json")
}

func LoadConfig() Config {
	return loadConfigFrom(GetConfigPath())
}

// loadConfigFrom reads and validates a config file, falling back to defaults
// if the file is missing or unreadable.
func loadConfigFrom(path string) Config {
	defaultConfig := Config{MenuBarIndicator: "currentSession"}

	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return defaultConfig
	}

	// Fall back to the default for an unknown or stale indicator key.
	if !isValidIndicator(config.MenuBarIndicator) {
		config.MenuBarIndicator = "currentSession"
	}

	return config
}

// modifyAndSaveConfig reads existing config, applies modifier function, and saves.
func modifyAndSaveConfig(modifier func(*Config)) error {
	return modifyAndSaveConfigAt(GetConfigPath(), modifier)
}

// modifyAndSaveConfigAt performs a serialized, atomic read-modify-write of the
// config at path. It holds both the in-process configWriteMutex and a
// cross-process advisory file lock (config_lock_unix.go) for the whole
// cycle, so the daemon (a menu click) and a CLI invocation running as
// separate OS processes can never clobber each other's update.
func modifyAndSaveConfigAt(path string, modifier func(*Config)) error {
	configWriteMutex.Lock()
	defer configWriteMutex.Unlock()

	release, err := acquireConfigLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("acquire config lock: %w", err)
	}
	defer release()

	var config Config
	if existingData, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(existingData, &config) != nil {
			config = Config{}
		}
	}

	modifier(&config)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, configFilePermissions)
}

// writeFileAtomic writes data to a temp file in the same directory and renames
// it over path. The rename is atomic, so a crash mid-write or a concurrent
// reader can never observe a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude-monitor-lite-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// SaveConfigPreservingSession updates only menuBarIndicator, preserving other fields.
func SaveConfigPreservingSession(menuBarIndicator string) error {
	return modifyAndSaveConfig(func(c *Config) {
		c.MenuBarIndicator = menuBarIndicator
	})
}
