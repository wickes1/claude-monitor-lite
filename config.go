package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const configFilePermissions = 0600 // Owner read/write only

// Config is the monitor's own settings — only which metric the menu bar shows:
// a row key (rows.go), or autoKey to follow the server's pick. Credentials are
// never stored here (see credentials.go).
type Config struct {
	MenuBarIndicator string `json:"menuBarIndicator"`
}

var unsafeInstanceChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// instanceName is the readable name of the Claude Code environment this monitor
// watches — the CLAUDE_CONFIG_DIR folder name, shown in the menu bar. Empty for
// the default ~/.claude environment.
func instanceName() string {
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		return ""
	}
	name := unsafeInstanceChars.ReplaceAllString(filepath.Base(absConfigDir()), "-")
	if name = strings.Trim(name, "-"); name == "" {
		return "custom"
	}
	return name
}

// instanceID makes the name unique per environment, so one monitor per
// CLAUDE_CONFIG_DIR can run side by side without sharing a PID file, config,
// log, or LaunchAgent — even when two dirs share a folder name.
func instanceID() string {
	if name := instanceName(); name != "" {
		return name + "-" + configDirHash()
	}
	return ""
}

// instanceFile returns this instance's dotfile in the home directory:
// ~/.claude-monitor-lite<ext> by default, ~/.claude-monitor-lite.<id><ext>
// for a CLAUDE_CONFIG_DIR instance.
func instanceFile(ext string) string {
	homeDir, _ := os.UserHomeDir()
	base := ".claude-monitor-lite"
	if id := instanceID(); id != "" {
		base += "." + id
	}
	return filepath.Join(homeDir, base+ext)
}

func GetConfigPath() string {
	return instanceFile(".json")
}

// LoadConfig reads the config, falling back to following the server's pick
// when the file is missing, unreadable, or names no row. A key is not checked
// against the rows here: which rows exist is only known once usage arrives,
// and headlineRow handles a pinned row the account does not have.
func LoadConfig() Config {
	var config Config
	if data, err := os.ReadFile(GetConfigPath()); err == nil {
		_ = json.Unmarshal(data, &config)
	}
	if config.MenuBarIndicator == "" {
		config.MenuBarIndicator = autoKey
	}
	return config
}

func SaveConfig(config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(GetConfigPath(), data, configFilePermissions)
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
