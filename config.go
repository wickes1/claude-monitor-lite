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
	AutoRenew        AutoRenew  `json:"autoRenew,omitempty"`
}

type AutoRenew struct {
	Enabled  bool              `json:"enabled"`
	Schedule []string          `json:"schedule"` // e.g., ["09:00", "14:00"]
	Message  string            `json:"message"`  // e.g., "hello"
	LastSent map[string]string `json:"lastSent"` // e.g., {"09:00": "2025-01-28"}
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
	return os.WriteFile(GetConfigPath(), data, configFilePermissions)
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

// SaveAutoRenewConfig updates only autoRenew settings, preserving other fields
func SaveAutoRenewConfig(autoRenew AutoRenew) error {
	return modifyAndSaveConfig(func(c *Config) {
		c.AutoRenew = autoRenew
	})
}

// UpdateAutoRenewLastSent updates only the lastSent map for auto-renew
func UpdateAutoRenewLastSent(scheduleTime, date string) error {
	return modifyAndSaveConfig(func(c *Config) {
		if c.AutoRenew.LastSent == nil {
			c.AutoRenew.LastSent = make(map[string]string)
		}
		c.AutoRenew.LastSent[scheduleTime] = date
	})
}
