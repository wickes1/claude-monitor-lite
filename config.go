package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	configFilePermissions = 0600 // Owner read/write only
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

// modifyAndSaveConfig reads existing config, applies modifier function, and saves
func modifyAndSaveConfig(modifier func(*Config)) error {
	path := GetConfigPath()
	existingData, err := os.ReadFile(path)

	var config Config
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingData, &config); unmarshalErr != nil {
			config = Config{}
		}
	}

	modifier(&config)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, configFilePermissions)
}

// SaveConfigPreservingSession updates only menuBarIndicator, preserving other fields
func SaveConfigPreservingSession(menuBarIndicator string) error {
	return modifyAndSaveConfig(func(c *Config) {
		c.MenuBarIndicator = menuBarIndicator
	})
}

