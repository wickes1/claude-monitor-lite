package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	usageURL        = "https://api.anthropic.com/api/oauth/usage"
	oauthBetaHeader = "oauth-2025-04-20"
	requestTimeout  = 10 * time.Second
)

var (
	ErrAuthFailed = errors.New("authentication failed")
	ErrTransient  = errors.New("transient error")
)

// RateLimitError is a 429 carrying the server's Retry-After hint (0 when
// absent or unusable). It satisfies errors.Is(err, ErrTransient) so the usual
// transient handling applies on top of the rate-limit backoff.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return "rate limited (status 429)" }

func (e *RateLimitError) Is(target error) bool { return target == ErrTransient }

// parseRetryAfter parses a Retry-After header value — either delta-seconds
// ("120") or an HTTP-date — into a duration. An absent, unparseable, zero,
// negative, or already-past value all mean "no usable server hint": it never
// returns a negative duration.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

var httpClient = &http.Client{Timeout: requestTimeout}

// ClaudeUsageClient polls the account-scoped usage endpoint Claude Code itself
// uses behind its /usage view.
type ClaudeUsageClient struct {
	url           string
	tokenProvider func() (string, error)
}

// NewUsageClient builds a client that authenticates with a token pulled from
// tokenProvider on every request. In production that is CurrentOAuthToken,
// which reads Claude Code's own credential, so no token is stored here.
func NewUsageClient(tokenProvider func() (string, error)) *ClaudeUsageClient {
	return &ClaudeUsageClient{url: usageURL, tokenProvider: tokenProvider}
}

// UsageLimits holds the parts of the usage response this monitor displays.
// The endpoint also returns many codename windows that are null on every
// account observed; they are left out rather than tracked.
//
// Limits is the server's own list of the meters this account has, and is what
// the menu is built from (rows.go).
type UsageLimits struct {
	ExtraUsage *ExtraUsageInfo `json:"extra_usage"`
	Limits     []LimitEntry    `json:"limits"`
}

// UsageLimit is one reading of a meter: how full it is and when it empties.
type UsageLimit struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"` // zero when the window has no reset
}

// ExtraUsageInfo is the usage credits that cover sends past the plan's limits.
// Amounts are in minor units of Currency (cents for USD).
type ExtraUsageInfo struct {
	// IsEnabled is false while credits cannot cover a send: turned off, or
	// out of credits. The other fields keep reporting either way.
	IsEnabled     bool     `json:"is_enabled"`
	MonthlyLimit  *float64 `json:"monthly_limit"` // null when spend is uncapped
	UsedCredits   *float64 `json:"used_credits"`
	Utilization   *float64 `json:"utilization"` // null without a monthly limit
	Currency      string   `json:"currency"`
	DecimalPlaces *int     `json:"decimal_places"`
}

// LimitEntry is one entry in limits[]: a meter the server says this account
// has. Which kinds appear depends on the plan, so nothing here is assumed to
// be present.
type LimitEntry struct {
	Kind     string      `json:"kind"` // "session", "weekly_all", "weekly_scoped", …
	Percent  float64     `json:"percent"`
	ResetsAt time.Time   `json:"resets_at"`
	Scope    *LimitScope `json:"scope"`
	// IsActive marks the entry the server picks for a display with room for
	// one number.
	IsActive bool `json:"is_active"`
}

// LimitScope narrows an entry to one model or one surface.
type LimitScope struct {
	Model   *LimitScopeName `json:"model"`
	Surface *LimitScopeName `json:"surface"`
}

type LimitScopeName struct {
	DisplayName string `json:"display_name"`
}

// GetUsageLimits fetches the current usage. Errors are classified for the
// caller: ErrAuthFailed means the user must log in to Claude Code again,
// ErrTransient (including *RateLimitError) means keep the last good data.
func (c *ClaudeUsageClient) GetUsageLimits() (*UsageLimits, error) {
	token, err := c.tokenProvider()
	if err != nil {
		// A transient provider failure (Claude Code's token briefly expired
		// between its own refreshes) must stay transient — reporting it as an
		// auth failure would tell a logged-in user to sign in again.
		if errors.Is(err, ErrTransient) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrAuthFailed, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	// Identify honestly as ourselves rather than spoofing claude-code; a
	// User-Agent must be present — a missing one triggers persistent 429s.
	//
	// A claude-code User-Agent would dodge the stricter per-account limit
	// this endpoint applies to third-party clients (tested: same token, only
	// the UA changed, 429 vs 200). Don't: that limit is deliberate,
	// impersonating the official client to bypass it puts the user's own
	// account at risk and would ship to every user of this tool, the version
	// string would need tracking, and the gain is only fresher data. Respect
	// the limit through the poll interval instead (refreshInterval in main.go).
	req.Header.Set("User-Agent", "claude-monitor-lite/"+version)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w (status 401)", ErrAuthFailed)
	case http.StatusTooManyRequests:
		return nil, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		// Outages and edge challenges pass; if one persists, classifyUpdate
		// turns it into a visible error.
		return nil, fmt.Errorf("%w: status %d", ErrTransient, resp.StatusCode)
	}

	var limits UsageLimits
	if err := json.NewDecoder(resp.Body).Decode(&limits); err != nil {
		// A non-JSON 200 is an edge interstitial or a truncated body.
		return nil, fmt.Errorf("%w: parse response: %v", ErrTransient, err)
	}
	return &limits, nil
}
