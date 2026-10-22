// statusline.go - Live usage from Claude Code's status line.
//
// Polling oauth/usage is capped at roughly one request per 2-3 minutes for
// non-Claude-Code clients (see refreshInterval). Claude Code itself receives
// the 5-hour and weekly utilization on every API response and hands it to the
// status line command as rate_limits. `statusline install` points Claude
// Code's status line at `claude-monitor-lite statusline`, which saves those
// two windows to a file the daemon watches and then runs the user's original
// status line command unchanged — so the menu bar follows Claude Code use
// within seconds at no extra API cost. Polling stays as the fallback: usage
// from claude.ai or the desktop app, extra usage and per-model windows never
// reach the status line.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// statusLineCheckInterval is how often the daemon looks for a new status line
// update — matching the 2s re-render Claude Code users commonly configure, so
// checking faster would find nothing new. It is a local stat, not a request.
const statusLineCheckInterval = 2 * time.Second

// statusLineInput is the part of Claude Code's status line JSON we read.
// Each window is present only while the API reports it and its reset has not
// passed; rate_limits itself is absent for API-key users and before the
// session's first API response.
type statusLineInput struct {
	RateLimits *struct {
		FiveHour *statusLineWindow `json:"five_hour"`
		SevenDay *statusLineWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type statusLineWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"` // Unix epoch seconds
}

func (w *statusLineWindow) usageLimit() *UsageLimit {
	if w == nil {
		return nil
	}
	limit := &UsageLimit{Utilization: w.UsedPercentage}
	if w.ResetsAt > 0 {
		limit.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
	}
	return limit
}

// statusLineLimits extracts the usage windows from one status line render, or
// nil when it carries none.
func statusLineLimits(input []byte) (*UsageLimits, error) {
	var in statusLineInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	if in.RateLimits == nil {
		return nil, nil
	}
	limits := &UsageLimits{
		FiveHour: in.RateLimits.FiveHour.usageLimit(),
		SevenDay: in.RateLimits.SevenDay.usageLimit(),
	}
	if limits.FiveHour == nil && limits.SevenDay == nil {
		return nil, nil
	}
	return limits, nil
}

// sameWindowTolerance absorbs the sub-second difference between a window's
// reset time as polled (22:49:59.56) and as the status line rounds it
// (22:50:00); distinct windows are hours apart.
const sameWindowTolerance = time.Minute

// newerWindow picks the more current reading of a usage window. Every open
// Claude Code session re-renders its status line with the rate_limits of its
// own last API response, so an idle session keeps reporting an old, lower
// figure. Within one window utilization only grows, so the higher reading is
// the newer one; a later reset time means a newer window altogether.
func newerWindow(a, b *UsageLimit) *UsageLimit {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if d := a.ResetsAt.Sub(b.ResetsAt); d > sameWindowTolerance || d < -sameWindowTolerance {
		if a.ResetsAt.After(b.ResetsAt) {
			return a
		}
		return b
	}
	if b.Utilization > a.Utilization {
		return b
	}
	return a
}

// mergeStatusLine overlays the windows a status line update carries onto the
// last full poll, keeping everything only polling can see (extra usage,
// per-model limits). Each window keeps whichever reading is newer, so a
// status line update can never roll back a fresher poll.
func mergeStatusLine(base, live *UsageLimits) *UsageLimits {
	merged := UsageLimits{}
	if base != nil {
		merged = *base
	}
	merged.FiveHour = newerWindow(merged.FiveHour, live.FiveHour)
	merged.SevenDay = newerWindow(merged.SevenDay, live.SevenDay)
	return &merged
}

// statusLineUsageFile holds the newest reading of each window across all
// Claude Code sessions' status line renders. Its mtime is when a reading last
// advanced, which is what lets the daemon tell it apart from an older poll.
func statusLineUsageFile() string {
	return instanceFile(".statusline-usage.json")
}

// statusLineWrapFile holds, as plain text, the status line command
// `statusline install` replaced — empty when there was none, in which case the
// wrapper prints nothing. Its existence marks the feed as installed. It lives
// apart from the config because the daemon rewrites the config from memory on
// every indicator click, which would drop it.
func statusLineWrapFile() string {
	return instanceFile(".statusline-wrap")
}

func readStatusLineWrap() (string, error) {
	data, err := os.ReadFile(statusLineWrapFile())
	return string(data), err
}

// statusLineGuardEnv marks the wrapped command's environment. If the wrapped
// command is somehow our own feed (a renamed binary slipping past
// isStatusLineFeed at install), the nested run stops here instead of
// re-executing itself until the user runs out of processes.
const statusLineGuardEnv = "CLAUDE_MONITOR_LITE_STATUSLINE"

// runStatusLine is what Claude Code runs on every status line render. Saving
// usage must never cost the user their status line, so its failures go to the
// log and the wrapped command runs regardless.
func runStatusLine() {
	if os.Getenv(statusLineGuardEnv) != "" {
		logStatusLineError(errors.New("wrapped status line command runs claude-monitor-lite itself; run `statusline uninstall`, fix statusLine.command, then install again"))
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err == nil {
		err = saveStatusLineUsage(input)
	}
	if err != nil {
		logStatusLineError(err)
	}

	wrapped, err := readStatusLineWrap()
	if err != nil {
		if !os.IsNotExist(err) {
			logStatusLineError(err)
		}
		return
	}
	if wrapped == "" {
		return
	}
	cmd := exec.Command("sh", "-c", wrapped)
	cmd.Env = append(os.Environ(), statusLineGuardEnv+"=1")
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		logStatusLineError(err)
		os.Exit(1)
	}
}

// saveStatusLineUsage folds the render's windows into the usage file, keeping
// the newer reading of each. Renders that advance nothing — the 2s re-render,
// or an idle session repeating an old figure — leave the file untouched, so
// its mtime stays the time a reading last moved.
//
// The read-merge-write is unlocked: two sessions saving in the same instant
// can drop the higher reading, but the active session restores it on its next
// 2s render, which is cheaper than a file lock on every render.
func saveStatusLineUsage(input []byte) error {
	limits, err := statusLineLimits(input)
	if err != nil || limits == nil {
		return err
	}
	path := statusLineUsageFile()
	old, err := os.ReadFile(path)
	if err == nil {
		var prev UsageLimits
		if json.Unmarshal(old, &prev) == nil {
			limits = mergeStatusLine(&prev, limits)
		}
	}
	data, err := json.Marshal(limits)
	if err != nil {
		return err
	}
	if bytes.Equal(old, data) {
		return nil
	}
	return writeFileAtomic(path, data, configFilePermissions)
}

// logStatusLineError appends to this instance's log. Stdout is the user's
// status line and stderr is not shown, so the log is the only place an error
// can surface.
func logStatusLineError(err error) {
	f, ferr := openLogFile()
	if ferr != nil {
		return
	}
	defer f.Close()
	log.New(f, "", log.LstdFlags).Printf("statusline: %v", err)
}

// readStatusLineUpdate returns the status line usage if it changed after
// since, or nil.
func readStatusLineUpdate(since time.Time) (*UsageLimits, time.Time, error) {
	path := statusLineUsageFile()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, since, nil
		}
		return nil, since, err
	}
	if !info.ModTime().After(since) {
		return nil, since, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, since, err
	}
	var limits UsageLimits
	if err := json.Unmarshal(data, &limits); err != nil {
		// Advance past the bad write so it is reported once, not every tick.
		return nil, info.ModTime(), err
	}
	return &limits, info.ModTime(), nil
}

// --- install / uninstall ---------------------------------------------------

// isStatusLineFeed reports whether a status line command is ours, under any
// binary path or CLAUDE_CONFIG_DIR prefix.
func isStatusLineFeed(command string) bool {
	command = strings.TrimSpace(command)
	return strings.Contains(command, "claude-monitor-lite") && strings.HasSuffix(command, " statusline")
}

// statusLineFeedCommand is the command Claude Code is pointed at. A
// CLAUDE_CONFIG_DIR instance pins the variable in the command, so the
// wrapper saves to that instance's files even if Claude Code were started
// without it in the environment.
func statusLineFeedCommand(binaryPath, configDir string) string {
	cmd := shellQuote(binaryPath) + " statusline"
	if configDir != "" {
		cmd = "CLAUDE_CONFIG_DIR=" + shellQuote(configDir) + " " + cmd
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jsonField is one member of a JSON object, kept in document order so
// rewriting a user's settings.json does not reshuffle it.
type jsonField struct {
	key   string
	value json.RawMessage
}

func parseObject(data []byte) ([]jsonField, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var fields []jsonField
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, jsonField{key: tok.(string), value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}

// renderObject writes fields back as compact JSON; callers indent the whole
// document once at the end.
func renderObject(fields []jsonField) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := marshalNoEscape(f.key)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		if err := json.Compact(&b, f.value); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalNoEscape is json.Marshal without HTML escaping, so a command such as
// `2>/dev/null` stays readable in settings.json instead of becoming >.
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func getField(fields []jsonField, key string) (json.RawMessage, bool) {
	for _, f := range fields {
		if f.key == key {
			return f.value, true
		}
	}
	return nil, false
}

func setField(fields []jsonField, key string, value json.RawMessage) []jsonField {
	for i := range fields {
		if fields[i].key == key {
			fields[i].value = value
			return fields
		}
	}
	return append(fields, jsonField{key: key, value: value})
}

func deleteField(fields []jsonField, key string) []jsonField {
	out := fields[:0]
	for _, f := range fields {
		if f.key != key {
			out = append(out, f)
		}
	}
	return out
}

func renderSettings(fields []jsonField) ([]byte, error) {
	compact, err := renderObject(fields)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// statusLineCommand returns the command of a statusLine object ("" if none).
func statusLineCommand(statusLine []jsonField) (string, error) {
	raw, ok := getField(statusLine, "command")
	if !ok {
		return "", nil
	}
	var command string
	err := json.Unmarshal(raw, &command)
	return command, err
}

// installStatusLineSettings points settings.json's status line at feed and
// returns the command it replaced. Re-running it over an installed feed only
// refreshes the feed command and reports already=true, so the original
// recorded at first install is never overwritten with our own command.
// Every other statusLine key (refreshInterval, padding) is kept.
func installStatusLineSettings(settings []byte, feed string) (out []byte, original string, already bool, err error) {
	fields, err := parseObject(settings)
	if err != nil {
		return nil, "", false, fmt.Errorf("settings.json: %w", err)
	}
	var statusLine []jsonField
	if raw, ok := getField(fields, "statusLine"); ok {
		if statusLine, err = parseObject(raw); err != nil {
			return nil, "", false, fmt.Errorf("settings.json statusLine: %w", err)
		}
	}
	if original, err = statusLineCommand(statusLine); err != nil {
		return nil, "", false, fmt.Errorf("settings.json statusLine.command: %w", err)
	}
	already = original == feed || isStatusLineFeed(original)

	feedJSON, err := marshalNoEscape(feed)
	if err != nil {
		return nil, "", false, err
	}
	statusLine = setField(statusLine, "type", json.RawMessage(`"command"`))
	statusLine = setField(statusLine, "command", feedJSON)
	rendered, err := renderObject(statusLine)
	if err != nil {
		return nil, "", false, err
	}
	fields = setField(fields, "statusLine", rendered)
	out, err = renderSettings(fields)
	return out, original, already, err
}

// uninstallStatusLineSettings restores original as the status line command,
// or removes the status line when there was none. It leaves settings alone
// (ours=false) when the status line is no longer our feed — the user has
// since pointed it elsewhere, and restoring would clobber that.
func uninstallStatusLineSettings(settings []byte, original, feed string) (out []byte, ours bool, err error) {
	fields, err := parseObject(settings)
	if err != nil {
		return nil, false, fmt.Errorf("settings.json: %w", err)
	}
	raw, ok := getField(fields, "statusLine")
	if !ok {
		return settings, false, nil
	}
	statusLine, err := parseObject(raw)
	if err != nil {
		return nil, false, fmt.Errorf("settings.json statusLine: %w", err)
	}
	command, err := statusLineCommand(statusLine)
	if err != nil || (command != feed && !isStatusLineFeed(command)) {
		return settings, false, nil
	}

	if original == "" {
		fields = deleteField(fields, "statusLine")
	} else {
		originalJSON, err := marshalNoEscape(original)
		if err != nil {
			return nil, false, err
		}
		statusLine = setField(statusLine, "command", originalJSON)
		rendered, err := renderObject(statusLine)
		if err != nil {
			return nil, false, err
		}
		fields = setField(fields, "statusLine", rendered)
	}
	out, err = renderSettings(fields)
	return out, true, err
}

// claudeSettingsPath is Claude Code's user settings file for this instance,
// with symlinks resolved so a dotfiles-managed settings.json is edited in
// place rather than replaced by a plain file.
func claudeSettingsPath() string {
	path := filepath.Join(absConfigDir(), "settings.json")
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func readSettings(path string) ([]byte, os.FileMode, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, 0644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return data, info.Mode().Perm(), nil
}

func handleStatusLine(args []string) {
	if len(args) == 0 {
		runStatusLine()
		return
	}
	switch args[0] {
	case "install":
		handleStatusLineInstall()
	case "uninstall":
		handleStatusLineUninstall()
	default:
		fmt.Fprintf(os.Stderr, "Unknown statusline command: %s\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// currentFeedCommand is the feed command for this binary and instance.
func currentFeedCommand() string {
	binPath, err := resolveBinaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve binary path: %v\n", err)
		os.Exit(1)
	}
	configDir := ""
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		configDir = absConfigDir()
	}
	return statusLineFeedCommand(binPath, configDir)
}

func handleStatusLineInstall() {
	feed := currentFeedCommand()

	path := claudeSettingsPath()
	settings, perm, err := readSettings(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", path, err)
		os.Exit(1)
	}
	out, original, already, err := installStatusLineSettings(settings, feed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update %s: %v\n", path, err)
		os.Exit(1)
	}

	if already {
		// The wrap file from the first install still holds the original;
		// without it the wrapper would silently drop the user's status line.
		if _, err := readStatusLineWrap(); err != nil {
			fmt.Fprintf(os.Stderr, "Status line already points at claude-monitor-lite, but %s is unreadable (%v).\n", statusLineWrapFile(), err)
			fmt.Fprintln(os.Stderr, "Set statusLine.command in settings.json back to your own command, then install again.")
			os.Exit(1)
		}
	} else {
		// Record the original before touching settings.json, so a failure
		// here leaves Claude Code exactly as it was.
		if err := writeFileAtomic(statusLineWrapFile(), []byte(original), configFilePermissions); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save the original status line: %v\n", err)
			os.Exit(1)
		}
		// Keep the first backup: a later one may already hold our command.
		backup := path + ".claude-monitor-lite.bak"
		if _, err := os.Stat(backup); settings != nil && os.IsNotExist(err) {
			if err := writeFileAtomic(backup, settings, perm); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to back up %s: %v\n", path, err)
				os.Exit(1)
			}
		}
	}

	if err := writeFileAtomic(path, out, perm); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
		os.Exit(1)
	}

	fmt.Println("✓ Claude Code's status line now feeds live usage to the menu bar.")
	fmt.Printf("  Settings: %s\n", path)
	if !already {
		if original == "" {
			fmt.Println("  You had no status line, so it stays blank.")
		} else {
			fmt.Printf("  Your status line still runs: %s\n", original)
		}
	}
	fmt.Println("  Takes effect on Claude Code's next status line render.")
	fmt.Println("  Remove with: claude-monitor-lite statusline uninstall")
}

func handleStatusLineUninstall() {
	wrapped, err := readStatusLineWrap()
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", statusLineWrapFile(), err)
		os.Exit(1)
	}
	installed := err == nil

	path := claudeSettingsPath()
	settings, perm, err := readSettings(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", path, err)
		os.Exit(1)
	}
	out, ours, err := uninstallStatusLineSettings(settings, wrapped, currentFeedCommand())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update %s: %v\n", path, err)
		os.Exit(1)
	}
	if ours && !installed {
		fmt.Fprintf(os.Stderr, "The status line points at claude-monitor-lite, but the original command was not recorded (%s missing).\n", statusLineWrapFile())
		fmt.Fprintln(os.Stderr, "Edit statusLine in settings.json by hand.")
		os.Exit(1)
	}
	if !ours {
		// The wrap file may be the only remaining copy of the original
		// command, so it stays.
		if installed {
			fmt.Printf("✓ Status line no longer points at claude-monitor-lite; left settings.json as is.\n  Your earlier command is kept in %s\n", statusLineWrapFile())
		} else {
			fmt.Println("✓ Status line feed not installed (nothing to remove).")
		}
		return
	}

	if err := writeFileAtomic(path, out, perm); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
		os.Exit(1)
	}
	for _, f := range []string{statusLineWrapFile(), statusLineUsageFile()} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove %s: %v\n", f, err)
		}
	}
	if wrapped == "" {
		fmt.Println("✓ Removed the status line feed (you had no status line before).")
	} else {
		fmt.Printf("✓ Restored your status line: %s\n", wrapped)
	}
}
