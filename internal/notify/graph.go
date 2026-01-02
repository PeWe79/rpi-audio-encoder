// Package notify provides notification services for silence alerts.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	graphBaseURL     = "https://graph.microsoft.com/v1.0"
	graphScope       = "https://graph.microsoft.com/.default"
	tokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

	// Retry settings
	maxRetries       = 3
	initialRetryWait = 1 * time.Second
	maxRetryWait     = 30 * time.Second
)

// GraphClient handles Microsoft Graph API email operations.
type GraphClient struct {
	fromAddress string
	httpClient  *http.Client
}

// NewGraphClient creates a new Graph API client with Client Credentials flow.
func NewGraphClient(cfg *types.GraphConfig) (*GraphClient, error) {
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("graph API requires tenant_id, client_id, and client_secret")
	}
	if cfg.FromAddress == "" {
		return nil, fmt.Errorf("graph API requires from_address (shared mailbox)")
	}

	conf := &clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     fmt.Sprintf(tokenURLTemplate, cfg.TenantID),
		Scopes:       []string{graphScope},
	}

	ctx := context.Background()
	httpClient := conf.Client(ctx)

	return &GraphClient{
		fromAddress: cfg.FromAddress,
		httpClient:  httpClient,
	}, nil
}

// graphMailRequest represents the Graph API sendMail request body.
type graphMailRequest struct {
	Message graphMessage `json:"message"`
}

type graphMessage struct {
	Subject      string           `json:"subject"`
	Body         graphBody        `json:"body"`
	ToRecipients []graphRecipient `json:"toRecipients"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphRecipient struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

type graphEmailAddress struct {
	Address string `json:"address"`
}

// SendMail sends an email via Graph API with retry logic.
func (c *GraphClient) SendMail(recipients []string, subject, body string) error {
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients specified")
	}

	toRecipients := make([]graphRecipient, 0, len(recipients))
	for _, addr := range recipients {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			toRecipients = append(toRecipients, graphRecipient{
				EmailAddress: graphEmailAddress{Address: addr},
			})
		}
	}

	if len(toRecipients) == 0 {
		return fmt.Errorf("no valid recipients after filtering")
	}

	payload := graphMailRequest{
		Message: graphMessage{
			Subject: subject,
			Body: graphBody{
				ContentType: "Text",
				Content:     body,
			},
			ToRecipients: toRecipients,
		},
	}

	return c.sendWithRetry(payload)
}

// sendWithRetry implements exponential backoff for failed requests.
func (c *GraphClient) sendWithRetry(payload graphMailRequest) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/users/%s/sendMail", graphBaseURL, c.fromAddress)
	retryWait := initialRetryWait

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryWait)
			retryWait *= 2
			if retryWait > maxRetryWait {
				retryWait = maxRetryWait
			}
		}

		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonData))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("send request: %w", err)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
			return nil
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			lastErr = fmt.Errorf("graph API returned %d: %s", resp.StatusCode, string(respBody))
			continue
		default:
			return fmt.Errorf("graph API error %d: %s", resp.StatusCode, string(respBody))
		}
	}

	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

// ValidateAuth attempts to acquire a token to validate the Graph API credentials.
func (c *GraphClient) ValidateAuth() error {
	// The httpClient already has a token source configured.
	// Making any request will trigger token acquisition.
	// We use a lightweight request to /me endpoint which will fail with 403
	// for app-only auth, but the token acquisition itself validates credentials.
	url := fmt.Sprintf("%s/users/%s", graphBaseURL, c.fromAddress)
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create validation request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Token acquisition failed
		if strings.Contains(err.Error(), "oauth2") || strings.Contains(err.Error(), "token") {
			return fmt.Errorf("authentication failed: %w", err)
		}
		return fmt.Errorf("validation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 200 = user exists and accessible
	// 403 = token valid but no User.Read permission (acceptable for Mail.Send only)
	// 404 = user/mailbox not found
	switch resp.StatusCode {
	case http.StatusOK, http.StatusForbidden:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("mailbox %s not found", c.fromAddress)
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication failed: invalid credentials")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("validation failed with status %d: %s", resp.StatusCode, string(body))
	}
}

// ValidateConfig checks if the Graph configuration has all required fields.
func ValidateConfig(cfg *types.GraphConfig) error {
	if cfg.TenantID == "" {
		return fmt.Errorf("tenant ID is required")
	}
	if cfg.ClientID == "" {
		return fmt.Errorf("client ID is required")
	}
	if cfg.ClientSecret == "" {
		return fmt.Errorf("client secret is required")
	}
	if cfg.FromAddress == "" {
		return fmt.Errorf("from address (shared mailbox) is required")
	}
	if cfg.Recipients == "" {
		return fmt.Errorf("recipients are required")
	}
	return nil
}

// IsConfigured returns true if the Graph configuration has the minimum required fields.
func IsConfigured(cfg *types.GraphConfig) bool {
	return cfg.TenantID != "" && cfg.ClientID != "" && cfg.ClientSecret != "" &&
		cfg.FromAddress != "" && cfg.Recipients != ""
}

// ParseRecipients splits a comma-separated recipients string into a slice.
func ParseRecipients(recipients string) []string {
	var result []string
	for _, r := range strings.Split(recipients, ",") {
		if r = strings.TrimSpace(r); r != "" {
			result = append(result, r)
		}
	}
	return result
}

// GetTokenSource returns an OAuth2 token source for the given config.
// This is used by the expiry checker to make authenticated requests.
func GetTokenSource(cfg *types.GraphConfig) (oauth2.TokenSource, error) {
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("graph API requires tenant_id, client_id, and client_secret")
	}

	conf := &clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     fmt.Sprintf(tokenURLTemplate, cfg.TenantID),
		Scopes:       []string{graphScope},
	}

	return conf.TokenSource(context.Background()), nil
}
