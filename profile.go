// profile.go - The Claude Code environments one monitor watches.
//
// Claude Code keeps each environment's login, settings and account details
// apart: the default one under ~/.claude, any other under the directory
// CLAUDE_CONFIG_DIR names. A profile is one such environment. The monitor
// watches every profile its config lists and shows each as a group in the
// menu, named after the account logged in there.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultProfileName is how the default environment is written on the command
// line and in the log, where an empty string would not read.
const defaultProfileName = "default"

// profile is one Claude Code environment. dir is the cleaned absolute
// CLAUDE_CONFIG_DIR, or empty for the default environment — which is not the
// same as naming ~/.claude: Claude Code stores the login under a different
// Keychain item as soon as the variable is set (see keychainService).
type profile struct {
	dir string
}

// newProfile returns the profile for a config dir however it was spelled
// (relative, trailing slash, ".." segments). An empty dir is the default
// environment.
func newProfile(dir string) profile {
	if dir == "" {
		return profile{}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return profile{dir: filepath.Clean(dir)}
}

// envProfile is the environment this process was started in. The status line
// commands use it: Claude Code runs them with its own CLAUDE_CONFIG_DIR.
func envProfile() profile {
	return newProfile(os.Getenv("CLAUDE_CONFIG_DIR"))
}

// configDir is where Claude Code keeps this environment's files.
func (p profile) configDir() string {
	if p.dir != "" {
		return p.dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

var unsafeFolderChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// folder is the readable name of the profile's directory, or
// defaultProfileName for the default environment. It names the profile where
// the account has no name to give, and in the log.
func (p profile) folder() string {
	if p.dir == "" {
		return defaultProfileName
	}
	name := unsafeFolderChars.ReplaceAllString(filepath.Base(p.dir), "-")
	if name = strings.Trim(name, "-"); name == "" {
		return "custom"
	}
	return name
}

// dirHash is Claude Code's 8-hex-char hash of the config dir.
func (p profile) dirHash() string {
	sum := sha256.Sum256([]byte(p.dir))
	return hex.EncodeToString(sum[:])[:8]
}

// id tells profiles apart in file names, even when two dirs share a folder
// name. Empty for the default environment.
func (p profile) id() string {
	if p.dir == "" {
		return ""
	}
	return p.folder() + "-" + p.dirHash()
}

// file returns one of this profile's own dotfiles in the home directory:
// ~/.claude-monitor-lite<ext> for the default environment,
// ~/.claude-monitor-lite.<id><ext> for any other. These are the files kept
// per profile — the usage cache and the status line feed; the monitor's own
// files (config, PID, log) are monitorFile's.
func (p profile) file(ext string) string {
	if id := p.id(); id != "" {
		return monitorFile("." + id + ext)
	}
	return monitorFile(ext)
}

// keychainService mirrors how Claude Code names its Keychain item: the plain
// name for the default environment, and
// "<name>-<sha256(cleaned absolute dir)[:8] hex>" whenever CLAUDE_CONFIG_DIR
// is set — even if it points at ~/.claude. Reading the plain item for such an
// environment would silently report the default account's usage instead.
func (p profile) keychainService() string {
	if p.dir == "" {
		return keychainServiceBase
	}
	return keychainServiceBase + "-" + p.dirHash()
}

// accountFile is where Claude Code records who is logged in: ~/.claude.json
// for the default environment, <dir>/.claude.json for any other.
func (p profile) accountFile() string {
	if p.dir != "" {
		return filepath.Join(p.dir, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// account is the part of Claude Code's oauthAccount block that names a
// profile. It is Claude Code's own bookkeeping, not a documented format, so
// every field may be missing.
type account struct {
	DisplayName      string `json:"displayName"`
	OrganizationName string `json:"organizationName"`
	OrganizationType string `json:"organizationType"` // "claude_max", "claude_team", …
	PlanDisplayName  string `json:"planDisplayName"`
}

// readAccount reads who is logged in to this profile. Any failure is an
// empty account: the profile is then named after its folder.
func (p profile) readAccount() account {
	var f struct {
		OAuthAccount account `json:"oauthAccount"`
	}
	if data, err := os.ReadFile(p.accountFile()); err == nil {
		_ = json.Unmarshal(data, &f)
	}
	return f.OAuthAccount
}

// plan is the account's plan as Claude names it, or failing that as its
// organization type implies: "claude_max_5x" is "Max 5x".
func (a account) plan() string {
	if a.PlanDisplayName != "" {
		return a.PlanDisplayName
	}
	plan, ok := strings.CutPrefix(a.OrganizationType, "claude_")
	if !ok || plan == "" {
		return ""
	}
	plan = strings.ReplaceAll(plan, "_", " ")
	return strings.ToUpper(plan[:1]) + plan[1:]
}

// name is how the account names itself: the person, the organization, and
// the plan in brackets — "Ada · Acme (Team)", "Ada (Max)". Only a Team or
// Enterprise seat has an organization to show: Claude names a personal
// account's "<email>'s Organization", which would put the address in the
// menu and in every screenshot of it. Empty when the account says nothing.
func (a account) name() string {
	who := a.DisplayName
	if org := a.OrganizationName; org != "" && (a.OrganizationType == "claude_team" || a.OrganizationType == "claude_enterprise") {
		if who != "" {
			who += " · "
		}
		who += org
	}
	plan := a.plan()
	switch {
	case who == "":
		return plan
	case plan == "":
		return who
	}
	return who + " (" + plan + ")"
}

// profileLabels names each profile's group in the menu. A profile whose
// account gives no name is called by its folder, and profiles that would
// share a name get their folder appended, so no two groups read the same.
func profileLabels(profiles []profile, accounts []account) []string {
	labels := make([]string, len(profiles))
	count := make(map[string]int)
	for i, p := range profiles {
		if labels[i] = accounts[i].name(); labels[i] == "" {
			labels[i] = p.folder()
		}
		count[labels[i]]++
	}
	for i, p := range profiles {
		if count[labels[i]] > 1 && labels[i] != p.folder() {
			labels[i] = fmt.Sprintf("%s [%s]", labels[i], p.folder())
		}
	}
	return labels
}
