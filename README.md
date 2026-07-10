# Claude Monitor Lite

A lightweight menu bar monitor that displays **real-time rate limits** from your Claude account.

![Menu Bar Interface](demo-menubar.png)

## Features

- Displays every usage window the Claude API reports — 5-hour session, weekly (all models), per-model windows (Sonnet, Opus, Design, Cowork, …) and pay-as-you-go extra usage
- Traffic light indicator: 🟢 Green (0-49%), 🟡 Yellow (50-79%), 🔴 Red (80%+)
- Auto-refresh every 60 seconds
- Requires Claude account

**Platform:** Tested on macOS. Other platforms not tested.

## Installation

```bash
# Homebrew
brew tap wickes1/tap
brew install --cask claude-monitor-lite

# Go
go install github.com/wickes1/claude-monitor-lite@latest
```

## Usage

```bash
claude-monitor-lite
```

**First run:** the monitor reuses your existing Claude Code login — nothing to copy or paste. Install Claude Code and sign in once, then run `claude-monitor-lite`. It reads Claude Code's credential at runtime (Keychain on macOS, `~/.claude/.credentials.json` on other platforms), holds it in memory only, and never stores a token of its own.

**Manual fallback (no Claude Code):** without Claude Code, the monitor falls back to a session key:
1. A browser opens to claude.ai — log in
2. Open DevTools → Application → Cookies → https://claude.ai
3. Copy the `sessionKey` value and paste when prompted

The session key is saved to `~/.claude-monitor-lite.json`; the Claude Code path above saves nothing.

**Already running:** The command shows current status.

![Terminal Output](demo-terminal.png)

**Switching metrics:** Click any metric in the menu to show it in the menu bar. Only windows with data are listed; a window appears automatically if the API starts reporting it.

## Commands

```bash
claude-monitor-lite              # Start or show status
claude-monitor-lite stop         # Stop the monitor
claude-monitor-lite logout       # Clear session
claude-monitor-lite install      # Start automatically at login
claude-monitor-lite uninstall    # Stop starting at login
claude-monitor-lite version      # Show version
claude-monitor-lite profile ...  # Multi-profile: add / login / list / switch / remove / shim
```

## Profiles

A profile is a Claude Code config directory. Claude Code natively isolates everything user-scope under `CLAUDE_CONFIG_DIR` — login/subscription, settings, MCP servers, skills, and history — so pointing a shell at a different directory runs a fully separate environment:

```bash
CLAUDE_CONFIG_DIR=~/.claude-profiles/work claude
```

This is the primary usage: per-shell isolation. Different shells can run different profiles at the same time — one on a work subscription, another on a personal one. No shim, no switching, nothing global.

The monitor adds two things on top:

- A registry (`profile add/list/switch/remove`) so profiles have names and known locations. Your existing `~/.claude` is registered automatically as `default` and never modified.
- Side-by-side usage: the menu bar polls every registered profile and shows each one's limits, not just the active profile's.

```bash
claude-monitor-lite profile add work      # Create an isolated profile
claude-monitor-lite profile login work    # Sign in to it (launches claude)
claude-monitor-lite profile list          # Names, dirs, credential health
claude-monitor-lite profile remove work   # Unregister (dir is kept)
```

The monitor is read-only toward every credential store: it never writes a Keychain item or credentials file and never refreshes a token — each profile's login is owned by the Claude Code running in that profile. A profile whose token has expired freezes at its last reading ("as of …") with reset countdowns still computed locally.

### Global switching (optional)

If you want a single machine-wide active profile instead of per-shell env vars, install the PATH shim:

```bash
claude-monitor-lite profile shim install   # writes ~/.claude-monitor-lite/bin/claude, prints the PATH line
claude-monitor-lite profile switch work    # new claude sessions use it
claude-monitor-lite profile shim uninstall
```

Every new `claude` invocation then runs in the active profile; already-running sessions are unaffected. The shim takes precedence over a `CLAUDE_CONFIG_DIR` already set in the shell — it exists to make one profile authoritative machine-wide. Skip it if you want per-shell control.

On macOS each profile's credential lives in its own Keychain item (`Claude Code-credentials-<sha256(dir)[:8]>`, matching [Claude Code's derivation](https://gist.github.com/KMJ-007/0979814968722051620461ab2aa01bf2)); on other platforms in `<dir>/.credentials.json` ([docs](https://code.claude.com/docs/en/authentication)). Isolation model inspired by [claude-code-profiles](https://github.com/quinnjr/claude-code-profiles).

### Isolation boundary

Isolation is user-scope only.

| Isolated per profile | Shared / not isolated |
|---|---|
| Login and subscription | Claude Code binary and auto-update |
| User settings, MCP servers, skills | Project-scope files in a repo (`.claude/`, `.mcp.json`, `CLAUDE.md`) |
| History | Shell environment variables |

Each new profile starts bare and logs in separately.

## Start at login

Register a LaunchAgent so the monitor starts automatically when you log in:

```bash
claude-monitor-lite install
```

This writes `~/Library/LaunchAgents/io.github.wickes1.claude-monitor-lite.plist` pointing at the binary on your `PATH`, so it keeps working across `brew upgrade`. To disable:

```bash
claude-monitor-lite uninstall
```

A LaunchAgent owns the monitor's lifecycle: `uninstall` disables autostart and stops a monitor that autostart had started, and a reinstall restarts it. Start it again anytime with `claude-monitor-lite`.

## Troubleshooting

**Session expired:** Run `claude-monitor-lite logout` then restart.

**App not responding:** Run `killall claude-monitor-lite` then restart.
