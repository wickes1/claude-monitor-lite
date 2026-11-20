<div align="center">

# Claude Monitor Lite

**Your Claude plan's usage limits, live in the macOS menu bar.**

[![Release](https://img.shields.io/github/v/release/wickes1/claude-monitor-lite)](https://github.com/wickes1/claude-monitor-lite/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/wickes1/claude-monitor-lite/total)](https://github.com/wickes1/claude-monitor-lite/releases)
[![License](https://img.shields.io/github/license/wickes1/claude-monitor-lite)](LICENSE)
![Platform](https://img.shields.io/badge/platform-macOS-lightgrey?logo=apple)

<img src="demo-menubar.png" width="560" alt="The menu: two accounts, each with its 5-hour and weekly limits, and the 5-hour session at 62% in the menu bar">

</div>

- **One number in the menu bar**: a traffic light, the percentage used, and the time until it resets.
- **Every limit your plan has** in the menu, for **every account** you use. The rows come from Claude's own list, so a new limit needs no update.
- **Nothing to set up.** It reuses Claude Code's login, read-only: no API key, no token stored.
- **Live while you work**: opt in, and the numbers follow Claude Code within seconds.

## Install

Needs [Claude Code](https://code.claude.com/docs) logged in with a Claude subscription.

```bash
brew install --cask wickes1/tap/claude-monitor-lite
claude-monitor-lite            # start it
claude-monitor-lite install    # and start it at every login
```

<details>
<summary>Without Homebrew, or through your coding agent</summary>

```bash
go install github.com/wickes1/claude-monitor-lite@latest
```

Builds are on the [releases page](https://github.com/wickes1/claude-monitor-lite/releases/latest). The Linux build is untested, and `install` is macOS-only.

To have Claude Code or another agent do it, paste:

```text
Install claude-monitor-lite on this Mac: run `brew install --cask wickes1/tap/claude-monitor-lite`,
then `claude-monitor-lite` and show me its output (stop if it reports no Claude Code login),
then `claude-monitor-lite install`. Ask me before `claude-monitor-lite statusline install`.
Docs: https://github.com/wickes1/claude-monitor-lite
```

</details>

## Use

| Menu bar | Meaning |
|---|---|
| 🟢 `12% (4h30m)` | Under 50% used; the time is what is left until the limit resets |
| 🟡 `62% (2h10m)` | 50% to 79% |
| 🔴 `91% (0h40m)` | 80% or more |
| ⚪ `Loading...` `Login` `Error` | No number to show: see [Troubleshooting](#troubleshooting) |

Click a row in the menu to pin it to the menu bar; the checkmark is on the row it shows. Until you pin one, it shows the limit Claude itself marks as the one to watch.

Run `claude-monitor-lite` again to print the same numbers in the terminal.

## Several accounts

Claude Code keeps a second account in its own directory, selected with `CLAUDE_CONFIG_DIR`. Add that directory once and it becomes a group in the menu, named after the account:

```bash
claude-monitor-lite add ~/.claude-work      # undo with: remove ~/.claude-work
```

Log in there first (`CLAUDE_CONFIG_DIR=~/.claude-work claude`, then `/login`). An account that fails to load says so in its own group and leaves the menu bar alone. If your only login is in such a directory, add it and then `claude-monitor-lite remove default`.

## Live updates

The usage endpoint allows other tools about one request every 2 to 3 minutes, so polling cannot go faster. Claude Code, though, hands the 5-hour and weekly usage to its [status line](https://code.claude.com/docs/en/statusline) after every response, and the monitor can read it there:

```bash
claude-monitor-lite statusline install      # undo with: statusline uninstall
```

Your own status line command keeps running unchanged, and `settings.json` is backed up first. For an added account, run it with that directory: `CLAUDE_CONFIG_DIR=~/.claude-work claude-monitor-lite statusline install`.

## Your login stays yours

- **Reads** Claude Code's login from where Claude Code keeps it (the Keychain on macOS), and holds it in memory only.
- **Sends** it to the usage endpoint Claude Code itself uses, as `claude-monitor-lite/<version>`, and nowhere else.
- **Never** stores a token, refreshes one, or writes to the credential store. Renewing it from outside could sign Claude Code out, so an account you have not used for some hours shows `Waiting for Claude Code` with its last reading until Claude Code runs there again.
- **macOS permissions: none.** No Accessibility, Screen Recording or Full Disk Access.

## Troubleshooting

| You see | Do |
|---|---|
| ⚪ `Login` | Log in to Claude Code with a Claude subscription; an API key or Console login has no plan limits |
| `Waiting for Claude Code` on a group | Nothing: it resumes after Claude Code runs in that account |
| ⚪ `Error`, or the monitor does not start | Read `~/.claude-monitor-lite.log`; a settings file that is not valid JSON is named there |
| The menu does not respond | `claude-monitor-lite stop`, then `claude-monitor-lite` |

## Reference

<details>
<summary>Commands</summary>

| Command | Does |
|---|---|
| `claude-monitor-lite` (or `start`) | Starts the monitor; prints usage when it is already running |
| `claude-monitor-lite stop` | Stops it |
| `claude-monitor-lite add <dir>` | Also watches the Claude Code account in `<dir>` |
| `claude-monitor-lite remove <dir>` | Stops watching it; `default` names `~/.claude` |
| `claude-monitor-lite install` / `uninstall` | Starts the monitor at every login, or undoes that and stops it |
| `claude-monitor-lite statusline install` / `uninstall` | Live updates from Claude Code's status line, or your status line restored |
| `claude-monitor-lite version` / `help` | Prints the version, or the commands |

</details>

<details>
<summary>Plans and the rows they show</summary>

The menu shows whatever the usage endpoint reports for the account; there is no built-in list.

| Plan | Rows to expect |
|---|---|
| Pro, Max, Team | 5-hour session and weekly (all models), plus a per-model weekly row where the plan has one |
| Ultra | Weekly rows; a 5-hour row only until Anthropic has verified the account |
| Enterprise | Like Team for seats with usage limits. A usage-based seat has none, and the menu says so |
| Usage credits turned on | A **Usage Credits** row: percent of the monthly limit and the amount spent |

Checked against Max and Team accounts; the rest follows Anthropic's plan descriptions. Not covered, having no plan limits to read: API key and Console logins, Amazon Bedrock, Google Vertex AI, the Free plan.

</details>

<details>
<summary>Files, upgrade and uninstall</summary>

| Path | Holds |
|---|---|
| `~/.claude-monitor-lite.json` | Settings: `profiles` (the account directories, in menu order; an empty string is the default) and the menu bar selection |
| `~/.claude-monitor-lite.log` | Log |
| `~/.claude-monitor-lite.*` | Usage cache and live update feed, one set per account |
| `~/Library/LaunchAgents/io.github.wickes1.claude-monitor-lite.plist` | Start at login |

```bash
# upgrade
brew upgrade --cask claude-monitor-lite && claude-monitor-lite install

# uninstall
claude-monitor-lite statusline uninstall   # once per account, if you installed live updates
claude-monitor-lite uninstall
brew uninstall --cask claude-monitor-lite
rm ~/.claude-monitor-lite*
```

</details>

What changed in each version: [releases](https://github.com/wickes1/claude-monitor-lite/releases).
