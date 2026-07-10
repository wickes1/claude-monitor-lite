// shim.go - Global switching surface for multi-profile Claude Code.
//
// A profile is nothing more than a CLAUDE_CONFIG_DIR value (see
// credentials.go's claudeConfigDir). Making `profile switch <name>` actually
// change which subscription every NEW `claude` invocation on the machine uses
// -- in any shell, any terminal tab, any editor task runner -- requires
// something ahead of the real `claude` binary on PATH that sets that
// variable before exec'ing it. That something is this shim: a small POSIX sh
// script installed at ~/.claude-monitor-lite/bin/claude.
//
// The shim itself never decides anything; it only reads a single small state
// file this program maintains (the "active profile state file",
// ~/.claude-monitor-lite-active) and mirrors its content into the
// environment:
//
//   - Missing file         -> no profiles configured; exec claude with the
//     environment completely untouched, identical to the shim not existing.
//   - Present, empty       -> the active profile is the default (~/.claude);
//     CLAUDE_CONFIG_DIR is explicitly unset before exec'ing, because a *set*
//     value (even one that happens to equal ~/.claude) changes the Keychain
//     service name Claude Code derives, breaking the existing login.
//   - Present, a directory -> that directory exists on disk: export
//     CLAUDE_CONFIG_DIR to it before exec'ing. If it does NOT exist (a
//     profile was removed, or the state file is stale), the shim falls
//     through with the environment untouched rather than pointing Claude
//     Code at a directory that vanished.
//
// The shim resolves the real `claude` binary itself by scanning PATH and
// skipping its own directory, so installing it can never cause it to exec
// itself. It performs no writes of any kind -- not to the state file, not to
// any profile's config dir, not to any credential store. This file's only
// writable surface is the shim script under this program's own bin dir,
// which keeps it inside the read-only invariant that governs the rest of
// this codebase (see credentials.go).
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Path components for the two files this stage owns, both rooted at the
// user's home directory.
const (
	shimBinDirName         = ".claude-monitor-lite"
	shimBinSubdir          = "bin"
	shimBinaryName         = "claude"
	activeProfileStateName = ".claude-monitor-lite-active"

	shimDirPermissions    = 0755
	shimScriptPermissions = 0755
)

// shimScript is the POSIX sh source installed verbatim at shimPath(). It is a
// static template -- no user data is ever interpolated into it, so there is
// no escaping concern. It implements ISC-77, 78, 79, 80, 82, 83.
//
// Performance note (ISC-79, <50ms overhead): the script does a single
// subshell fork to resolve its own directory, then one builtin `read` and one
// builtin `for` loop over PATH -- no other subshells, no external commands
// besides the final exec of the real claude binary.
const shimScript = `#!/bin/sh
# Installed by claude-monitor-lite ('profile shim install'). Do not edit by
# hand -- regenerate with: claude-monitor-lite profile shim install
#
# Stands in for the real 'claude' binary on PATH so that
# 'claude-monitor-lite profile switch <name>' changes which Claude Code
# config directory every NEW claude invocation uses, machine-wide, without
# editing any shell rc file. See shim.go for the full design rationale.
#
# This script performs no writes of its own. All state is the single active-
# profile file read below, which the monitor maintains. A missing, empty, or
# unreadable state file is always treated as "do nothing" -- never as an
# error -- so that installing the shim with zero profiles configured behaves
# exactly like the shim was never installed.

state_file="$HOME/.claude-monitor-lite-active"

# Resolve our own directory so the PATH scan below can skip it -- this is
# what prevents the shim from ever finding and exec'ing itself. Deliberately
# uses only shell parameter-expansion (no external dirname) so this works
# even with a minimal PATH.
case $0 in
    */*) self_dir_raw=${0%/*} ;;
    *)   self_dir_raw=. ;;
esac
self_dir=$(CDPATH= cd -- "$self_dir_raw" && pwd)

if [ -f "$state_file" ]; then
    active_dir=""
    IFS= read -r active_dir < "$state_file"
    if [ -z "$active_dir" ]; then
        # Empty state file: the active profile is the default (~/.claude).
        # CLAUDE_CONFIG_DIR must stay unset -- an explicit value, even one
        # that names ~/.claude itself, changes the Keychain service name
        # Claude Code derives and would break the existing login.
        unset CLAUDE_CONFIG_DIR
    elif [ -d "$active_dir" ]; then
        export CLAUDE_CONFIG_DIR="$active_dir"
    fi
    # Else: the recorded directory no longer exists on disk -- fall through
    # with the environment untouched rather than pointing Claude Code at a
    # config dir that vanished.
fi
# Else: no state file at all means no profiles are configured -- environment
# untouched, identical to the shim not being installed.

real_claude=""
old_ifs=$IFS
IFS=:
for dir in $PATH; do
    # Skip our own directory both textually and by inode (-ef): a PATH entry
    # that names the shim dir non-canonically (symlink, trailing slash) would
    # pass the string compare and make the shim exec itself in a loop.
    if [ -z "$real_claude" ] && [ -n "$dir" ] && [ "$dir" != "$self_dir" ] \
        && ! [ "$dir" -ef "$self_dir" ] \
        && [ -f "$dir/claude" ] && [ -x "$dir/claude" ]; then
        real_claude="$dir/claude"
    fi
done
IFS=$old_ifs

if [ -z "$real_claude" ]; then
    echo "claude-monitor-lite shim: could not find the real 'claude' binary on PATH (searched outside $self_dir)" >&2
    exit 127
fi

exec "$real_claude" "$@"
`

// shimDir returns ~/.claude-monitor-lite/bin, the directory the shim script
// lives in.
func shimDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, shimBinDirName, shimBinSubdir), nil
}

// shimPath returns ~/.claude-monitor-lite/bin/claude, the shim script itself.
func shimPath() (string, error) {
	dir, err := shimDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, shimBinaryName), nil
}

// ActiveProfileStatePath returns ~/.claude-monitor-lite-active, the small
// state file the shim reads on every invocation and SwitchProfile (owned
// elsewhere) writes on every switch. Deliberately not the full JSON config:
// the shim must stay fast (ISC-79) and must not need to understand the
// config schema to do its one job.
func ActiveProfileStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, activeProfileStateName), nil
}

// InstallShim writes the shim script to ~/.claude-monitor-lite/bin/claude
// (mode 0755, atomically) and returns the exact `export PATH=...` line the
// user must add to their shell rc themselves. It never edits any rc file
// (ISC-81) -- printing instructions and asking the user to act is the whole
// point: this program's only writable surfaces are its own config, its own
// state file, and its own shim script, and an rc file is none of those.
func InstallShim() (string, error) {
	dir, err := shimDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, shimDirPermissions); err != nil {
		return "", fmt.Errorf("create shim directory %s: %w", dir, err)
	}

	path, err := shimPath()
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, []byte(shimScript), shimScriptPermissions); err != nil {
		return "", fmt.Errorf("write shim script %s: %w", path, err)
	}

	return fmt.Sprintf(`export PATH="%s:$PATH"`, dir), nil
}

// UninstallShim removes the shim script and, if the shim's bin directory is
// then empty, removes that directory too (ISC-84). It is idempotent: calling
// it when nothing is installed is a silent no-op, not an error, matching the
// existing uninstall's tolerance for "nothing to remove" (see install.go).
func UninstallShim() error {
	path, err := shimPath()
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove shim script %s: %w", path, err)
	}

	dir, err := shimDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Directory already gone, or never existed -- nothing left to do.
		return nil
	}
	if len(entries) == 0 {
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("remove empty shim directory %s: %w", dir, err)
		}
	}
	return nil
}
