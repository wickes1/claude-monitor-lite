package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	SevenDayOpus      *UsageLimit `json:"seven_day_opus,omitempty"`
	IguanaNecktie     *UsageLimit `json:"iguana_necktie,omitempty"`
	LastUpdated       time.Time   `json:"-"`
}

type UsageLimit struct {
	Utilization  float64   `json:"utilization"`
	ResetsAt     string    `json:"resets_at"`
	ResetsAtTime time.Time `json:"-"`
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
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
	return &ClaudeUsageClient{
		sessionKey: sessionKey,
		httpClient: sharedHTTPClient,
	}
}

func NewClaudeUsageClientWithOrg(sessionKey, organizationID string) *ClaudeUsageClient {
	return &ClaudeUsageClient{
		sessionKey:     sessionKey,
		organizationID: organizationID,
		httpClient:     sharedHTTPClient,
	}
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
	url := fmt.Sprintf("%s/organizations/%s/usage", claudeAPIBaseURL, c.organizationID)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set authentication cookie
	req.Header.Set("Cookie", fmt.Sprintf("sessionKey=%s", c.sessionKey))
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch usage limits: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%w (status %d)", ErrAuthFailed, resp.StatusCode)
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
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
	parseResetTime(limits.SevenDayOpus)

	limits.LastUpdated = time.Now()
	return &limits, nil
}

// fetchOrganizationID retrieves the organization ID from the account endpoint
func (c *ClaudeUsageClient) fetchOrganizationID() error {
	// Try to get organization ID from account/organizations endpoint
	url := fmt.Sprintf("%s/organizations", claudeAPIBaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Cookie", fmt.Sprintf("sessionKey=%s", c.sessionKey))
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
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
