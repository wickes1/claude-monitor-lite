package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

const configFilePermissions = 0600 // Owner read/write only

// Config is the monitor's own settings: which Claude Code environments it
// watches and which metric the menu bar shows. Credentials are never stored
// here (see credentials.go).
type Config struct {
	// Profiles lists the config dirs to watch, in menu order. An empty string
	// is the default environment (profile.go).
	Profiles         []string  `json:"profiles"`
	MenuBarIndicator Indicator `json:"menuBarIndicator"`
}

// Indicator picks the row the menu bar shows: a row key (rows.go) of the
// profile at Dir, or autoKey to follow the server's pick for that profile.
// It starts as autoKey on the first profile and stays there until a row is
// clicked: which rows a plan has is not known before its usage arrives, so
// no row could be the default.
type Indicator struct {
	Dir string `json:"dir"`
	Key string `json:"key"`
}

// profiles returns the watched profiles in menu order.
func (c Config) profiles() []profile {
	profiles := make([]profile, len(c.Profiles))
	for i, dir := range c.Profiles {
		profiles[i] = newProfile(dir)
	}
	return profiles
}

// monitorFile returns one of the monitor's own dotfiles in the home
// directory, ~/.claude-monitor-lite<ext>: the config, the PID file and the
// log, of which there is one however many profiles are watched.
func monitorFile(ext string) string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".claude-monitor-lite"+ext)
}

func GetConfigPath() string {
	return monitorFile(".json")
}

// LoadConfig reads the config. A missing or unreadable file, or one that
// names no profile, watches the default environment; a missing or unknown
// selection follows the server's pick for the first profile. A row key is not
// checked against the rows here: which rows exist is only known once usage
// arrives, and headlineRow handles a pinned row the account does not have.
func LoadConfig() Config {
	var config Config
	if data, err := os.ReadFile(GetConfigPath()); err == nil {
		_ = json.Unmarshal(data, &config)
	}
	// One group per environment: the same dir listed twice would poll one
	// account twice and show it twice.
	var dirs []string
	for _, p := range config.profiles() {
		if !slices.Contains(dirs, p.dir) {
			dirs = append(dirs, p.dir)
		}
	}
	if len(dirs) == 0 {
		dirs = []string{""}
	}
	config.Profiles = dirs
	if config.MenuBarIndicator.Key == "" || !slices.Contains(dirs, config.MenuBarIndicator.Dir) {
		config.MenuBarIndicator = Indicator{Dir: dirs[0], Key: autoKey}
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
