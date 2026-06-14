# Claude Monitor Lite

A lightweight menu bar monitor that displays **real-time rate limits** from your Claude account.

![Menu Bar Interface](demo-menubar.png)

## Features

- Displays every usage window the Claude API reports — 5-hour session, weekly (all models), per-model windows (Sonnet, Opus, Design, Cowork, …) and pay-as-you-go extra usage
- Traffic light indicator: 🟢 Green (0-49%), 🟡 Yellow (50-79%), 🔴 Red (80%+)
- Auto-refresh every 30 seconds
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

**First run setup:**
1. Browser opens to claude.ai
2. Log in to your account
3. Open DevTools (F12 or Cmd+Option+I)
4. Navigate to: Application → Cookies → https://claude.ai
5. Copy the `sessionKey` value
6. Paste when prompted
7. Monitor starts and appears in your menu bar

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
