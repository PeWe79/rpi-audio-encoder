package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"golang.org/x/oauth2"
)

const (
	// expiryWarningDays is the number of days before expiration to show a warning.
	expiryWarningDays = 30
	// expiryCheckInterval is how often to re-check the secret expiry.
	expiryCheckInterval = 24 * time.Hour
)

// SecretExpiryChecker monitors the client secret expiration date.
type SecretExpiryChecker struct {
	mu          sync.RWMutex
	cfg         *types.GraphConfig
	tokenSource oauth2.TokenSource
	cachedInfo  types.SecretExpiryInfo
	lastCheck   time.Time
	stopCh      chan struct{}
	doneCh      chan struct{} // signals when current check completes
	running     bool
	checking    bool // true while a check is in progress
	httpClient  *http.Client
}

// NewSecretExpiryChecker creates a new expiry checker for the given config.
func NewSecretExpiryChecker(cfg *types.GraphConfig) *SecretExpiryChecker {
	return &SecretExpiryChecker{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// Start begins the background expiry checking goroutine.
func (c *SecretExpiryChecker) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	// Recreate channels for restart capability (previous Stop() closed them)
	c.stopCh = make(chan struct{})
	c.doneCh = make(chan struct{})
	c.running = true
	c.mu.Unlock()

	// Initial check
	c.check()

	// Background periodic check
	go func() {
		ticker := time.NewTicker(expiryCheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.check()
			case <-c.stopCh:
				return
			}
		}
	}()
}

// Stop halts the background expiry checking.
// It waits for any in-progress check to complete before returning.
func (c *SecretExpiryChecker) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	close(c.stopCh)
	c.running = false
	checking := c.checking
	doneCh := c.doneCh
	c.mu.Unlock()

	// Wait for in-progress check to complete
	if checking && doneCh != nil {
		<-doneCh
	}
}

// GetInfo returns the cached secret expiry information.
func (c *SecretExpiryChecker) GetInfo() types.SecretExpiryInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cachedInfo
}

// UpdateConfig updates the configuration and triggers a re-check.
func (c *SecretExpiryChecker) UpdateConfig(cfg *types.GraphConfig) {
	c.mu.Lock()
	c.cfg = cfg
	c.tokenSource = nil // Force new token source
	c.mu.Unlock()

	c.check()
}

// check queries the Azure AD Graph API for the app's credential expiry.
func (c *SecretExpiryChecker) check() {
	c.mu.Lock()
	c.checking = true
	cfg := c.cfg
	doneCh := c.doneCh
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.checking = false
		c.mu.Unlock()
		// Signal completion (non-blocking in case no one is waiting)
		select {
		case doneCh <- struct{}{}:
		default:
		}
	}()

	if cfg == nil || cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		c.mu.Lock()
		c.cachedInfo = types.SecretExpiryInfo{
			Error: "Graph API not configured",
		}
		c.lastCheck = time.Now()
		c.mu.Unlock()
		return
	}

	info, err := c.fetchExpiryInfo(cfg)
	c.mu.Lock()
	if err != nil {
		c.cachedInfo = types.SecretExpiryInfo{
			Error: err.Error(),
		}
	} else {
		c.cachedInfo = info
	}
	c.lastCheck = time.Now()
	c.mu.Unlock()
}

// applicationResponse represents the Graph API response for an application.
type applicationResponse struct {
	PasswordCredentials []passwordCredential `json:"passwordCredentials"`
}

type passwordCredential struct {
	EndDateTime string `json:"endDateTime"`
}

// fetchExpiryInfo queries the Azure AD Graph API for credential expiry.
func (c *SecretExpiryChecker) fetchExpiryInfo(cfg *types.GraphConfig) (types.SecretExpiryInfo, error) {
	// Get or create token source
	c.mu.Lock()
	if c.tokenSource == nil {
		ts, err := TokenSource(cfg)
		if err != nil {
			c.mu.Unlock()
			return types.SecretExpiryInfo{}, fmt.Errorf("create token source: %w", err)
		}
		c.tokenSource = ts
	}
	ts := c.tokenSource
	c.mu.Unlock()

	// Get token
	token, err := ts.Token()
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("acquire token: %w", err)
	}

	// Query application by appId with context for timeout
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	apiURL := fmt.Sprintf("%s/applications(appId='%s')", graphBaseURL, url.PathEscape(cfg.ClientID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return types.SecretExpiryInfo{}, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var appResp applicationResponse
	if err := json.Unmarshal(body, &appResp); err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("parse response: %w", err)
	}

	// Find the earliest expiring credential
	var earliest time.Time
	for _, cred := range appResp.PasswordCredentials {
		if cred.EndDateTime == "" {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, cred.EndDateTime)
		if err != nil {
			continue
		}
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}

	if earliest.IsZero() {
		return types.SecretExpiryInfo{
			Error: "no password credentials found",
		}, nil
	}

	daysLeft := int(time.Until(earliest).Hours() / 24)
	if daysLeft < 0 {
		daysLeft = 0
	}

	return types.SecretExpiryInfo{
		ExpiresAt:   earliest.Format(time.RFC3339),
		ExpiresSoon: daysLeft <= expiryWarningDays,
		DaysLeft:    daysLeft,
	}, nil
}
