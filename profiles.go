// profiles.go - Named, fully isolated Claude Code profiles and switching.
//
// A profile IS a config directory: CLAUDE_CONFIG_DIR (or ~/.claude when
// unset). Registering, listing, and switching profiles never reaches inside
// that directory except to read it (credentials.go's ReadClaudeCodeCredentialForDir
// and, read-only, <dir>/.claude.json for identity — see ISC-16, added by a
// later stage). This file owns only the registry (Config.Profiles /
// Config.ActiveProfile) and the switch engine, which is metadata-only:
//
//   - Nothing here ever creates, moves, or deletes a Keychain item.
//   - Nothing here ever writes inside a profile's config dir (AddProfile's
//     os.MkdirAll creates the directory itself, once, at registration time —
//     it never writes a file inside it).
//   - Switching updates two things only: our own config's ActiveProfile
//     field, and our own small state file (contract 5) that the shim reads.
//
// This keeps the monitor's read-only invariant intact even as profiles
// multiply: every credential store stays exactly as Claude Code itself left
// it.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// profileNameRe is the only shape a profile name may take: this keeps names
// safe to use as directory path components and to display without escaping.
var profileNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// defaultProfileName is the name auto-registered for the pre-existing,
// pre-feature Claude Code config dir (~/.claude, or CLAUDE_CONFIG_DIR if the
// user already had one set). It is never deleted by RemoveProfile logic
// implicitly — like any other profile, it can only be removed by explicit
// name, and never while active.
const defaultProfileName = "default"

// Profile registry and switch-engine errors. Each is wrapped with context by
// its caller, so callers can errors.Is against the sentinel while still
// getting a specific message.
var (
	ErrInvalidProfileName        = errors.New("invalid profile name: must match ^[a-zA-Z0-9_-]{1,32}$")
	ErrProfileExists             = errors.New("profile already exists")
	ErrProfileNotFound           = errors.New("profile not found")
	ErrCannotRemoveActiveProfile = errors.New("cannot remove the active profile")
)

// profilesBaseDirOverride, when non-empty, replaces the default
// ~/.claude-profiles base directory AddProfile uses when no --dir is given.
// Tests set this to a t.TempDir() so a default-dir AddProfile call never
// touches the real home directory.
var profilesBaseDirOverride string

func profilesBaseDir() string {
	if profilesBaseDirOverride != "" {
		return profilesBaseDirOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-profiles")
}

// stateFilePathOverride, when non-empty, replaces the default
// ~/.claude-monitor-lite-active state file path. Tests set this to a temp
// file so SwitchProfile never writes the real one.
var stateFilePathOverride string

// activeStateFilePath returns the path of the shim's active-profile state
// file (contract 5): its content is the active profile's config dir path
// followed by a newline when the active profile is NOT default, and empty
// when it IS default (so the shim knows to leave CLAUDE_CONFIG_DIR unset).
// A missing file means no profiles are configured at all — the shim passes
// through untouched.
func activeStateFilePath() string {
	if stateFilePathOverride != "" {
		return stateFilePathOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-monitor-lite-active")
}

// validateProfileName rejects any name before it can reach a config mutation,
// so a malformed name can never end up as a directory path component or
// registry key.
func validateProfileName(name string) error {
	if !profileNameRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidProfileName, name)
	}
	return nil
}

// findProfile returns the profile named name from profiles, if present.
func findProfile(profiles []ProfileMeta, name string) (ProfileMeta, bool) {
	for _, p := range profiles {
		if p.Name == name {
			return p, true
		}
	}
	return ProfileMeta{}, false
}

// profileNames extracts just the names, in registry order, for error
// messages that list what's available.
func profileNames(profiles []ProfileMeta) []string {
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	return names
}

// ensureDefaultProfileLocked registers the "default" profile pointing at
// claudeConfigDir() if it is not already present, and defaults ActiveProfile
// to "default" if unset. It must only be called from inside a config
// modifier (i.e. while configWriteMutex is already held via
// modifyAndSaveConfig), hence "Locked". It never touches anything inside the
// default config dir — only Config's own Profiles/ActiveProfile fields.
func ensureDefaultProfileLocked(c *Config) {
	if _, ok := findProfile(c.Profiles, defaultProfileName); !ok {
		c.Profiles = append(c.Profiles, ProfileMeta{
			Name:    defaultProfileName,
			Dir:     claudeConfigDir(),
			AddedAt: time.Now(),
		})
	}
	if c.ActiveProfile == "" {
		c.ActiveProfile = defaultProfileName
	}
}

// EnsureDefaultProfile auto-registers the "default" profile (pointing at the
// user's existing ~/.claude) on first multi-profile use (ISC-13). It is
// idempotent and safe to call before every profile-registry mutation.
func EnsureDefaultProfile() error {
	return modifyAndSaveConfig(ensureDefaultProfileLocked)
}

// ListProfiles returns every registered profile, in registration order. It is
// a pure read: with zero profiles configured it returns an empty slice
// without registering anything, so a bare `profile list` on a fresh install
// costs nothing and writes nothing (backward-compat contract 7).
func ListProfiles() []ProfileMeta {
	cfg := LoadConfig()
	return cfg.Profiles
}

// activeProfileName returns the configured active profile's name, defaulting
// to "default" when Profiles is non-empty but ActiveProfile is unset, or to
// "" when no profiles are configured at all.
func activeProfileName(cfg Config) string {
	if len(cfg.Profiles) == 0 {
		return ""
	}
	if cfg.ActiveProfile == "" {
		return defaultProfileName
	}
	return cfg.ActiveProfile
}

// activeProfileDir resolves the config directory CurrentOAuthToken and
// InvalidateOAuthToken should use for the "no explicit dir given" call
// shape: the active profile's dir, or claudeConfigDir() when no profiles are
// configured (backward-compat contract 7) or the recorded active profile is
// somehow missing from the registry (degrade to default rather than error,
// since this feeds a token lookup that must never crash the poller).
func activeProfileDir() string {
	cfg := LoadConfig()
	name := activeProfileName(cfg)
	if name == "" {
		return claudeConfigDir()
	}
	if meta, ok := findProfile(cfg.Profiles, name); ok {
		return meta.Dir
	}
	return claudeConfigDir()
}

// ActiveProfileMeta returns the currently active profile's metadata. With no
// profiles configured, it synthesizes the default profile's metadata (name
// "default", dir claudeConfigDir()) without persisting anything, so callers
// can treat "no profiles yet" and "default profile" uniformly.
func ActiveProfileMeta() (ProfileMeta, error) {
	cfg := LoadConfig()
	name := activeProfileName(cfg)
	if name == "" {
		return ProfileMeta{Name: defaultProfileName, Dir: claudeConfigDir()}, nil
	}
	meta, ok := findProfile(cfg.Profiles, name)
	if !ok {
		return ProfileMeta{}, fmt.Errorf("%w: active profile %q is not registered", ErrProfileNotFound, name)
	}
	return meta, nil
}

// AddProfile registers a new profile named name, creating its config
// directory (mode 0700) if it does not already exist. If dir is empty, the
// profile's directory defaults to <profilesBaseDir>/<name> (normally
// ~/.claude-profiles/<name>). Registering a duplicate name is refused
// (ISC-14). This also lazily runs EnsureDefaultProfile first, since adding a
// second profile is exactly the "first multi-profile use" moment (ISC-13).
//
// AddProfile creates the profile's own directory but never writes a file
// inside it — that directory is left for the user to `claude` login into
// (ISC-15, a later stage), keeping this program's read-only invariant
// toward every profile's actual Claude Code state.
func AddProfile(name, dir string) (ProfileMeta, error) {
	if err := validateProfileName(name); err != nil {
		return ProfileMeta{}, err
	}
	if err := EnsureDefaultProfile(); err != nil {
		return ProfileMeta{}, fmt.Errorf("ensure default profile: %w", err)
	}

	if dir == "" {
		dir = filepath.Join(profilesBaseDir(), name)
	} else {
		// A user-supplied dir is stored absolute: the registry is read by
		// processes with different working directories (the LaunchAgent
		// daemon runs with cwd=/), and the dir feeds the Keychain service
		// derivation — a stored relative path would silently select a wrong
		// credential store and pin the profile at "needs login".
		abs, err := filepath.Abs(dir)
		if err != nil {
			return ProfileMeta{}, fmt.Errorf("resolve profile directory %s: %w", dir, err)
		}
		dir = abs
	}

	var added ProfileMeta
	var mutateErr error
	err := modifyAndSaveConfig(func(c *Config) {
		if _, exists := findProfile(c.Profiles, name); exists {
			mutateErr = fmt.Errorf("%w: %q", ErrProfileExists, name)
			return
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			mutateErr = fmt.Errorf("create profile directory %s: %w", dir, err)
			return
		}
		added = ProfileMeta{Name: name, Dir: dir, AddedAt: time.Now()}
		c.Profiles = append(c.Profiles, added)
	})
	if err != nil {
		return ProfileMeta{}, err
	}
	if mutateErr != nil {
		return ProfileMeta{}, mutateErr
	}
	return added, nil
}

// RemoveProfile unregisters a profile by name. It only removes the registry
// entry — it never deletes the profile's config directory or any Keychain
// item (ISC-6), so a removed-then-re-added profile of the same name would
// see its prior login again. Removing the active profile is refused
// (ISC-7): switch away first.
func RemoveProfile(name string) error {
	cfg := LoadConfig()
	if _, ok := findProfile(cfg.Profiles, name); !ok {
		return fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	if activeProfileName(cfg) == name {
		return fmt.Errorf("%w: %q (switch to another profile first)", ErrCannotRemoveActiveProfile, name)
	}

	var mutateErr error
	err := modifyAndSaveConfig(func(c *Config) {
		if activeProfileName(*c) == name {
			mutateErr = fmt.Errorf("%w: %q (switch to another profile first)", ErrCannotRemoveActiveProfile, name)
			return
		}
		filtered := c.Profiles[:0:0]
		found := false
		for _, p := range c.Profiles {
			if p.Name == name {
				found = true
				continue
			}
			filtered = append(filtered, p)
		}
		if !found {
			mutateErr = fmt.Errorf("%w: %q", ErrProfileNotFound, name)
			return
		}
		c.Profiles = filtered
	})
	if err != nil {
		return err
	}
	return mutateErr
}

// writeActiveStateFile writes the shim's state file (contract 5): empty
// content when the active profile is the default profile (so the shim
// leaves CLAUDE_CONFIG_DIR unset), or dir + "\n" otherwise. Written
// atomically via writeFileAtomic, matching every other write this program
// performs to its own surfaces.
func writeActiveStateFile(dir string, isDefault bool) error {
	content := []byte{}
	if !isDefault {
		content = []byte(dir + "\n")
	}
	path := activeStateFilePath()
	// writeFileAtomic renames a temp file into place; the temp file must be
	// created in the same directory as path for the rename to stay atomic,
	// so make sure that directory exists (it's the user's home directory in
	// production, always present; a fresh t.TempDir() in tests).
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("prepare state file directory: %w", err)
	}
	return writeFileAtomic(path, content, 0600)
}

// SwitchResult summarizes the outcome of a SwitchProfile call for CLI/menubar
// display: which profile was active before, which is active now, whether
// this was a no-op (already active), and the target's credential health.
type SwitchResult struct {
	From          string
	To            string
	AlreadyActive bool
	Health        CredentialStatus
}

// SwitchProfile makes name the active profile: it atomically updates
// Config.ActiveProfile and writes the shim's state file (contract 5), then
// reports the target's credential health without blocking on it — a
// needs-login or expired target still switches successfully, since the
// switch is about which store is picked, not whether it currently holds a
// live token (ISC-22).
//
// Switching to an unknown profile errors, listing what IS available
// (ISC-24). Switching to the already-active profile is a no-op, reported via
// AlreadyActive rather than as an error (ISC-23). The switch path performs
// zero writes to any Keychain item or profile config dir — only this
// program's own config and state file are touched (ISC-25).
// errSwitchAlreadyActive signals, inside SwitchProfile's locked update, that
// the target is already active — the cycle is aborted (nothing written) and
// the caller translates it into an AlreadyActive result, not an error.
var errSwitchAlreadyActive = errors.New("profile already active")

func SwitchProfile(name string) (SwitchResult, error) {
	if err := EnsureDefaultProfile(); err != nil {
		return SwitchResult{}, fmt.Errorf("ensure default profile: %w", err)
	}

	// Validation, the ActiveProfile update, and the shim state-file write all
	// happen inside one locked read-modify-write cycle (updateConfigLocked
	// holds a cross-process file lock), so a concurrent switch from another
	// process — the daemon vs. a CLI invocation — can neither clobber this
	// update nor leave the state file pointing at a different profile than
	// the config records (ISC-49, ISC-64).
	var (
		result    SwitchResult
		targetDir string
	)
	err := updateConfigLocked(func(c *Config) error {
		from := activeProfileName(*c)
		target, ok := findProfile(c.Profiles, name)
		if !ok {
			available := strings.Join(profileNames(c.Profiles), ", ")
			if available == "" {
				available = "(none registered)"
			}
			return fmt.Errorf("%w: %q (available: %s)", ErrProfileNotFound, name, available)
		}
		targetDir = target.Dir
		result = SwitchResult{From: from, To: name}
		if from == name {
			result.AlreadyActive = true
			return errSwitchAlreadyActive
		}
		c.ActiveProfile = name
		return nil
	}, func(Config) error {
		if err := writeActiveStateFile(targetDir, name == defaultProfileName); err != nil {
			return fmt.Errorf("write active state file: %w", err)
		}
		return nil
	})

	if errors.Is(err, errSwitchAlreadyActive) {
		result.Health = CredentialHealth(targetDir)
		return result, nil
	}
	if err != nil {
		return SwitchResult{}, err
	}

	// Audit line: from->to only, never any credential material.
	log.Printf("profile switch: %s -> %s", result.From, name)

	result.Health = CredentialHealth(targetDir)
	return result, nil
}
