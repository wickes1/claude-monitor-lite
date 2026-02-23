package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// UsageLimits represents the real-time usage data from Claude
// Based on actual API response from claude.ai/api/organizations/{org_id}/usage
type UsageLimits struct {
	FiveHour          *UsageLimit `json:"five_hour,omitempty"`
	SevenDay          *UsageLimit `json:"seven_day,omitempty"`
	SevenDayOAuthApps *UsageLimit `json:"seven_day_oauth_apps,omitempty"`
	SevenDaySonnet    *UsageLimit `json:"seven_day_sonnet,omitempty"`
	SevenDayOpus      *UsageLimit `json:"seven_day_opus,omitempty"`
	SevenDayCowork    *UsageLimit `json:"seven_day_cowork,omitempty"`
	IguanaNecktie     *UsageLimit `json:"iguana_necktie,omitempty"`
	ExtraUsage        *UsageLimit `json:"extra_usage,omitempty"`
	LastUpdated       time.Time   `json:"-"`
}

type UsageLimit struct {
	Utilization  float64   `json:"utilization"`
	ResetsAt     string    `json:"resets_at"`
	ResetsAtTime time.Time `json:"-"`
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
	}
	client.seedCookieJar()
	return client
}

func NewClaudeUsageClientWithOrg(sessionKey, organizationID string) *ClaudeUsageClient {
	client := &ClaudeUsageClient{
		sessionKey:     sessionKey,
		organizationID: organizationID,
		httpClient:     sharedHTTPClient,
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
				modifyAndSaveConfig(func(cfg *Config) {
					cfg.SessionKey = newKey
				})
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
	return code == http.StatusTooManyRequests ||
		code >= 500 ||
		code == http.StatusServiceUnavailable
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
	apiURL := fmt.Sprintf("%s/organizations/%s/usage", claudeAPIBaseURL, c.organizationID)

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
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Parse reset times
	parseResetTime := func(limit *UsageLimit) {
		if limit != nil && limit.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339, limit.ResetsAt); err == nil && !t.IsZero() {
				limit.ResetsAtTime = t
			}
		}
	}
	parseResetTime(limits.FiveHour)
	parseResetTime(limits.SevenDay)
	parseResetTime(limits.SevenDaySonnet)
	parseResetTime(limits.SevenDayOpus)
	parseResetTime(limits.SevenDayCowork)

	limits.LastUpdated = time.Now()
	return &limits, nil
}

// fetchOrganizationID retrieves the organization ID from the account endpoint
func (c *ClaudeUsageClient) fetchOrganizationID() error {
	apiURL := fmt.Sprintf("%s/organizations", claudeAPIBaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := c.doAPIRequest(ctx, apiURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.refreshSessionFromJar()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch organizations (status %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
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
