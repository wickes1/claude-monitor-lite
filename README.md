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
```

## Which login it reads

The monitor reads whichever Claude Code environment it is started in: `~/.claude` by default, or `CLAUDE_CONFIG_DIR` when that is set. Claude Code isolates everything user-scope under that directory — login/subscription, settings, MCP servers, skills, and history — so launching the monitor from a shell with `CLAUDE_CONFIG_DIR` set monitors that environment instead:

```bash
CLAUDE_CONFIG_DIR=~/.claude-work claude-monitor-lite
```

It is read-only toward the credential store: it never writes a Keychain item or credentials file and never refreshes a token — the login is owned by Claude Code. On macOS the credential is read from the `Claude Code-credentials` Keychain item; on other platforms from `<dir>/.credentials.json` ([docs](https://code.claude.com/docs/en/authentication)).

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
