// Package notify provides notification services for silence alerts.
package notify

import (
	"fmt"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// GraphConfig is the Microsoft Graph configuration for email notifications.
type GraphConfig = types.GraphConfig

// SendSilenceAlert sends an email notification for critical silence via Microsoft Graph.
func SendSilenceAlert(cfg *GraphConfig, stationName string, durationMs int64, threshold float64) error {
	if !IsConfigured(cfg) {
		return nil // Silently skip if not configured
	}

	subject := "[ALERT] Silence Detected - " + stationName
	body := fmt.Sprintf(
		"Silence detected on the audio encoder.\n\n"+
			"Duration:  %.1f seconds\n"+
			"Threshold: %.1f dB\n"+
			"Time:      %s\n\n"+
			"Please check the audio source.",
		float64(durationMs)/1000.0, threshold, util.HumanTime(),
	)

	return sendEmail(cfg, subject, body)
}

// SendRecoveryAlert sends an email notification when audio recovers from silence via Microsoft Graph.
func SendRecoveryAlert(cfg *GraphConfig, stationName string, silenceDurationMs int64) error {
	if !IsConfigured(cfg) {
		return nil // Silently skip if not configured
	}

	subject := "[OK] Audio Recovered - " + stationName
	body := fmt.Sprintf(
		"Audio recovered on the encoder.\n\n"+
			"Silence lasted: %.1f seconds\n"+
			"Time:           %s",
		float64(silenceDurationMs)/1000.0, util.HumanTime(),
	)

	return sendEmail(cfg, subject, body)
}

// SendTestEmail sends a test email to verify Microsoft Graph configuration.
// This function first validates authentication before sending the test email.
func SendTestEmail(cfg *GraphConfig, stationName string) error {
	if err := ValidateConfig(cfg); err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	client, err := NewGraphClient(cfg)
	if err != nil {
		return fmt.Errorf("create Graph client: %w", err)
	}

	// Validate authentication first
	if err := client.ValidateAuth(); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	subject := "[TEST] " + stationName
	body := fmt.Sprintf(
		"Test email from the audio encoder.\n\n"+
			"Time: %s\n\n"+
			"Microsoft Graph configuration is working correctly.",
		util.HumanTime(),
	)

	recipients := ParseRecipients(cfg.Recipients)
	if err := client.SendMail(recipients, subject, body); err != nil {
		return fmt.Errorf("send email: %w", err)
	}

	return nil
}

// sendEmail delivers an email message to configured recipients via Microsoft Graph.
func sendEmail(cfg *GraphConfig, subject, body string) error {
	client, err := NewGraphClient(cfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}

	recipients := ParseRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid recipients")
	}

	if err := client.SendMail(recipients, subject, body); err != nil {
		return util.WrapError("send email via Graph", err)
	}

	return nil
}
