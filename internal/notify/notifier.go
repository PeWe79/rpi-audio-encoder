package notify

import (
	"fmt"
	"sync"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// SilenceNotifier manages notifications for silence detection events.
type SilenceNotifier struct {
	cfg *config.Config

	// mu protects the notification state fields below
	mu sync.Mutex

	// Track which notifications have been sent for current silence period
	webhookSent bool
	emailSent   bool
	logSent     bool

	// Cached Graph client for email notifications
	graphClient *GraphClient
}

// NewSilenceNotifier returns a SilenceNotifier configured with the given config.
func NewSilenceNotifier(cfg *config.Config) *SilenceNotifier {
	return &SilenceNotifier{cfg: cfg}
}

// InvalidateGraphClient clears the cached Graph client.
// Call this when Graph configuration changes.
func (n *SilenceNotifier) InvalidateGraphClient() {
	n.mu.Lock()
	n.graphClient = nil
	n.mu.Unlock()
}

// getOrCreateGraphClient returns the cached Graph client, creating it if needed.
func (n *SilenceNotifier) getOrCreateGraphClient(cfg *GraphConfig) (*GraphClient, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.graphClient != nil {
		return n.graphClient, nil
	}

	client, err := NewGraphClient(cfg)
	if err != nil {
		return nil, err
	}
	n.graphClient = client
	return client, nil
}

// HandleEvent processes a silence event and triggers notifications.
func (n *SilenceNotifier) HandleEvent(event audio.SilenceEvent) {
	if event.JustEntered {
		n.handleSilenceStart(event.DurationMs)
	}

	if event.JustRecovered {
		n.handleSilenceEnd(event.TotalDurationMs)
	}
}

// handleSilenceStart triggers notifications when silence is first detected.
func (n *SilenceNotifier) handleSilenceStart(durationMs int64) {
	cfg := n.cfg.Snapshot()

	n.trySend(&n.webhookSent, cfg.HasWebhook(), func() { n.sendSilenceWebhook(cfg, durationMs) })
	n.trySend(&n.emailSent, cfg.HasGraph(), func() { n.sendSilenceEmail(cfg, durationMs) })
	n.trySend(&n.logSent, cfg.HasLogPath(), func() { n.logSilenceStart(cfg) })
}

// trySend sends a notification if the condition is met and not already sent.
func (n *SilenceNotifier) trySend(sent *bool, condition bool, sender func()) {
	n.mu.Lock()
	shouldSend := !*sent && condition
	if shouldSend {
		*sent = true
	}
	n.mu.Unlock()
	if shouldSend {
		go sender()
	}
}

// handleSilenceEnd triggers recovery notifications when silence ends.
func (n *SilenceNotifier) handleSilenceEnd(totalDurationMs int64) {
	cfg := n.cfg.Snapshot()

	// Only send recovery notifications if we sent the corresponding start notification
	n.mu.Lock()
	shouldSendWebhookRecovery := n.webhookSent
	shouldSendEmailRecovery := n.emailSent
	shouldSendLogRecovery := n.logSent
	// Reset notification state for next silence period
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.mu.Unlock()

	if shouldSendWebhookRecovery {
		go n.sendRecoveryWebhook(cfg, totalDurationMs)
	}

	if shouldSendEmailRecovery {
		go n.sendRecoveryEmail(cfg, totalDurationMs)
	}

	if shouldSendLogRecovery {
		go n.logSilenceEnd(cfg, totalDurationMs)
	}
}

// Reset clears the notification state.
func (n *SilenceNotifier) Reset() {
	n.mu.Lock()
	n.webhookSent = false
	n.emailSent = false
	n.logSent = false
	n.mu.Unlock()
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceWebhook(cfg config.Snapshot, durationMs int64) {
	util.LogNotifyResult(
		func() error { return SendSilenceWebhook(cfg.WebhookURL, durationMs, cfg.SilenceThreshold) },
		"Silence webhook",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryWebhook(cfg config.Snapshot, durationMs int64) {
	util.LogNotifyResult(
		func() error { return SendRecoveryWebhook(cfg.WebhookURL, durationMs) },
		"Recovery webhook",
	)
}

// BuildGraphConfig creates a GraphConfig from the config snapshot.
//
//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func BuildGraphConfig(cfg config.Snapshot) *GraphConfig {
	return &GraphConfig{
		TenantID:     cfg.GraphTenantID,
		ClientID:     cfg.GraphClientID,
		ClientSecret: cfg.GraphClientSecret,
		FromAddress:  cfg.GraphFromAddress,
		Recipients:   cfg.GraphRecipients,
	}
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendSilenceEmail(cfg config.Snapshot, durationMs int64) {
	graphCfg := BuildGraphConfig(cfg)
	util.LogNotifyResult(
		func() error {
			return n.sendEmailWithClient(graphCfg, cfg.StationName, durationMs, cfg.SilenceThreshold, true)
		},
		"Silence email",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) sendRecoveryEmail(cfg config.Snapshot, durationMs int64) {
	graphCfg := BuildGraphConfig(cfg)
	util.LogNotifyResult(
		func() error { return n.sendEmailWithClient(graphCfg, cfg.StationName, durationMs, 0, false) },
		"Recovery email",
	)
}

// sendEmailWithClient sends an email using the cached Graph client.
func (n *SilenceNotifier) sendEmailWithClient(cfg *GraphConfig, stationName string, durationMs int64, threshold float64, isSilence bool) error {
	if !IsConfigured(cfg) {
		return nil
	}

	client, err := n.getOrCreateGraphClient(cfg)
	if err != nil {
		return util.WrapError("create Graph client", err)
	}

	var subject, body string
	if isSilence {
		subject = "[ALERT] Silence Detected - " + stationName
		body = fmt.Sprintf(
			"Silence detected on the audio encoder.\n\n"+
				"Duration:  %.1f seconds\n"+
				"Threshold: %.1f dB\n"+
				"Time:      %s\n\n"+
				"Please check the audio source.",
			float64(durationMs)/1000.0, threshold, util.HumanTime(),
		)
	} else {
		subject = "[OK] Audio Recovered - " + stationName
		body = fmt.Sprintf(
			"Audio recovered on the encoder.\n\n"+
				"Silence lasted: %.1f seconds\n"+
				"Time:           %s",
			float64(durationMs)/1000.0, util.HumanTime(),
		)
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

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceStart(cfg config.Snapshot) {
	util.LogNotifyResult(
		func() error { return LogSilenceStart(cfg.LogPath, cfg.SilenceThreshold) },
		"Silence log",
	)
}

//nolint:gocritic // hugeParam: copy is acceptable for infrequent notification events
func (n *SilenceNotifier) logSilenceEnd(cfg config.Snapshot, durationMs int64) {
	util.LogNotifyResult(
		func() error { return LogSilenceEnd(cfg.LogPath, durationMs, cfg.SilenceThreshold) },
		"Recovery log",
	)
}
