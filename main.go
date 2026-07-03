package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

const (
	refreshInterval    = 30 * time.Second
	pidCheckTimeout    = 500 * time.Millisecond
	pidFilePermissions = 0644 // Owner read/write, others read
	logFilePermissions = 0600 // Owner read/write only (may contain error bodies)
)

// version is set via ldflags at build time (e.g., -X main.version=v1.0.0)
var version = "dev"

// githubURL is the project repository, shown in (and opened from) the About dialog.
const githubURL = "https://github.com/wickes1/claude-monitor-lite"

var (
	// indicatorMenuItems holds the systray menu items, parallel to the
	// indicators table. Populated once in onReady.
	indicatorMenuItems []*systray.MenuItem

	// Refresh button
	mRefresh *systray.MenuItem

	// App config
	appConfig    Config
	pidFile      string
	claudeClient *ClaudeUsageClient

	// Guards appConfig.MenuBarIndicator — written by menu clicks (event loop),
	// read by the update worker.
	indicatorMutex sync.RWMutex

	// Last fetched limits for instant display switching (protected by mutex)
	lastLimits  *UsageLimits
	limitsMutex sync.RWMutex

	// Consecutive error tracking for transient error resilience
	consecutiveErrors int
	consecutiveMutex  sync.Mutex

	// Context for graceful shutdown
	appCtx    context.Context
	appCancel context.CancelFunc
)

// indicator is one selectable usage metric: a stable config key, a menu label,
// and how to pull its window out of a usage response. This table is the single
// source of truth — menu items, config validation, display and selection all
// derive from it, so a new API field means adding exactly one row here.
type indicator struct {
	key   string
	label string
	get   func(*UsageLimits) *UsageLimit
}

// indicators lists every metric the usage API returns, in API response order.
var indicators = []indicator{
	{"currentSession", "5-Hour Session", func(u *UsageLimits) *UsageLimit { return u.FiveHour }},
	{"weeklyAll", "Weekly (All)", func(u *UsageLimits) *UsageLimit { return u.SevenDay }},
	{"weeklyOAuthApps", "Weekly (OAuth Apps)", func(u *UsageLimits) *UsageLimit { return u.SevenDayOAuthApps }},
	{"weeklyOpus", "Weekly (Opus)", func(u *UsageLimits) *UsageLimit { return u.SevenDayOpus }},
	{"weeklySonnet", "Weekly (Sonnet)", func(u *UsageLimits) *UsageLimit { return u.SevenDaySonnet }},
	{"weeklyCowork", "Weekly (Cowork)", func(u *UsageLimits) *UsageLimit { return u.SevenDayCowork }},
	{"weeklyDesign", "Weekly (Design)", func(u *UsageLimits) *UsageLimit { return u.SevenDayOmelette }},
	{"tangelo", "Tangelo", func(u *UsageLimits) *UsageLimit { return u.Tangelo }},
	{"iguanaNecktie", "Iguana Necktie", func(u *UsageLimits) *UsageLimit { return u.IguanaNecktie }},
	{"designPromotional", "Design (Promotional)", func(u *UsageLimits) *UsageLimit { return u.OmelettePromotional }},
	{"nimbusQuill", "Nimbus Quill", func(u *UsageLimits) *UsageLimit { return u.NimbusQuill }},
	{"cinderCove", "Cinder Cove", func(u *UsageLimits) *UsageLimit { return u.CinderCove }},
	{"amberLadder", "Amber Ladder", func(u *UsageLimits) *UsageLimit { return u.AmberLadder }},
	{"extraUsage", "Extra Usage", extraUsageWindow},
}

// extraUsageWindow adapts the pay-as-you-go extra_usage object to the window
// shape so it can be displayed like any other metric. It has no reset time and
// reports nothing until pay-as-you-go is enabled and reporting a utilization.
func extraUsageWindow(u *UsageLimits) *UsageLimit {
	if u.ExtraUsage == nil || u.ExtraUsage.Utilization == nil {
		return nil
	}
	return &UsageLimit{Utilization: *u.ExtraUsage.Utilization}
}

// findIndicator returns the indicator for key, falling back to the first
// (default) entry when key is unknown.
func findIndicator(key string) indicator {
	for i := range indicators {
		if indicators[i].key == key {
			return indicators[i]
		}
	}
	return indicators[0]
}

// isValidIndicator reports whether key names a known indicator.
func isValidIndicator(key string) bool {
	for i := range indicators {
		if indicators[i].key == key {
			return true
		}
	}
	return false
}

// Helper function to create Claude client from session
func createClientFromSession(session *AuthSession) *ClaudeUsageClient {
	if session.AuthMode == AuthModeOAuth {
		return NewOAuthUsageClient(session.AccessToken)
	}
	if session.OrganizationID != "" {
		return NewClaudeUsageClientWithOrg(session.SessionKey, session.OrganizationID)
	}
	return NewClaudeUsageClient(session.SessionKey)
}

// tryRefreshOAuthSession recovers an expired OAuth session without user
// interaction, then rebuilds the live client. It prefers re-reading Claude
// Code's (self-refreshing) credential — which never risks rotating a refresh
// token out from under Claude Code — and only falls back to refreshing via our
// stored refresh token. Returns true if the session was recovered.
//
// Called only from the update worker goroutine, the sole owner of claudeClient,
// so the reassignment needs no lock.
// Returns nil on success; ErrTransient if the refresh failed only transiently
// (so the caller can keep last-good data instead of forcing a re-login); or
// ErrAuthFailed when the session genuinely cannot be refreshed.
func tryRefreshOAuthSession() error {
	session, err := LoadAuthSession()
	if err != nil || session.AuthMode != AuthModeOAuth {
		return ErrAuthFailed
	}
	// Prefer Claude Code's own (self-refreshing) credential — this never rotates
	// its refresh token out from under it.
	if cred, err := ReadClaudeCodeCredential(); err == nil && !cred.expired() {
		applyRefreshedCredential(session, cred)
		return nil
	}
	// Last resort: refresh via our stored refresh token. Surface the failure
	// kind so the caller can tell a transient blip from a dead token.
	if session.RefreshToken != "" {
		cred, err := RefreshAccessToken(session.RefreshToken)
		if err != nil {
			return err
		}
		applyRefreshedCredential(session, cred)
		return nil
	}
	return ErrAuthFailed
}

func applyRefreshedCredential(session *AuthSession, cred *OAuthCredential) {
	session.AccessToken = cred.AccessToken
	if cred.RefreshToken != "" {
		session.RefreshToken = cred.RefreshToken
	}
	session.ExpiresAt = cred.ExpiresAt
	if err := SaveAuthSession(session); err != nil {
		log.Printf("Failed to persist refreshed session: %v", err)
	}
	claudeClient = createClientFromSession(session)
}

// getMenuBarIndicator and setMenuBarIndicator guard appConfig.MenuBarIndicator,
// which is written by menu clicks (event loop) and read by the update worker.
func getMenuBarIndicator() string {
	indicatorMutex.RLock()
	defer indicatorMutex.RUnlock()
	return appConfig.MenuBarIndicator
}

func setMenuBarIndicator(key string) {
	indicatorMutex.Lock()
	appConfig.MenuBarIndicator = key
	indicatorMutex.Unlock()
}

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

// Helper function to format a single usage limit with reset time
func formatUsageWithReset(limit *UsageLimit, label string) string {
	if limit == nil {
		return fmt.Sprintf("%s --", label)
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)

	// Special case: no active session (0% with no reset time)
	if !hasTime && utilization == 0 {
		return fmt.Sprintf("%s %d%% (no active session)", label, utilization)
	}

	if hasTime {
		return fmt.Sprintf("%s %d%% (resets %s, in %dh %dm)",
			label, utilization, formatResetTime(limit.ResetsAtTime), hours, minutes)
	}

	return fmt.Sprintf("%s %d%%", label, utilization)
}

// Helper function to format usage limit for console display
func formatConsoleUsage(limit *UsageLimit, label string, noSessionMsg string) string {
	if limit == nil {
		return fmt.Sprintf("%s  --\n", label)
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)

	if hasTime {
		return fmt.Sprintf("%s  %3d%%  (resets %s, in %dh %dm)\n",
			label, utilization, formatResetTime(limit.ResetsAtTime), hours, minutes)
	}

	if noSessionMsg != "" {
		return fmt.Sprintf("%s  %3d%%  (%s)\n", label, utilization, noSessionMsg)
	}
	return fmt.Sprintf("%s  %3d%%\n", label, utilization)
}

// Helper function to get the selected limit based on indicator setting
func getSelectedLimit(limits *UsageLimits, indicatorKey string) *UsageLimit {
	return findIndicator(indicatorKey).get(limits)
}

// scopedLimitLabel builds the console label for a model/surface-scoped
// limits[] entry, e.g. "Weekly (Fable)". It prefers the model display name,
// falls back to the model ID, and finally to a generic "Scoped" label when
// neither is present.
func scopedLimitLabel(entry LimitEntry) string {
	if entry.Scope != nil && entry.Scope.Model != nil {
		if entry.Scope.Model.DisplayName != nil {
			return fmt.Sprintf("Weekly (%s)", *entry.Scope.Model.DisplayName)
		}
		if entry.Scope.Model.ID != nil {
			return fmt.Sprintf("Weekly (%s)", *entry.Scope.Model.ID)
		}
	}
	return "Weekly (Scoped)"
}

// scopedLimitToUsageLimit adapts a limits[] entry to the UsageLimit shape so
// it can be rendered with the existing formatConsoleUsage helper.
func scopedLimitToUsageLimit(entry LimitEntry) *UsageLimit {
	return &UsageLimit{
		Utilization:  entry.Percent,
		ResetsAt:     entry.ResetsAt,
		ResetsAtTime: entry.ResetsAtTime,
	}
}

// formatScopedLimitLine renders one model/surface-scoped limits[] entry as a
// console line, in the same format as formatConsoleUsage.
func formatScopedLimitLine(entry LimitEntry) string {
	return formatConsoleUsage(scopedLimitToUsageLimit(entry), scopedLimitLabel(entry)+":", "")
}

// Helper function to display usage stats. Windows with no data are skipped to
// match the menu, except the 5-hour session, which is always shown as the
// headline metric.
func displayUsageStats(limits *UsageLimits) {
	fmt.Println("=== Current Usage ===")
	for i := range indicators {
		limit := indicators[i].get(limits)
		isCurrentSession := indicators[i].key == "currentSession"
		if limit == nil && !isCurrentSession {
			continue
		}
		noSessionMsg := ""
		if isCurrentSession {
			noSessionMsg = "no active session"
		}
		fmt.Print(formatConsoleUsage(limit, indicators[i].label+":", noSessionMsg))
	}
	// Scoped limits (e.g. a weekly window narrowed to one model) are display
	// only — they are not selectable indicators and never touch config
	// validation or persistence. Unscoped entries (session, weekly_all) merely
	// duplicate five_hour/seven_day already shown above, so only scoped
	// entries are printed here.
	if limits != nil {
		for _, entry := range limits.Limits {
			if entry.Scope != nil {
				fmt.Print(formatScopedLimitLine(entry))
			}
		}
	}
	fmt.Println()
}

// Helper function to update menu bar display
func updateMenuBarDisplay(limits *UsageLimits) {
	limit := getSelectedLimit(limits, getMenuBarIndicator())

	if limit == nil {
		systray.SetTitle("⚪ --")
		return
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)
	color := getColorIndicator(limit.Utilization)

	if hasTime {
		systray.SetTitle(fmt.Sprintf("%s %d%% (%dh%dm)", color, utilization, hours, minutes))
	} else {
		systray.SetTitle(fmt.Sprintf("%s %d%%", color, utilization))
	}
}

func main() {
	appConfig = LoadConfig()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatal("Failed to get home directory:", err)
	}
	pidFile = filepath.Join(homeDir, ".claude-monitor-lite.pid")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			handleAutoStart()
		case "stop":
			handleStop()
		case "logout":
			handleLogout()
		case "install":
			handleInstall()
		case "uninstall":
			handleUninstall()
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
	fmt.Println("  claude-monitor-lite              Start monitor (login if needed, show status if running)")
	fmt.Println("  claude-monitor-lite start        Same as above")
	fmt.Println("  claude-monitor-lite stop         Stop the monitor")
	fmt.Println("  claude-monitor-lite logout       Clear session and stop monitor")
	fmt.Println("  claude-monitor-lite install      Start automatically at login (LaunchAgent)")
	fmt.Println("  claude-monitor-lite uninstall    Stop starting automatically at login")
	fmt.Println("  claude-monitor-lite version      Show version")
	fmt.Println("  claude-monitor-lite help         Show this help")
	fmt.Println()
	fmt.Println("First time? Just run: claude-monitor-lite")
}

func handleAutoStart() {
	// Check authentication first
	_, err := LoadAuthSession()
	if err != nil {
		// Not logged in - run login flow
		fmt.Println("⚠️  Not authenticated")
		fmt.Println()
		_, err = handleLoginFlow()
		if err != nil {
			os.Exit(1)
		}
	}

	// Check if already running
	if isRunning() {
		// Already running - show status
		handleStatusDisplay()
		return
	}

	// Not running - show current usage then start it
	session, err := LoadAuthSession()
	if err == nil {
		client := createClientFromSession(session)
		if limits, err := client.GetUsageLimits(); err == nil {
			displayUsageStats(limits)
		}
	}

	fmt.Println("⚙️  Starting Claude Monitor Lite...")
	fmt.Println()
	handleStart()
}

// loginValidationAttempts and loginRetryDelay control how hard the login flow
// tries to validate a freshly entered key before giving up.
const (
	loginValidationAttempts = 3
	loginRetryDelay         = 1 * time.Second
)

// validateSession checks that a session key works, retrying on transient
// failures (Cloudflare interstitials, network blips) so a momentary hiccup
// does not reject a valid key. A real auth failure returns immediately.
func validateSession(client *ClaudeUsageClient, attempts int, delay time.Duration) error {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = client.TestSession()
		if err == nil || errors.Is(err, ErrSessionExpired) {
			return err
		}
		if attempt < attempts {
			time.Sleep(delay)
		}
	}
	return err
}

func handleLoginFlow() (*AuthSession, error) {
	// Zero-paste path: reuse Claude Code's existing login if present.
	if session, err := LoginWithClaudeCode(); err == nil {
		fmt.Println("✓ Connected using your Claude Code login — no copy-paste needed.")
		fmt.Println()
		if limits, err := createClientFromSession(session).GetUsageLimits(); err != nil {
			fmt.Printf("Note: Could not fetch usage data: %v\n", err)
			fmt.Println()
		} else {
			displayUsageStats(limits)
		}
		return session, nil
	} else {
		fmt.Printf("Claude Code login not found (%v).\n", err)
		fmt.Println("Falling back to manual session key entry.")
		fmt.Println()
	}

	// Fallback: manual sessionKey paste.
	session, err := LoginWithBrowser()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
		return nil, err
	}

	// Test the session and fetch organization ID
	client := NewClaudeUsageClient(session.SessionKey)
	if err := validateSession(client, loginValidationAttempts, loginRetryDelay); err != nil {
		fmt.Fprintf(os.Stderr, "Session validation failed: %v\n", err)
		fmt.Println("The session key may be invalid. Please try again.")
		return nil, err
	}

	// Save the organization ID
	session.OrganizationID = client.organizationID
	if err := SaveAuthSession(session); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save organization ID: %v\n", err)
	}

	fmt.Println("✓ Session validated successfully!")
	fmt.Println()

	// Fetch and display current usage
	if limits, err := client.GetUsageLimits(); err != nil {
		fmt.Printf("Note: Could not fetch usage data: %v\n", err)
		fmt.Println()
	} else {
		displayUsageStats(limits)
	}

	return session, nil
}

func handleStatusDisplay() {
	pid, _ := readValidPID()
	fmt.Printf("✓ Already running (PID: %d)\n", pid)
	fmt.Println()

	// Load session
	session, err := LoadAuthSession()
	if err != nil {
		fmt.Println("❌ Not authenticated. Run 'claude-monitor-lite logout' then restart.")
		os.Exit(1)
	}

	client := createClientFromSession(session)
	limits, err := client.GetUsageLimits()
	if err != nil {
		fmt.Printf("Error loading usage data: %v\n", err)
		fmt.Println("Try running 'claude-monitor-lite logout' then restart.")
		os.Exit(1)
	}

	displayUsageStats(limits)

	// Show which indicator is displayed in menu bar
	selected := findIndicator(getMenuBarIndicator())

	limit := selected.get(limits)
	utilization := 0.0
	if limit != nil {
		utilization = limit.Utilization
	}

	fmt.Printf("Menu Bar Shows:  %s (%s %d%%)\n", selected.label, getColorIndicator(utilization), roundUtilization(utilization))
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
	pid, ok := readValidPID()
	if !ok {
		fmt.Println("Claude Monitor Lite is not running.")
		os.Exit(0)
	}

	// readValidPID already confirmed this PID is our process.
	process, _ := os.FindProcess(pid) // never errors on Unix
	if err := process.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop process: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Claude Monitor Lite (PID: %d) stopped.\n", pid)
	time.Sleep(pidCheckTimeout)
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: Failed to remove PID file: %v\n", err)
	}
}

func handleLogout() {
	// Stop daemon if running
	if pid, ok := readValidPID(); ok {
		fmt.Println("Stopping monitor...")
		process, _ := os.FindProcess(pid) // never errors on Unix
		process.Signal(syscall.SIGTERM)
		time.Sleep(pidCheckTimeout)
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: Failed to remove PID file: %v\n", err)
		}
	}

	// Clear session data only (preserves menu bar settings)
	if err := ClearAuthSession(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to clear session: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Logged out! Session data cleared.")
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

// setupDaemonLogging routes the daemon's log output to a file. The daemon runs
// detached with no terminal attached, so otherwise every log line (transient
// errors, fetch failures) is discarded and the process cannot be diagnosed
// after the fact. Logs go to ~/.claude-monitor-lite.log.
func setupDaemonLogging() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logPath := filepath.Join(homeDir, ".claude-monitor-lite.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePermissions)
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

	systray.SetTitle("⚪ Loading...")
	systray.SetTooltip("Claude Monitor Lite")

	// Check authentication. With no stored session, try the zero-paste Claude
	// Code path before asking the user to do anything.
	session, err := LoadAuthSession()
	if err != nil {
		if s, e := LoginWithClaudeCode(); e == nil {
			session, err = s, nil
		} else {
			// Daemon has no terminal, so log why auto-login fell through.
			log.Printf("Claude Code auto-login unavailable: %v", e)
		}
	}
	if err != nil {
		systray.SetTitle("⚪ Not logged in")
		mLogin := systray.AddMenuItem("⚠️  Please login first", "Login required")
		mLogin.Disable()
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Quit the application")

		go func() {
			<-mQuit.ClickedCh
			systray.Quit()
		}()

		fmt.Println("ERROR: Not authenticated. Please run 'claude-monitor-lite' to login first.")
		return
	}

	claudeClient = createClientFromSession(session)

	indicatorMenuItems = make([]*systray.MenuItem, len(indicators))
	for i := range indicators {
		indicatorMenuItems[i] = systray.AddMenuItem(indicators[i].label+": --", "Click to show in menu bar")
	}
	systray.AddSeparator()

	mRefresh = systray.AddMenuItem("Refresh Now", "Refresh usage data")
	mOpenConfig := systray.AddMenuItem("Open Config", "Open config file in editor")
	mAbout := systray.AddMenuItem("About", "Show version info")
	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	updateMenuCheckmarks()

	// A single worker owns all usage fetching, so updateStats never runs
	// concurrently with itself.
	go runUpdateWorker(appCtx)

	// One goroutine per indicator forwards its clicks — the indicator count is
	// dynamic, so a single static select cannot cover them.
	for i := range indicators {
		go func(idx int) {
			for {
				select {
				case <-appCtx.Done():
					return
				case <-indicatorMenuItems[idx].ClickedCh:
					selectIndicator(indicators[idx].key)
				}
			}
		}(i)
	}

	// Event loop — handles non-indicator menu interaction.
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
				requestRefresh()
			case <-mOpenConfig.ClickedCh:
				go openConfigFile()
			case <-mAbout.ClickedCh:
				go showAboutDialog()
			}
		}
	}()
}

// selectIndicator switches which usage metric the menu bar shows, updating the
// display immediately from cached data so the click feels instant.
func selectIndicator(key string) {
	setMenuBarIndicator(key)
	updateMenuCheckmarks()

	limitsMutex.RLock()
	cached := lastLimits
	limitsMutex.RUnlock()
	if cached != nil {
		// Re-apply visibility: the newly deselected window may now hide, and
		// the newly selected one must show even if it has no data.
		applyIndicatorMenu(cached)
		updateMenuBarDisplay(cached)
	}

	go func() {
		if err := SaveConfigPreservingSession(key); err != nil {
			log.Printf("Failed to save menu bar indicator: %v", err)
		}
	}()
}

// openConfigFile opens the config file in the user's default editor.
func openConfigFile() {
	if err := exec.Command("open", GetConfigPath()).Start(); err != nil {
		log.Printf("Failed to open config file: %v", err)
	}
}

// showAboutDialog shows a version dialog with a button that opens the project
// on GitHub.
func showAboutDialog() {
	script := fmt.Sprintf(`display dialog "Claude Monitor Lite\nVersion: %s" buttons {"View on GitHub", "OK"} default button "OK" with title "About"
if button returned of result is "View on GitHub" then open location "%s"`, version, githubURL)
	if err := exec.Command("osascript", "-e", script).Start(); err != nil {
		log.Printf("Failed to show about dialog: %v", err)
	}
}

func updateMenuCheckmarks() {
	current := getMenuBarIndicator()
	for i := range indicators {
		if indicators[i].key == current {
			indicatorMenuItems[i].Check()
		} else {
			indicatorMenuItems[i].Uncheck()
		}
	}
}

// applyIndicatorMenu refreshes every indicator menu item's title from limits and
// hides the ones with no data, so the menu is not cluttered with permanently
// empty windows. The selected indicator always stays visible — hiding it would
// hide its checkmark and leave the user unable to see their current choice.
func applyIndicatorMenu(limits *UsageLimits) {
	selected := getMenuBarIndicator()
	for i := range indicators {
		limit := indicators[i].get(limits)
		indicatorMenuItems[i].SetTitle(formatUsageWithReset(limit, indicators[i].label+":"))
		if limit != nil || indicators[i].key == selected {
			indicatorMenuItems[i].Show()
		} else {
			indicatorMenuItems[i].Hide()
		}
	}
}

// maxTransientErrors is the number of consecutive transient failures tolerated
// before showing an error in the menu bar. At 30s intervals, 3 = ~1.5 minutes.
const maxTransientErrors = 3

// updateAction is how the menu bar should react to a failed usage fetch.
type updateAction int

const (
	actionShowError updateAction = iota // show the error state
	actionShowAuth                      // show "Login" — session expired
	actionKeepStale                     // tolerate a transient blip, keep last good data
)

// classifyUpdate decides how the menu bar should react to a failed usage fetch,
// given the error and how many consecutive failures have occurred.
func classifyUpdate(err error, consecutiveCount int) updateAction {
	if errors.Is(err, ErrAuthFailed) {
		return actionShowAuth
	}
	if errors.Is(err, ErrTransient) && consecutiveCount <= maxTransientErrors {
		return actionKeepStale
	}
	return actionShowError
}

// refreshTrigger asks the update worker to fetch now. Sends coalesce, so rapid
// refresh requests do not pile up.
var refreshTrigger = make(chan struct{}, 1)

func requestRefresh() {
	select {
	case refreshTrigger <- struct{}{}:
	default:
	}
}

// runUpdateWorker is the single goroutine that fetches usage data — on a timer
// and on demand. Confining fetches to one goroutine means updateStats never
// runs concurrently with itself.
func runUpdateWorker(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	updateStats() // initial fetch
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			updateStats()
		case <-refreshTrigger:
			updateStats()
		}
	}
}

func updateStats() {
	if claudeClient == nil {
		systray.SetTitle("⚪ Error")
		return
	}

	limits, err := claudeClient.GetUsageLimits()
	if err != nil && errors.Is(err, ErrAuthFailed) {
		// An OAuth access token likely expired — try a silent refresh and one
		// retry before telling the user to log in again.
		switch refreshErr := tryRefreshOAuthSession(); {
		case refreshErr == nil:
			limits, err = claudeClient.GetUsageLimits()
		case errors.Is(refreshErr, ErrTransient):
			// Refresh was only rate-limited/transient — keep last-good data and
			// retry next tick rather than telling a logged-in user to re-login.
			err = ErrTransient
		}
	}
	if err != nil {
		consecutiveMutex.Lock()
		consecutiveErrors++
		count := consecutiveErrors
		consecutiveMutex.Unlock()

		// The error message goes on the first item, which must be visible even
		// if auto-hide had hidden it (e.g. the 5-hour window had no data).
		switch classifyUpdate(err, count) {
		case actionShowAuth:
			indicatorMenuItems[0].Show()
			indicatorMenuItems[0].SetTitle("Session expired - please login again")
			systray.SetTitle("⚪ Login")
		case actionKeepStale:
			log.Printf("Transient error (%d/%d): %v", count, maxTransientErrors, err)
		case actionShowError:
			log.Printf("Error fetching usage: %v", err)
			systray.SetTitle("⚪ Error")
			indicatorMenuItems[0].Show()
			indicatorMenuItems[0].SetTitle("Error loading data")
		}
		return
	}

	// Success — reset error counter
	consecutiveMutex.Lock()
	consecutiveErrors = 0
	consecutiveMutex.Unlock()

	// Refresh every indicator's title and hide the ones with no data.
	applyIndicatorMenu(limits)

	// Store limits for instant display switching (thread-safe)
	limitsMutex.Lock()
	lastLimits = limits
	limitsMutex.Unlock()

	// Update menu bar display
	updateMenuBarDisplay(limits)
}

func onExit() {
	if appCancel != nil {
		appCancel()
	}
	cleanup()
}
