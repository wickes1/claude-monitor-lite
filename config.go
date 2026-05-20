package main

import (
	"encoding/json"
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
	SessionKey       string     `json:"sessionKey,omitempty"`
	OrganizationID   string     `json:"organizationId,omitempty"`
	SavedAt          *time.Time `json:"savedAt,omitempty"`
	MenuBarIndicator string     `json:"menuBarIndicator"`
}

func GetConfigPath() string {
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

	switch config.MenuBarIndicator {
	case "currentSession", "weeklyAll", "weeklySonnet", "weeklyDesign":
		// valid
	default:
		config.MenuBarIndicator = "currentSession"
	}

	return config
}

// modifyAndSaveConfig reads existing config, applies modifier function, and saves.
func modifyAndSaveConfig(modifier func(*Config)) error {
	return modifyAndSaveConfigAt(GetConfigPath(), modifier)
}

// modifyAndSaveConfigAt performs a serialized, atomic read-modify-write of the
// config at path.
func modifyAndSaveConfigAt(path string, modifier func(*Config)) error {
	configWriteMutex.Lock()
	defer configWriteMutex.Unlock()

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
