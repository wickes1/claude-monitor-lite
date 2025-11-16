package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

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
	defaultConfig := Config{MenuBarIndicator: "currentSession"}

	data, err := os.ReadFile(GetConfigPath())
	if err != nil {
		return defaultConfig
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return defaultConfig
	}

	if config.MenuBarIndicator == "" {
		config.MenuBarIndicator = "currentSession"
	}

	return config
}

func SaveConfig(config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(GetConfigPath(), data, 0600)
}

// SaveConfigPreservingSession updates only menuBarIndicator, preserving session fields
func SaveConfigPreservingSession(menuBarIndicator string) error {
	// Read the current file to preserve session fields
	path := GetConfigPath()
	existingData, err := os.ReadFile(path)

	var existing Config
	if err == nil {
		// File exists, parse it to preserve session fields
		// If unmarshal fails, existing will be zero-valued (safe)
		if unmarshalErr := json.Unmarshal(existingData, &existing); unmarshalErr != nil {
			// On parse error, start fresh with just the menuBarIndicator
			existing = Config{}
		}
	}

	// Only update the menuBarIndicator
	existing.MenuBarIndicator = menuBarIndicator

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
