package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const (
	claudeAPIBaseURL    = "https://claude.ai/api"
	oauthAPIBaseURL     = "https://api.anthropic.com/api"
	oauthBetaHeader     = "oauth-2025-04-20"
	defaultUserAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
	requestTimeout      = 10 * time.Second
	maxIdleConns        = 2
	maxIdleConnsPerHost = 1
	idleConnTimeout     = 90 * time.Second
)

var (
	// Typed errors for better error handling
	ErrAuthFailed     = errors.New("authentication failed - session may have expired")
	ErrOrgIDNotFound  = errors.New("organization ID not found in response")
	ErrSessionExpired = errors.New("session expired")
	ErrTransient      = errors.New("transient error")
)

// Shared HTTP client for connection pooling
var sharedHTTPClient = newHTTPClient()

type ClaudeUsageClient struct {
	sessionKey     string
	tokenProvider  func() (string, error)
	authMode       string
	httpClient     *http.Client
	organizationID string
	baseURL        string
}

// usageResponseKnownKeys lists every top-level key the usage endpoint is known
// to return. It must be updated in the same change that adds struct fields to
// UsageLimits, so the drift check in GetUsageLimits stays accurate. It only
// detects new top-level keys — drift nested inside extra_usage, spend, or any
// window object is not detected.
var usageResponseKnownKeys = map[string]bool{
	"five_hour":                  true,
	"seven_day":                  true,
	"seven_day_oauth_apps":       true,
	"seven_day_opus":             true,
	"seven_day_sonnet":           true,
	"seven_day_cowork":           true,
	"seven_day_omelette":         true,
	"tangelo":                    true,
	"iguana_necktie":             true,
	"omelette_promotional":       true,
	"nimbus_quill":               true,
	"cinder_cove":                true,
	"amber_ladder":               true,
	"extra_usage":                true,
	"limits":                     true,
	"spend":                      true,
	"member_dashboard_available": true,
}

// UsageLimits mirrors the claude.ai/api/organizations/{org_id}/usage response.
// Every field the endpoint returns is represented here in API response order so
// the response is captured in full; windows the account does not use arrive as
// null and unmarshal to nil pointers.
type UsageLimits struct {
	FiveHour                 *UsageLimit     `json:"five_hour,omitempty"`
	SevenDay                 *UsageLimit     `json:"seven_day,omitempty"`
	SevenDayOAuthApps        *UsageLimit     `json:"seven_day_oauth_apps,omitempty"`
	SevenDayOpus             *UsageLimit     `json:"seven_day_opus,omitempty"`
	SevenDaySonnet           *UsageLimit     `json:"seven_day_sonnet,omitempty"`
	SevenDayCowork           *UsageLimit     `json:"seven_day_cowork,omitempty"`
	SevenDayOmelette         *UsageLimit     `json:"seven_day_omelette,omitempty"`
	Tangelo                  *UsageLimit     `json:"tangelo,omitempty"`
	IguanaNecktie            *UsageLimit     `json:"iguana_necktie,omitempty"`
	OmelettePromotional      *UsageLimit     `json:"omelette_promotional,omitempty"`
	NimbusQuill              *UsageLimit     `json:"nimbus_quill,omitempty"`
	CinderCove               *UsageLimit     `json:"cinder_cove,omitempty"`
	AmberLadder              *UsageLimit     `json:"amber_ladder,omitempty"`
	ExtraUsage               *ExtraUsageInfo `json:"extra_usage,omitempty"`
	Limits                   []LimitEntry    `json:"limits,omitempty"`
	Spend                    *SpendInfo      `json:"spend,omitempty"`
	MemberDashboardAvailable bool            `json:"member_dashboard_available"`
}

type UsageLimit struct {
	Utilization      float64   `json:"utilization"`
	ResetsAt         *string   `json:"resets_at"`
	ResetsAtTime     time.Time `json:"-"`
	LimitDollars     *float64  `json:"limit_dollars"`
	UsedDollars      *float64  `json:"used_dollars"`
	RemainingDollars *float64  `json:"remaining_dollars"`
}

// ExtraUsageInfo mirrors the extra_usage object — pay-as-you-go credit state.
// All fields except IsEnabled are null until pay-as-you-go is turned on.
type ExtraUsageInfo struct {
	IsEnabled      bool     `json:"is_enabled"`
	MonthlyLimit   *float64 `json:"monthly_limit"`
	UsedCredits    *float64 `json:"used_credits"`
	Utilization    *float64 `json:"utilization"`
	Currency       *string  `json:"currency"`
	DisabledReason *string  `json:"disabled_reason"`
	DecimalPlaces  *int     `json:"decimal_places"`
	// Daily and Weekly are captured losslessly as raw JSON because their shape
	// is unknowable while they arrive as null — no observed non-null sample to
	// model a struct from.
	Daily  json.RawMessage `json:"daily"`
	Weekly json.RawMessage `json:"weekly"`
}

// LimitEntry is one entry in the limits[] array — a flattened, display-ready
// view of a usage window (session, weekly, or a model/surface-scoped weekly
// slice), separate from the legacy named windows above.
type LimitEntry struct {
	Kind         string      `json:"kind"`
	Group        string      `json:"group"`
	Percent      float64     `json:"percent"`
	Severity     string      `json:"severity"`
	ResetsAt     *string     `json:"resets_at"`
	ResetsAtTime time.Time   `json:"-"`
	Scope        *LimitScope `json:"scope"`
	IsActive     bool        `json:"is_active"`
}

// LimitScope narrows a LimitEntry to a specific model and/or surface. Surface
// is captured as raw JSON because it is only observed as null so far.
type LimitScope struct {
	Model   *LimitScopeModel `json:"model"`
	Surface json.RawMessage  `json:"surface"`
}

type LimitScopeModel struct {
	ID          *string `json:"id"`
	DisplayName *string `json:"display_name"`
}

// SpendInfo mirrors the spend object — pay-as-you-go dollar spend tracking,
// distinct from the credit-based ExtraUsageInfo. Limit, Cap, Balance and
// AutoReload are captured as raw JSON because they are only observed as null
// so far and their populated shape is unknown.
type SpendInfo struct {
	Used               *SpendUsed      `json:"used"`
	Limit              json.RawMessage `json:"limit"`
	Percent            float64         `json:"percent"`
	Severity           string          `json:"severity"`
	Enabled            bool            `json:"enabled"`
	DisabledReason     *string         `json:"disabled_reason"`
	Cap                json.RawMessage `json:"cap"`
	Balance            json.RawMessage `json:"balance"`
	AutoReload         json.RawMessage `json:"auto_reload"`
	Disclaimer         *string         `json:"disclaimer"`
	CanPurchaseCredits bool            `json:"can_purchase_credits"`
	CanToggle          bool            `json:"can_toggle"`
}

type SpendUsed struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Exponent    int    `json:"exponent"`
}

func newHTTPClient() *http.Client {
	// Use a cookie jar to automatically handle Cloudflare cookies
	// (__cf_bm, _cfuvid) and session key refreshes from Set-Cookie headers
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: requestTimeout,
		Jar:     jar,
		Transport: &http.Transport{
			MaxIdleConns:        maxIdleConns,
			MaxIdleConnsPerHost: maxIdleConnsPerHost,
			IdleConnTimeout:     idleConnTimeout,
			DisableCompression:  false,
			DisableKeepAlives:   false,
		},
	}
}

func NewClaudeUsageClient(sessionKey string) *ClaudeUsageClient {
	client := &ClaudeUsageClient{
		sessionKey: sessionKey,
		httpClient: sharedHTTPClient,
		baseURL:    claudeAPIBaseURL,
	}
	client.seedCookieJar()
	return client
}

func NewClaudeUsageClientWithOrg(sessionKey, organizationID string) *ClaudeUsageClient {
	client := &ClaudeUsageClient{
		sessionKey:     sessionKey,
		organizationID: organizationID,
		httpClient:     sharedHTTPClient,
		baseURL:        claudeAPIBaseURL,
	}
	client.seedCookieJar()
	return client
}

// NewOAuthUsageClient builds a client that authenticates against
// api.anthropic.com with a token pulled from tokenProvider on every request —
// the zero-paste path. tokenProvider is CurrentOAuthToken in production
// (credentials.go): it reads Claude Code's own credential fresh, in memory
// only, so no access token is ever stored on this struct or persisted to our
// config. No cookie jar or organization lookup is needed; the token is
// account-scoped.
func NewOAuthUsageClient(tokenProvider func() (string, error)) *ClaudeUsageClient {
	return &ClaudeUsageClient{
		tokenProvider: tokenProvider,
		authMode:      AuthModeOAuth,
		httpClient:    sharedHTTPClient,
		baseURL:       oauthAPIBaseURL,
	}
}

// seedCookieJar sets the initial sessionKey cookie in the jar so
// subsequent requests via the jar include it automatically.
func (c *ClaudeUsageClient) seedCookieJar() {
	u, _ := url.Parse("https://claude.ai")
	c.httpClient.Jar.SetCookies(u, []*http.Cookie{
		{Name: "sessionKey", Value: c.sessionKey},
	})
}

// refreshSessionFromJar checks if the API returned a new sessionKey
// via Set-Cookie and updates the client + persisted config.
func (c *ClaudeUsageClient) refreshSessionFromJar() {
	u, _ := url.Parse("https://claude.ai")
	for _, cookie := range c.httpClient.Jar.Cookies(u) {
		if cookie.Name == "sessionKey" && cookie.Value != "" && cookie.Value != c.sessionKey {
			c.sessionKey = cookie.Value
			// Persist the refreshed key to config in background
			go func(newKey string) {
				if err := modifyAndSaveConfig(func(cfg *Config) {
					cfg.SessionKey = newKey
				}); err != nil {
					log.Printf("Failed to persist refreshed session key: %v", err)
				}
			}(cookie.Value)
			return
		}
	}
}

// doAPIRequest performs an authenticated GET request with proper headers.
func (c *ClaudeUsageClient) doAPIRequest(ctx context.Context, apiURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if c.authMode == AuthModeOAuth {
		token, err := c.tokenProvider()
		if err != nil {
			// A transient provider failure (e.g. Claude Code's token is briefly
			// expired between its own background refreshes) must stay transient —
			// masking it as an auth failure would wrongly tell a logged-in user
			// to sign in again.
			if errors.Is(err, ErrTransient) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrAuthFailed, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-beta", oauthBetaHeader)
		// Identify honestly as ourselves rather than spoofing claude-code; the
		// endpoint accepts this, but a User-Agent must be present — a missing
		// one triggers persistent 429s.
		req.Header.Set("User-Agent", "claude-monitor-lite/"+version)
	} else {
		req.Header.Set("User-Agent", defaultUserAgent)
	}

	return c.httpClient.Do(req)
}

// isTransientStatus returns true for HTTP status codes that indicate a
// temporary problem (server error, rate limit, Cloudflare challenge).
func isTransientStatus(code int) bool {
	return code == http.StatusRequestTimeout || // 408
		code == http.StatusTooManyRequests || // 429
		code >= 500
}

// GetUsageLimits fetches real-time usage limits from Claude API
func (c *ClaudeUsageClient) GetUsageLimits() (*UsageLimits, error) {
	var apiURL string
	if c.authMode == AuthModeOAuth {
		// OAuth tokens are account-scoped, so there is no org lookup and no org
		// in the path — Claude Code hits this same endpoint behind its /usage view.
		apiURL = fmt.Sprintf("%s/oauth/usage", c.baseURL)
	} else {
		// First, get organization ID if not already cached
		if c.organizationID == "" {
			if err := c.fetchOrganizationID(); err != nil {
				return nil, fmt.Errorf("failed to get organization ID: %w", err)
			}
		}
		apiURL = fmt.Sprintf("%s/organizations/%s/usage", c.baseURL, c.organizationID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := c.doAPIRequest(ctx, apiURL)
	if err != nil {
		// A request that never went out (e.g. the OAuth token provider
		// failed) already carries a specific classification from
		// doAPIRequest — preserve it so an auth failure is never misreported
		// as a transient blip. Anything else here is a raw network-level
		// Do() failure, which is transient by nature.
		if errors.Is(err, ErrAuthFailed) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	// Pick up any refreshed sessionKey or Cloudflare cookies (cookie mode only)
	if c.authMode != AuthModeOAuth {
		c.refreshSessionFromJar()
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w (status %d)", ErrAuthFailed, resp.StatusCode)
	}

	// 403 could be Cloudflare bot challenge, not necessarily auth failure
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		// Check if this is a real auth failure or a Cloudflare challenge
		if strings.Contains(bodyStr, "cloudflare") || strings.Contains(bodyStr, "cf-") ||
			strings.Contains(bodyStr, "challenge") || strings.Contains(bodyStr, "<!DOCTYPE") {
			return nil, fmt.Errorf("%w: cloudflare challenge (status 403)", ErrTransient)
		}
		return nil, fmt.Errorf("%w (status %d)", ErrAuthFailed, resp.StatusCode)
	}

	if isTransientStatus(resp.StatusCode) {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: status %d: %s", ErrTransient, resp.StatusCode, string(body))
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read response: %v", ErrTransient, err)
	}

	var limits UsageLimits
	if err := json.Unmarshal(body, &limits); err != nil {
		// A 200 response with a non-JSON body is almost always a Cloudflare
		// interstitial ("Just a moment...") or a truncated/empty body from a
		// network hiccup. Treat it as transient so the menu bar tolerates it
		// instead of immediately flashing "Error".
		return nil, fmt.Errorf("%w: failed to parse response: %v", ErrTransient, err)
	}

	// Parse reset times (use RFC3339Nano to handle fractional seconds)
	parseResetTime := func(limit *UsageLimit) {
		if limit != nil && limit.ResetsAt != nil && *limit.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, *limit.ResetsAt); err == nil && !t.IsZero() {
				limit.ResetsAtTime = t
			}
		}
	}
	parseResetTime(limits.FiveHour)
	parseResetTime(limits.SevenDay)
	parseResetTime(limits.SevenDayOAuthApps)
	parseResetTime(limits.SevenDayOpus)
	parseResetTime(limits.SevenDaySonnet)
	parseResetTime(limits.SevenDayCowork)
	parseResetTime(limits.SevenDayOmelette)
	parseResetTime(limits.Tangelo)
	parseResetTime(limits.IguanaNecktie)
	parseResetTime(limits.OmelettePromotional)
	parseResetTime(limits.NimbusQuill)
	parseResetTime(limits.CinderCove)
	parseResetTime(limits.AmberLadder)

	for i := range limits.Limits {
		entry := &limits.Limits[i]
		if entry.ResetsAt != nil && *entry.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, *entry.ResetsAt); err == nil && !t.IsZero() {
				entry.ResetsAtTime = t
			}
		}
	}

	// Drift alarm: a second, lossless unmarshal into raw top-level keys lets us
	// detect a new field the API started returning before it silently vanishes
	// into an unparsed response. This only catches new top-level keys — drift
	// nested inside extra_usage, spend, or a window object is not detected.
	var rawTopLevel map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawTopLevel); err == nil {
		for key := range rawTopLevel {
			if !usageResponseKnownKeys[key] {
				log.Printf("usage response contains unknown top-level key %q — struct fields may need updating", key)
			}
		}
	}

	return &limits, nil
}

// fetchOrganizationID retrieves the organization ID from the account endpoint
func (c *ClaudeUsageClient) fetchOrganizationID() error {
	apiURL := fmt.Sprintf("%s/organizations", c.baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := c.doAPIRequest(ctx, apiURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	c.refreshSessionFromJar()

	// A 401 here means the session genuinely expired.
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w (status 401 fetching organizations)", ErrAuthFailed)
	}

	// Any other non-200 (5xx, 429, Cloudflare challenge, etc.) is treated as
	// transient so a brief hiccup does not flash "Error" on the first failure.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: organizations endpoint status %d: %s", ErrTransient, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: failed to read organizations response: %v", ErrTransient, err)
	}

	// Helper to extract org ID from map
	extractOrgID := func(org map[string]any) (string, bool) {
		if id, ok := org["uuid"].(string); ok {
			return id, true
		}
		if id, ok := org["id"].(string); ok {
			return id, true
		}
		return "", false
	}

	// Try parsing as array first
	var orgs []map[string]any
	if err := json.Unmarshal(body, &orgs); err == nil && len(orgs) > 0 {
		if id, ok := extractOrgID(orgs[0]); ok {
			c.organizationID = id
			return nil
		}
	} else {
		// Try as single object
		var org map[string]any
		if err := json.Unmarshal(body, &org); err == nil {
			if id, ok := extractOrgID(org); ok {
				c.organizationID = id
				return nil
			}
		}
	}

	return ErrOrgIDNotFound
}

// TestSession tests if the session key is still valid
func (c *ClaudeUsageClient) TestSession() error {
	_, err := c.GetUsageLimits()
	if err != nil && errors.Is(err, ErrAuthFailed) {
		return ErrSessionExpired
	}
	return err
}
