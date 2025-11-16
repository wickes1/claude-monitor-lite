# Claude Monitor Lite

A lightweight macOS menu bar monitor for Claude usage, showing **real-time rate limits** directly from your Claude account.

## Features

- Real-time Claude usage monitoring in your macOS menu bar
- Shows actual usage limits from claude.ai/settings/usage:
  - Current 5-hour session usage
  - Weekly limit (all models)
  - Weekly limit (Opus only)
- One-time browser login - saves session securely
- Auto-refresh every 30 seconds
- Runs quietly in the background

## Prerequisites

- **macOS only** (requires macOS system tray APIs)
- Go 1.25 or later
- Claude account (Free, Pro, or Max)

## Installation

```bash
git clone https://github.com/wickes1/claude-monitor-lite.git
cd claude-monitor-lite
make build
```

## Quick Start

Just run it! The app handles everything automatically:

```bash
claude-monitor-lite
```

**First time:** Guides you through browser login, extracts session key, and starts monitoring.

**Already running:** Shows current status.

The app appears in your menu bar with a traffic light indicator:
- 🟢 Green: 0-49% usage
- 🟡 Yellow: 50-79% usage
- 🔴 Red: 80%+ usage

## Login Process (First Time)

1. Browser opens to claude.ai automatically
2. Login if needed
3. Open DevTools (F12 or Cmd+Option+I)
4. Go to: Application tab → Cookies → https://claude.ai
5. Find and copy the 'sessionKey' cookie value
6. Paste when prompted

Session key is saved with 0600 permissions (owner read/write only).

## Commands

```bash
claude-monitor-lite          # Auto-start (login if needed, show status if running)
claude-monitor-lite stop     # Stop the monitor
claude-monitor-lite logout   # Clear session and stop monitor
claude-monitor-lite help     # Show help
```

## Switching Display Metrics

Click the menu bar icon and select any usage metric to display:
- **5-Hour Session** - Current session percentage
- **Weekly (All)** - All models weekly percentage
- **Weekly (Opus)** - Opus-only weekly percentage

Selection persists across restarts (✓ indicates active).

## Development

```bash
make build    # Build the binary
make start    # Build and start
make stop     # Stop the monitor
make restart  # Restart the monitor
make logout   # Logout and remove all data
make help     # Show available commands
```

## Troubleshooting

### Session expired or not authenticated
```bash
claude-monitor-lite logout
claude-monitor-lite
```

### App hung or not responding
```bash
killall claude-monitor-lite
claude-monitor-lite
```

## Configuration Files

- `~/.claude-monitor-lite.json` - Session & config (0600 permissions)
- `~/.claude-monitor-lite.pid` - Process ID file

## Security

- Session key stored with restricted permissions (0600)
- No third-party data transmission
- Direct Claude API communication only
- Clean removal via `logout` command

## License

MIT
