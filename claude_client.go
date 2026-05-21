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
	httpClient     *http.Client
	organizationID string
	baseURL        string
}

// UsageLimits mirrors the claude.ai/api/organizations/{org_id}/usage response.
// Every field the endpoint returns is represented here in API response order so
// the response is captured in full; windows the account does not use arrive as
// null and unmarshal to nil pointers.
type UsageLimits struct {
	FiveHour            *UsageLimit     `json:"five_hour,omitempty"`
	SevenDay            *UsageLimit     `json:"seven_day,omitempty"`
	SevenDayOAuthApps   *UsageLimit     `json:"seven_day_oauth_apps,omitempty"`
	SevenDayOpus        *UsageLimit     `json:"seven_day_opus,omitempty"`
	SevenDaySonnet      *UsageLimit     `json:"seven_day_sonnet,omitempty"`
	SevenDayCowork      *UsageLimit     `json:"seven_day_cowork,omitempty"`
	SevenDayOmelette    *UsageLimit     `json:"seven_day_omelette,omitempty"`
	Tangelo             *UsageLimit     `json:"tangelo,omitempty"`
	IguanaNecktie       *UsageLimit     `json:"iguana_necktie,omitempty"`
	OmelettePromotional *UsageLimit     `json:"omelette_promotional,omitempty"`
	ExtraUsage          *ExtraUsageInfo `json:"extra_usage,omitempty"`
}

type UsageLimit struct {
	Utilization  float64   `json:"utilization"`
	ResetsAt     *string   `json:"resets_at"`
	ResetsAtTime time.Time `json:"-"`
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

	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/json")

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
	// First, get organization ID if not already cached
	if c.organizationID == "" {
		if err := c.fetchOrganizationID(); err != nil {
			return nil, fmt.Errorf("failed to get organization ID: %w", err)
		}
	}

	// Build the actual endpoint
	apiURL := fmt.Sprintf("%s/organizations/%s/usage", c.baseURL, c.organizationID)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := c.doAPIRequest(ctx, apiURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	// Pick up any refreshed sessionKey or Cloudflare cookies
	c.refreshSessionFromJar()

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
