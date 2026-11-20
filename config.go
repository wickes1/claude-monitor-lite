package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// LoadConfig reads the config. A missing file, or one that names no profile,
// watches the default environment; a missing or unknown selection follows the
// server's pick for the first profile. A row key is not checked against the
// rows here: which rows exist is only known once usage arrives, and
// headlineRow handles a pinned row the account does not have.
//
// A file that is there but cannot be used is an error, not a reason to fall
// back: the config is saved whole on every change, so carrying on with
// defaults would overwrite the list of profiles the user wrote.
func LoadConfig() (Config, error) {
	var config Config
	path := GetConfigPath()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return config, err
	}
	if err == nil {
		// A field of an unexpected type is left at its default and the rest
		// is kept; anything else means the file is not the JSON it should be.
		var wrongType *json.UnmarshalTypeError
		if err := json.Unmarshal(data, &config); err != nil && !errors.As(err, &wrongType) {
			return config, fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	}

	// One group per environment: the same dir listed twice would poll one
	// account twice and show it twice.
	var dirs []string
	for _, dir := range config.Profiles {
		if rest, ok := strings.CutPrefix(dir, "~/"); ok {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, rest)
		}
		// The monitor starts from launchd, in /: a relative dir would resolve
		// to somewhere the user never meant.
		if dir != "" && !filepath.IsAbs(dir) {
			return config, fmt.Errorf("%s: profile %q must be an absolute path", path, dir)
		}
		if p := newProfile(dir); !slices.Contains(dirs, p.dir) {
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
	return config, nil
}

var (
	errNotWatched  = errors.New("not watched")
	errOnlyProfile = errors.New("the only environment watched")
)

// watch adds a profile to the end of the list and reports whether it was new.
func (c *Config) watch(dir string) bool {
	if slices.Contains(c.Profiles, dir) {
		return false
	}
	c.Profiles = append(c.Profiles, dir)
	return true
}

// unwatch takes a profile off the list. The last one cannot go: there would
// be nothing left to show. A menu bar selection that pointed at the removed
// profile returns to the first one's pick.
func (c *Config) unwatch(dir string) error {
	idx := slices.Index(c.Profiles, dir)
	switch {
	case idx < 0:
		return errNotWatched
	case len(c.Profiles) == 1:
		return errOnlyProfile
	}
	c.Profiles = slices.Delete(c.Profiles, idx, idx+1)
	if c.MenuBarIndicator.Dir == dir {
		c.MenuBarIndicator = Indicator{Dir: c.Profiles[0], Key: autoKey}
	}
	return nil
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
