package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	running     bool
}

// NewSecretExpiryChecker creates a new expiry checker for the given config.
func NewSecretExpiryChecker(cfg *types.GraphConfig) *SecretExpiryChecker {
	return &SecretExpiryChecker{
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

// Start begins the background expiry checking goroutine.
func (c *SecretExpiryChecker) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
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
func (c *SecretExpiryChecker) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		close(c.stopCh)
		c.running = false
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
	cfg := c.cfg
	c.mu.Unlock()

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
	KeyID       string `json:"keyId"`
	DisplayName string `json:"displayName"`
}

// fetchExpiryInfo queries the Azure AD Graph API for credential expiry.
func (c *SecretExpiryChecker) fetchExpiryInfo(cfg *types.GraphConfig) (types.SecretExpiryInfo, error) {
	// Get or create token source
	c.mu.Lock()
	if c.tokenSource == nil {
		ts, err := GetTokenSource(cfg)
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

	// Query application by appId
	url := fmt.Sprintf("%s/applications(appId='%s')", graphBaseURL, cfg.ClientID)
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
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
