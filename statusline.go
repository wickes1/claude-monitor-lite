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
// from claude.ai or the desktop app, usage credits and per-model windows never
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
	"slices"
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
// session's first API response. Behind a Claude gateway it carries a
// spend_limit instead, which is not read: a gateway login has no claude.ai
// plan for this monitor to poll in the first place.
type statusLineInput struct {
	SessionID  string `json:"session_id"`
	RateLimits *struct {
		FiveHour *statusLineWindow `json:"five_hour"`
		SevenDay *statusLineWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type statusLineWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"` // Unix epoch seconds
}

// usageLimit converts a window, or returns nil for one without a reset time:
// the feed drops a reading once its reset passes, so a reading that never
// resets would never leave it.
func (w *statusLineWindow) usageLimit() *UsageLimit {
	if w == nil || w.ResetsAt <= 0 {
		return nil
	}
	return &UsageLimit{Utilization: w.UsedPercentage, ResetsAt: time.Unix(w.ResetsAt, 0).UTC()}
}

// liveWindows is the two windows the status line reports. In the usage
// response they are the limits[] entries of kind session and weekly_all.
type liveWindows struct {
	FiveHour *UsageLimit
	SevenDay *UsageLimit
}

// statusLineLimits extracts the session and its usage windows from one status
// line render; limits is nil when the render carries none.
func statusLineLimits(input []byte) (session string, limits *liveWindows, err error) {
	var in statusLineInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", nil, err
	}
	if in.RateLimits == nil {
		return in.SessionID, nil, nil
	}
	limits = &liveWindows{
		FiveHour: in.RateLimits.FiveHour.usageLimit(),
		SevenDay: in.RateLimits.SevenDay.usageLimit(),
	}
	if limits.FiveHour == nil && limits.SevenDay == nil {
		return in.SessionID, nil, nil
	}
	return in.SessionID, limits, nil
}

// statusLineFeed is the usage file: what each Claude Code session last
// reported for each window, and when that reading last changed.
//
// It is kept per session because every open session re-renders its status
// line with the rate_limits of its own last API response, so an idle session
// repeats an old figure for as long as it stays open. A render therefore says
// nothing about how old its numbers are; only a session's numbers changing
// shows that a response just arrived. Ordering readings by that change is
// what lets the daemon show the newest one, including a lower one after
// Anthropic resets a limit in the middle of its window.
type statusLineFeed struct {
	Sessions map[string]*sessionWindows `json:"sessions"`
}

type sessionWindows struct {
	FiveHour *feedWindow `json:"five_hour,omitempty"`
	SevenDay *feedWindow `json:"seven_day,omitempty"`
}

type feedWindow struct {
	UsageLimit
	// ChangedAt is when the session's reading of this window last changed.
	// It is zero for a reading recorded on first sight, whose age is unknown:
	// the session may have been idle on that figure for hours.
	ChangedAt time.Time `json:"changed_at"`
}

// open reports whether the window has yet to reset. Claude Code stops
// reporting a window once its reset passes.
func (w *feedWindow) open(now time.Time) bool {
	return w != nil && w.ResetsAt.After(now)
}

// record folds one session's render into the feed and reports whether it
// changed anything. Renders that repeat the session's last reading — the 2s
// re-render, an idle session — change nothing, so the file is left alone.
func (f *statusLineFeed) record(session string, live *liveWindows, now time.Time) bool {
	s := f.Sessions[session]
	if s == nil {
		s = &sessionWindows{}
		f.Sessions[session] = s
	}
	five := recordWindow(&s.FiveHour, live.FiveHour, now)
	seven := recordWindow(&s.SevenDay, live.SevenDay, now)
	return five || seven
}

// recordWindow stores live in slot when it differs from the reading there. A
// window missing from the render is not news: the reading stays as it was.
func recordWindow(slot **feedWindow, live *UsageLimit, now time.Time) bool {
	prev := *slot
	switch {
	case live == nil:
		return false
	case prev == nil:
		*slot = &feedWindow{UsageLimit: *live}
		return true
	case prev.Utilization == live.Utilization && prev.ResetsAt.Equal(live.ResetsAt):
		return false
	}
	*slot = &feedWindow{UsageLimit: *live, ChangedAt: now}
	return true
}

// prune drops the sessions that have no open window left. A session is kept
// whole until then, expired windows included, so that its next window is
// recognised as a change rather than a first sight.
func (f *statusLineFeed) prune(now time.Time) {
	for id, s := range f.Sessions {
		if !s.FiveHour.open(now) && !s.SevenDay.open(now) {
			delete(f.Sessions, id)
		}
	}
}

// liveSince returns the windows some session saw change after since, each
// from the session whose reading changed last, or nil when there is none.
func (f *statusLineFeed) liveSince(since, now time.Time) *liveWindows {
	var five, seven *feedWindow
	for _, s := range f.Sessions {
		five = laterChange(five, s.FiveHour, now)
		seven = laterChange(seven, s.SevenDay, now)
	}
	live := &liveWindows{}
	if five != nil && five.ChangedAt.After(since) {
		live.FiveHour = &five.UsageLimit
	}
	if seven != nil && seven.ChangedAt.After(since) {
		live.SevenDay = &seven.UsageLimit
	}
	if live.FiveHour == nil && live.SevenDay == nil {
		return nil
	}
	return live
}

func laterChange(best, w *feedWindow, now time.Time) *feedWindow {
	if !w.open(now) || w.ChangedAt.IsZero() {
		return best
	}
	if best == nil || w.ChangedAt.After(best.ChangedAt) {
		return w
	}
	return best
}

// sameWindowTolerance absorbs the sub-second difference between a window's
// reset time as polled (22:49:59.56) and as the status line rounds it
// (22:50:00); distinct windows are hours apart.
const sameWindowTolerance = time.Minute

// newerWindow picks which of a polled reading and a later status line reading
// to show. A later reset time means a newer window altogether. Within one
// window the higher reading wins: the endpoint counts usage the response
// headers have not caught up with yet, so a status line reading a point below
// the poll is lag, and showing it would make the menu bar tick backwards. A
// real drop — a limit reset mid-window — is picked up by the next poll, which
// replaces the base outright.
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
// session and weekly rows of the last poll, leaving everything only polling
// can see (usage credits, per-model limits) as polled. It only updates rows
// the poll listed: which meters an account has is the server's to say.
func mergeStatusLine(base *UsageLimits, live *liveWindows) *UsageLimits {
	merged := *base
	merged.Limits = slices.Clone(base.Limits) // base's entries stay as polled
	for i := range merged.Limits {
		entry := &merged.Limits[i]
		var window *UsageLimit
		switch entry.Kind {
		case kindSession:
			window = live.FiveHour
		case kindWeeklyAll:
			window = live.SevenDay
		}
		newer := newerWindow(&UsageLimit{Utilization: entry.Percent, ResetsAt: entry.ResetsAt}, window)
		entry.Percent, entry.ResetsAt = newer.Utilization, newer.ResetsAt
	}
	return &merged
}

// statusLineUsageFile holds the status line feed. Its mtime is when some
// session's reading last changed.
func statusLineUsageFile(p profile) string {
	return p.file(".statusline-usage.json")
}

// statusLineWrapFile holds, as plain text, the status line command
// `statusline install` replaced — empty when there was none, in which case the
// wrapper prints nothing. Its existence marks the feed as installed. It lives
// apart from the config because the daemon rewrites the config from memory on
// every indicator click, which would drop it.
func statusLineWrapFile(p profile) string {
	return p.file(".statusline-wrap")
}

func readStatusLineWrap(p profile) (string, error) {
	data, err := os.ReadFile(statusLineWrapFile(p))
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
	// Claude Code runs this with its own CLAUDE_CONFIG_DIR, which is what says
	// whose usage the render carries.
	p := envProfile()
	input, err := io.ReadAll(os.Stdin)
	if err == nil {
		err = saveStatusLineUsage(p, input)
	}
	if err != nil {
		logStatusLineError(err)
	}

	wrapped, err := readStatusLineWrap(p)
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

// saveStatusLineUsage folds the render into the feed. A render that changes
// nothing leaves the file untouched, so its mtime stays the time a reading
// last moved.
//
// The read-merge-write is unlocked: two sessions saving in the same instant
// can drop one's reading, but that session records it again on its next
// render, which is cheaper than a file lock on every render.
func saveStatusLineUsage(p profile, input []byte) error {
	session, limits, err := statusLineLimits(input)
	if err != nil || limits == nil {
		return err
	}
	// An unreadable feed (a crash can leave the file empty) is replaced
	// rather than repaired.
	feed, _ := readStatusLineFeed(p)
	now := time.Now()
	if !feed.record(session, limits, now) {
		return nil
	}
	feed.prune(now)
	data, err := json.Marshal(feed)
	if err != nil {
		return err
	}
	return writeFileAtomic(statusLineUsageFile(p), data, configFilePermissions)
}

// readStatusLineFeed reads the feed; a missing file is an empty feed. It
// always returns a usable feed, alongside the error when the file is bad.
func readStatusLineFeed(p profile) (*statusLineFeed, error) {
	feed := &statusLineFeed{}
	data, err := os.ReadFile(statusLineUsageFile(p))
	if err == nil {
		err = json.Unmarshal(data, feed)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil || feed.Sessions == nil {
		feed.Sessions = make(map[string]*sessionWindows)
	}
	return feed, err
}

// logStatusLineError appends to the monitor's log. Stdout is the user's
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

// readStatusLineUpdate returns the feed if its file changed after since, or
// nil. The returned time is the file's mtime, advanced past a bad write too,
// so that one is reported once rather than on every tick.
func readStatusLineUpdate(p profile, since time.Time) (*statusLineFeed, time.Time, error) {
	info, err := os.Stat(statusLineUsageFile(p))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, since, nil
		}
		return nil, since, err
	}
	if !info.ModTime().After(since) {
		return nil, since, nil
	}
	feed, err := readStatusLineFeed(p)
	if err != nil {
		return nil, info.ModTime(), err
	}
	return feed, info.ModTime(), nil
}

// --- install / uninstall ---------------------------------------------------

// isStatusLineFeed reports whether a status line command is ours, under any
// binary path or CLAUDE_CONFIG_DIR prefix.
func isStatusLineFeed(command string) bool {
	command = strings.TrimSpace(command)
	return strings.Contains(command, "claude-monitor-lite") && strings.HasSuffix(command, " statusline")
}

// statusLineFeedCommand is the command Claude Code is pointed at. A
// CLAUDE_CONFIG_DIR environment pins the variable in the command, so the
// wrapper saves to that profile's files even if Claude Code were started
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

// claudeSettingsPath is Claude Code's user settings file for the profile,
// with symlinks resolved so a dotfiles-managed settings.json is edited in
// place rather than replaced by a plain file.
func claudeSettingsPath(p profile) string {
	path := filepath.Join(p.configDir(), "settings.json")
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

// currentFeedCommand is the feed command for this binary and profile.
func currentFeedCommand(p profile) string {
	binPath, err := resolveBinaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve binary path: %v\n", err)
		os.Exit(1)
	}
	return statusLineFeedCommand(binPath, p.dir)
}

// The install and uninstall commands act on the environment they are run in,
// like Claude Code itself: set CLAUDE_CONFIG_DIR to wire up another profile.
func handleStatusLineInstall() {
	p := envProfile()
	path, original, already, err := installStatusLine(p, currentFeedCommand(p))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
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

// installStatusLine points the profile's status line at feed and returns the
// settings file it changed, the command that was there before, and whether
// the feed was installed already. On an error it leaves nothing of its own
// behind: a wrap file from an install that did not happen would make
// `statusline uninstall` and the About window report live updates that are
// not there.
func installStatusLine(p profile, feed string) (path, original string, already bool, err error) {
	path = claudeSettingsPath(p)
	// Claude Code creates this directory on its first run. Without it there
	// is no status line to feed, and creating it here would turn a mistyped
	// CLAUDE_CONFIG_DIR into a stray directory.
	if dir := filepath.Dir(path); !isDir(dir) {
		return path, "", false, fmt.Errorf("%s does not exist: Claude Code has not run in this environment yet. Run it once (and check CLAUDE_CONFIG_DIR, if you set it), then install again", dir)
	}
	settings, perm, err := readSettings(path)
	if err != nil {
		return path, "", false, fmt.Errorf("failed to read %s: %w", path, err)
	}
	out, original, already, err := installStatusLineSettings(settings, feed)
	if err != nil {
		return path, "", false, fmt.Errorf("failed to update %s: %w", path, err)
	}

	if already {
		// The wrap file from the first install still holds the original;
		// without it the wrapper would silently drop the user's status line.
		if _, err := readStatusLineWrap(p); err != nil {
			return path, "", true, fmt.Errorf("status line already points at claude-monitor-lite, but %s is unreadable (%v).\nSet statusLine.command in settings.json back to your own command, then install again", statusLineWrapFile(p), err)
		}
	} else {
		// Record the original before touching settings.json, so a failure
		// here leaves Claude Code exactly as it was.
		if err := writeFileAtomic(statusLineWrapFile(p), []byte(original), configFilePermissions); err != nil {
			return path, original, false, fmt.Errorf("failed to save the original status line: %w", err)
		}
		// Keep the first backup: a later one may already hold our command.
		backup := path + ".claude-monitor-lite.bak"
		if _, err := os.Stat(backup); settings != nil && os.IsNotExist(err) {
			if err := writeFileAtomic(backup, settings, perm); err != nil {
				os.Remove(statusLineWrapFile(p))
				return path, original, false, fmt.Errorf("failed to back up %s: %w", path, err)
			}
		}
	}

	if err := writeFileAtomic(path, out, perm); err != nil {
		if !already {
			// settings.json still holds the original command, so the copy
			// in the wrap file is not needed.
			os.Remove(statusLineWrapFile(p))
		}
		return path, original, already, fmt.Errorf("failed to write %s: %w", path, err)
	}
	return path, original, already, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func handleStatusLineUninstall() {
	p := envProfile()
	wrapped, err := readStatusLineWrap(p)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", statusLineWrapFile(p), err)
		os.Exit(1)
	}
	installed := err == nil

	path := claudeSettingsPath(p)
	settings, perm, err := readSettings(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", path, err)
		os.Exit(1)
	}
	out, ours, err := uninstallStatusLineSettings(settings, wrapped, currentFeedCommand(p))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update %s: %v\n", path, err)
		os.Exit(1)
	}
	if ours && !installed {
		fmt.Fprintf(os.Stderr, "The status line points at claude-monitor-lite, but the original command was not recorded (%s missing).\n", statusLineWrapFile(p))
		fmt.Fprintln(os.Stderr, "Edit statusLine in settings.json by hand.")
		os.Exit(1)
	}
	if !ours {
		// The wrap file may be the only remaining copy of the original
		// command, so it stays.
		if installed {
			fmt.Printf("✓ Status line no longer points at claude-monitor-lite; left settings.json as is.\n  Your earlier command is kept in %s\n", statusLineWrapFile(p))
		} else {
			fmt.Println("✓ Status line feed not installed (nothing to remove).")
		}
		return
	}

	if err := writeFileAtomic(path, out, perm); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
		os.Exit(1)
	}
	for _, f := range []string{statusLineWrapFile(p), statusLineUsageFile(p)} {
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
