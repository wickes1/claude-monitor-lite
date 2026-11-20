package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

const (
	// Base poll interval. oauth/usage rate-limits non-Claude-Code clients per
	// account: on a Max account a 60s poll got 429 on every second request
	// (roughly one request per ~2min allowed), while a Team account took 60s
	// fine. 180s clears the strictest observed limit with jitter headroom and
	// matches ccstatusline's cache age for the same endpoint.
	refreshInterval    = 180 * time.Second
	maxBackoffInterval = 300 * time.Second // cap when the endpoint rate-limits us (429)
	pidCheckTimeout    = 500 * time.Millisecond
	pidFilePermissions = 0644 // Owner read/write, others read
	logFilePermissions = 0600 // Owner read/write only (may contain error bodies)
)

// version is set via ldflags at build time (e.g., -X main.version=v1.0.0)
var version = "dev"

// githubURL is the project repository, opened from the About window.
const githubURL = "https://github.com/wickes1/claude-monitor-lite"

var (
	// appConfig is written by menu clicks and read by the update workers;
	// once the menu is up it is guarded by displayMu (monitor.go).
	appConfig Config
	pidFile   string

	// Context for graceful shutdown
	appCtx    context.Context
	appCancel context.CancelFunc
)

// maxMenuRows is the size of each profile's row pool. Accounts observed so
// far report three or four rows; rows past the pool are left off the menu but
// still printed by the status command.
const maxMenuRows = 12

// Helper function to round utilization to nearest integer
func roundUtilization(utilization float64) int {
	return int(utilization + 0.5)
}

// Helper function to get color indicator based on utilization
func getColorIndicator(utilization float64) string {
	if utilization < 50.0 {
		return "🟢"
	}
	if utilization < 80.0 {
		return "🟡"
	}
	return "🔴"
}

// Helper function to round minutes to nearest 10
func roundToTenMinutes(minutes int) int {
	return ((minutes + 5) / 10) * 10
}

// Helper function to calculate time until reset
func calculateTimeUntilReset(resetTime time.Time) (hours, minutes int, valid bool) {
	if resetTime.IsZero() {
		return 0, 0, false
	}

	// Truncate current time to the minute (ignore seconds)
	now := time.Now()
	nowTruncated := time.Date(now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), 0, 0, now.Location())

	duration := resetTime.Sub(nowTruncated)
	if duration < 0 {
		return 0, 0, false
	}

	// Whole minutes remaining, truncated to the minute
	totalMinutes := int(duration.Minutes())

	return totalMinutes / 60, totalMinutes % 60, true
}

// Helper function to format reset time for display
func formatResetTime(resetTime time.Time) string {
	local := resetTime.Local()

	// Round minutes to nearest 10
	roundedMinutes := roundToTenMinutes(local.Minute())

	// Adjust hour if minutes rolled over
	hour := local.Hour()
	if roundedMinutes >= 60 {
		hour = (hour + 1) % 24
		roundedMinutes = 0
	}

	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d",
		local.Year(), local.Month(), local.Day(), hour, roundedMinutes)
}

// rowDetail is what follows a row's percentage: when it resets, or failing
// that the row's note.
func rowDetail(row usageRow) string {
	if row.limit != nil {
		if hours, minutes, ok := calculateTimeUntilReset(row.limit.ResetsAt); ok {
			return fmt.Sprintf("resets %s, in %dh %dm", formatResetTime(row.limit.ResetsAt), hours, minutes)
		}
		// The 5-hour window opens with the first message, so a session with
		// nothing to reset has not started.
		if row.key == kindSession {
			return "no active session"
		}
	}
	return row.note
}

// formatRow renders a row as a menu item title, or as a line of the status
// command's output when console is set.
func formatRow(row usageRow, console bool) string {
	gap, value := " ", "--"
	if console {
		gap = "  "
	}
	if row.limit != nil {
		value = fmt.Sprintf("%d%%", roundUtilization(row.limit.Utilization))
		if console {
			value = fmt.Sprintf("%4s", value)
		}
	}
	text := row.label + ":" + gap + value
	if detail := rowDetail(row); detail != "" {
		text += gap + "(" + detail + ")"
	}
	return text
}

// displayUsageStats prints every usage row, including any the menu's row pool
// has no room for.
func displayUsageStats(title string, limits *UsageLimits) {
	fmt.Printf("=== %s ===\n", title)
	rows := usageRows(limits)
	for _, row := range rows {
		fmt.Println(formatRow(row, true))
	}
	if len(rows) == 0 {
		fmt.Println(noLimitsMessage)
	}
	fmt.Println()
}

// menuBarTitle is the menu bar text for the headline row; ok is false when
// there is no row to show.
func menuBarTitle(row usageRow, ok bool) string {
	if !ok || row.limit == nil {
		return "⚪ --"
	}
	utilization := roundUtilization(row.limit.Utilization)
	color := getColorIndicator(row.limit.Utilization)
	if hours, minutes, hasTime := calculateTimeUntilReset(row.limit.ResetsAt); hasTime {
		return fmt.Sprintf("%s %d%% (%dh%dm)", color, utilization, hours, minutes)
	}
	return fmt.Sprintf("%s %d%%", color, utilization)
}

func main() {
	// Every dotfile path hangs off the home directory; without it they would
	// silently resolve relative to the working directory.
	if _, err := os.UserHomeDir(); err != nil {
		log.Fatal("Failed to get home directory:", err)
	}
	pidFile = monitorFile(".pid")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			handleAutoStart()
		case "stop":
			handleStop()
		case "install":
			handleInstall()
		case "uninstall":
			handleUninstall()
		case "add":
			handleProfileAdd(os.Args[2:])
		case "remove":
			handleProfileRemove(os.Args[2:])
		case "statusline":
			handleStatusLine(os.Args[2:])
		case "version", "--version", "-v":
			fmt.Printf("claude-monitor-lite %s\n", version)
			os.Exit(0)
		case "help", "--help", "-h":
			printUsage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
			printUsage()
			os.Exit(1)
		}
		return
	}

	// Default command (no args) - auto-handle everything
	handleAutoStart()
}

func printUsage() {
	fmt.Printf("Claude Monitor Lite %s - Menu bar monitor for Claude usage\n", version)
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  claude-monitor-lite              Start monitor (show status if running)")
	fmt.Println("  claude-monitor-lite start        Same as above")
	fmt.Println("  claude-monitor-lite stop         Stop the monitor")
	fmt.Println("  claude-monitor-lite add <dir>    Also watch the Claude Code environment in <dir>")
	fmt.Println("  claude-monitor-lite remove <dir> Stop watching it (\"default\" names ~/.claude)")
	fmt.Println("  claude-monitor-lite install      Start automatically at login (LaunchAgent)")
	fmt.Println("  claude-monitor-lite uninstall    Stop starting automatically at login")
	fmt.Println("  claude-monitor-lite statusline install    Live usage from Claude Code's status line")
	fmt.Println("  claude-monitor-lite statusline uninstall  Restore your original status line")
	fmt.Println("  claude-monitor-lite version      Show version")
	fmt.Println("  claude-monitor-lite help         Show this help")
	fmt.Println()
	fmt.Println("Reads your Claude Code login. <dir> is another environment's CLAUDE_CONFIG_DIR;")
	fmt.Println("each watched environment is a group in the menu, named after its account.")
}

// handleAutoStart shows status if the monitor is running, otherwise starts it.
// A missing Claude Code login is reported here, at the terminal, rather than
// as a menu bar that can only say "Login". With several profiles the monitor
// still starts while at least one is logged in; the rest say so in the menu.
func handleAutoStart() {
	loadConfig()
	if isRunning() {
		handleStatusDisplay()
		return
	}
	loggedIn := false
	for _, p := range appConfig.profiles() {
		if _, err := p.readCredential(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		} else {
			loggedIn = true
		}
	}
	if !loggedIn {
		os.Exit(1)
	}
	fmt.Println("⚙️  Starting Claude Monitor Lite...")
	handleStart()
}

func handleStatusDisplay() {
	pid, _ := readValidPID()
	fmt.Printf("✓ Already running (PID: %d)\n", pid)
	fmt.Println()

	profiles := appConfig.profiles()
	accounts := make([]account, len(profiles))
	for i, p := range profiles {
		accounts[i] = p.readAccount()
	}
	labels := profileLabels(profiles, accounts)

	var shows string
	for i, p := range profiles {
		title := "Current Usage"
		if len(profiles) > 1 {
			title = labels[i]
		}
		path := p.file(".usage.json")
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("=== %s ===\nNo usage fetched yet — check the menu bar in a moment.\n\n", title)
			continue
		}
		var limits *UsageLimits
		if err := json.Unmarshal(data, &limits); err != nil {
			// The other profiles' usage is still worth printing.
			fmt.Fprintf(os.Stderr, "Unreadable usage cache %s: %v\n\n", path, err)
			continue
		}
		if info, err := os.Stat(path); err == nil {
			fmt.Printf("As of %s:\n", formatAsOf(info.ModTime(), time.Now()))
		}
		displayUsageStats(title, limits)

		if p.dir != appConfig.MenuBarIndicator.Dir {
			continue
		}
		row, auto, ok := headlineRow(usageRows(limits), appConfig.MenuBarIndicator.Key)
		if !ok {
			continue
		}
		value := "--"
		if row.limit != nil {
			value = fmt.Sprintf("%s %d%%", getColorIndicator(row.limit.Utilization), roundUtilization(row.limit.Utilization))
		}
		if auto {
			value += ", Claude's pick"
		}
		shows = fmt.Sprintf("%s (%s)", row.label, value)
		if len(profiles) > 1 {
			shows = labels[i] + " — " + shows
		}
	}
	if shows != "" {
		fmt.Printf("Menu Bar Shows:  %s\n", shows)
	}
}

func handleStart() {
	if os.Getenv("CLAUDE_MONITOR_DAEMON") != "1" {
		if isRunning() {
			fmt.Println("Claude Monitor Lite is already running.")
			fmt.Println("Use 'claude-monitor-lite stop' to stop it first.")
			os.Exit(1)
		}
	}

	daemonize()

	// We are now the detached daemon child — route logs to a file.
	setupDaemonLogging()

	if err := createPIDFile(); err != nil {
		log.Fatal("Failed to create PID file:", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cleanup()
		os.Exit(0)
	}()

	systray.Run(onReady, onExit)
}

func handleStop() {
	pid, err := stopRunning()
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "Failed to stop process: %v\n", err)
		os.Exit(1)
	case pid == 0:
		fmt.Println("Claude Monitor Lite is not running.")
	default:
		fmt.Printf("Claude Monitor Lite (PID: %d) stopped.\n", pid)
	}
}

// profileArg reads the one argument of add and remove: a config dir, or
// defaultProfileName for the default environment.
func profileArg(command string, args []string) profile {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintf(os.Stderr, "Usage: claude-monitor-lite %s <dir>   (%q names the default environment, ~/.claude)\n", command, defaultProfileName)
		os.Exit(1)
	}
	if args[0] == defaultProfileName {
		return profile{}
	}
	return newProfile(args[0])
}

// loadConfig reads the config for the commands that need it, and stops on
// one that cannot be used. The daemon has no terminal, so the reason goes to
// the log as well.
func loadConfig() {
	config, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		if f, ferr := openLogFile(); ferr == nil {
			log.New(f, "", log.LstdFlags).Printf("Not started: %v", err)
			f.Close()
		}
		os.Exit(1)
	}
	appConfig = config
}

// handleProfileAdd starts watching another Claude Code environment. It is
// checked here, at the terminal: a mistyped dir or an environment nobody is
// logged in to would otherwise only show as a group that never loads.
func handleProfileAdd(args []string) {
	p := profileArg("add", args)
	loadConfig()
	if slices.Contains(appConfig.Profiles, p.dir) {
		fmt.Printf("✓ Already watching %s.\n", p)
		return
	}
	if info, err := os.Stat(p.configDir()); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "❌ %s is not a directory. Give the environment's CLAUDE_CONFIG_DIR.\n", p.configDir())
		os.Exit(1)
	}
	if _, err := p.readCredential(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		if p.dir != "" {
			fmt.Fprintf(os.Stderr, "  Log in there with: CLAUDE_CONFIG_DIR=%s claude   (then /login)\n", shellQuote(p.dir))
		}
		os.Exit(1)
	}

	appConfig.watch(p.dir)
	saveProfiles()
	fmt.Printf("✓ Watching %s.\n", p)
	if stopForRestart() {
		handleStart()
	}
}

func handleProfileRemove(args []string) {
	p := profileArg("remove", args)
	loadConfig()
	switch err := appConfig.unwatch(p.dir); err {
	case errNotWatched:
		fmt.Fprintf(os.Stderr, "❌ Not watching %s. Watched:\n", p)
		for _, watched := range appConfig.profiles() {
			fmt.Fprintf(os.Stderr, "  %s\n", watched)
		}
		os.Exit(1)
	case errOnlyProfile:
		fmt.Fprintln(os.Stderr, "❌ It is the only environment watched. Add another first, or stop the monitor.")
		os.Exit(1)
	}

	// Stopped before its files change: a running monitor would write the
	// profile's usage cache back.
	restart := stopForRestart()
	saveProfiles()
	// The cache would otherwise be printed by a later add as that profile's
	// current usage.
	if err := os.Remove(p.file(".usage.json")); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove %s: %v\n", p.file(".usage.json"), err)
	}
	fmt.Printf("✓ No longer watching %s.\n", p)
	if _, err := os.Stat(statusLineWrapFile(p)); err == nil {
		fmt.Println("  Its status line still feeds the monitor. Restore it with:")
		if p.dir == "" {
			fmt.Println("    claude-monitor-lite statusline uninstall")
		} else {
			fmt.Printf("    CLAUDE_CONFIG_DIR=%s claude-monitor-lite statusline uninstall\n", shellQuote(p.dir))
		}
	}
	if restart {
		handleStart()
	}
}

func saveProfiles() {
	if err := SaveConfig(appConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save %s: %v\n", GetConfigPath(), err)
		os.Exit(1)
	}
}

// stopForRestart stops a running monitor and reports whether there was one,
// for the caller to start again once the profiles are saved: the menu is
// built once, at start.
func stopForRestart() bool {
	pid, err := stopRunning()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop the running monitor (restart it yourself to apply): %v\n", err)
		os.Exit(1)
	}
	return pid != 0
}

// stopRunning stops the running monitor and returns its PID, or 0 when
// none was running.
func stopRunning() (int, error) {
	pid, ok := readValidPID()
	if !ok {
		return 0, nil
	}
	// readValidPID already confirmed this PID is our process.
	process, _ := os.FindProcess(pid) // never errors on Unix
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return pid, err
	}
	time.Sleep(pidCheckTimeout)
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: Failed to remove PID file: %v\n", err)
	}
	return pid, nil
}

// isOurProcess reports whether pid belongs to a live claude-monitor-lite
// process. os.FindProcess never fails on Unix and PIDs are recycled, so a
// bare liveness check (Signal 0) can return a false positive for an unrelated
// process that inherited the PID; checking the command name guards against that.
func isOurProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	// `ps` exits non-zero if the PID does not exist.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "claude-monitor-lite")
}

// readValidPID returns the PID from the PID file only if it belongs to a live
// claude-monitor-lite process. A stale or recycled-PID file is removed and
// (0, false) returned.
func readValidPID() (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !isOurProcess(pid) {
		os.Remove(pidFile)
		return 0, false
	}
	return pid, true
}

func isRunning() bool {
	_, ok := readValidPID()
	return ok
}

// createPIDFile writes the daemon PID file. O_EXCL makes creation fail if the
// file already exists, so two daemons racing to start cannot both claim
// ownership (callers clear stale PID files via readValidPID before reaching here).
func createPIDFile() error {
	f, err := os.OpenFile(pidFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, pidFilePermissions)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strconv.Itoa(os.Getpid()))
	return err
}

// openLogFile opens the monitor's log for appending.
func openLogFile() (*os.File, error) {
	return os.OpenFile(monitorFile(".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePermissions)
}

// setupDaemonLogging routes the daemon's log output to a file. The daemon runs
// detached with no terminal attached, so otherwise every log line (transient
// errors, fetch failures) is discarded and the process cannot be diagnosed
// after the fact. Logs go to ~/.claude-monitor-lite.log.
func setupDaemonLogging() {
	f, err := openLogFile()
	if err != nil {
		return
	}
	log.SetOutput(f)
	// O_CREATE only sets permissions for a newly created file; enforce them on
	// a pre-existing log file too.
	if err := f.Chmod(logFilePermissions); err != nil {
		log.Printf("Warning: could not restrict log file permissions: %v", err)
	}
	log.Printf("claude-monitor-lite %s started (pid %d)", version, os.Getpid())
}

func cleanup() {
	if pidFile != "" {
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: Failed to remove PID file: %v\n", err)
		}
	}
}

func onReady() {
	// Create context for graceful shutdown
	appCtx, appCancel = context.WithCancel(context.Background())

	setStatusItemName()
	systray.SetTooltip("Claude Monitor Lite")

	for _, p := range appConfig.profiles() {
		monitors = append(monitors, newMonitor(p))
	}

	// One group per profile: its head item, then its rows. With one profile
	// the menu is the rows alone, exactly as before there were groups.
	for i, m := range monitors {
		if i > 0 {
			systray.AddSeparator()
		}
		m.headItem = systray.AddMenuItem("", "")
		m.headItem.Disable()
		m.rowItems = make([]*systray.MenuItem, maxMenuRows)
		for j := range m.rowItems {
			m.rowItems[j] = systray.AddMenuItem("", "Click to show in menu bar")
			m.rowItems[j].Hide()
		}
	}
	systray.AddSeparator()

	mRefresh := systray.AddMenuItem("Refresh Now", "Refresh usage data")
	mAbout := systray.AddMenuItem("About", "Version, accounts and how the monitor works")
	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	displayMu.Lock()
	render()
	displayMu.Unlock()

	for _, m := range monitors {
		// A single worker per profile owns its usage fetching, so updateStats
		// never runs concurrently with itself.
		go m.run(appCtx)

		// One goroutine per row item forwards its clicks — a single static
		// select cannot cover a pool.
		for j := range m.rowItems {
			go func(m *monitor, idx int) {
				for {
					select {
					case <-appCtx.Done():
						return
					case <-m.rowItems[idx].ClickedCh:
						if key, ok := m.rowKeyAt(idx); ok {
							selectIndicator(Indicator{Dir: m.profile.dir, Key: key})
						}
					}
				}
			}(m, j)
		}
	}

	// Event loop — handles the rest of the menu.
	go func() {
		for {
			select {
			case <-appCtx.Done():
				return
			case <-mQuit.ClickedCh:
				appCancel()
				systray.Quit()
				return
			case <-mRefresh.ClickedCh:
				for _, m := range monitors {
					m.requestRefresh()
				}
			case <-mAbout.ClickedCh:
				showAbout()
			}
		}
	}()
}

// maxTransientErrors is the number of consecutive transient failures tolerated
// before showing an error in the menu bar — about 9 minutes at refreshInterval.
const maxTransientErrors = 3

// updateAction is how the menu bar should react to a failed usage fetch.
type updateAction int

const (
	actionShowError updateAction = iota // show the error state
	actionShowAuth                      // show "Login" — session expired
	actionKeepStale                     // tolerate a transient blip, keep last good data
)

// classifyUpdate decides how the menu bar should react to a failed usage fetch,
// given the error, how many consecutive failures have occurred, and whether
// last-good data is cached.
//
// Being rate-limited keeps the last-good numbers for as long as it lasts: the
// backoff recovers on its own, and "Error" would only alarm a user whose
// account is fine. Any other transient failure (expired token, outage) gets
// maxTransientErrors polls of grace and then shows "Error", so the menu bar
// never presents frozen numbers as current.
func classifyUpdate(err error, consecutiveCount int, haveCache bool) updateAction {
	var rl *RateLimitError
	switch {
	case errors.Is(err, ErrAuthFailed):
		return actionShowAuth
	case errors.As(err, &rl) && haveCache:
		return actionKeepStale
	case errors.Is(err, ErrTransient) && consecutiveCount <= maxTransientErrors:
		return actionKeepStale
	}
	return actionShowError
}

// nextPollDelay returns the base interval normally, and on consecutive
// rate-limited (429) polls backs off exponentially from base toward max.
// rateLimitFailures is the count of consecutive 429s including this one
// (0 = not rate-limited). A usable server Retry-After (>0) overrides only
// upward — we never poll sooner than told.
func nextPollDelay(base, max time.Duration, rateLimitFailures int, retryAfter time.Duration) time.Duration {
	if rateLimitFailures <= 0 {
		return base
	}
	d := base
	for i := 1; i < rateLimitFailures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// pollJitterFraction is how far withJitter may spread a delay, as a fraction
// of the delay itself (0.10 = ±10%).
const pollJitterFraction = 0.10

// withJitter spreads a delay by ±10% so monitors polling the same account don't
// beat in lockstep.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := (rand.Float64()*2 - 1) * pollJitterFraction * float64(d)
	return d + time.Duration(delta)
}

func onExit() {
	if appCancel != nil {
		appCancel()
	}
	cleanup()
}
