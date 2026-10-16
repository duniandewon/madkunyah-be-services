package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

const (
	ChannelWhatsApp = "whatsapp"
	ChannelEmail    = "email"
)

// Notifier delivers a code over one channel.
type Notifier interface {
	SendOTP(ctx context.Context, channel string, to domain.Identifier, code string) error
}

type quotaRefunder interface {
	RefundOTPQuota(ctx context.Context, identifier, date string) error
}

// DispatchJob is one code delivery. Fallback is an email to try if WhatsApp
// fails permanently; QuotaDay identifies the quota slot to refund.
type DispatchJob struct {
	To       domain.Identifier
	Code     string
	Fallback string
	QuotaDay string
}

// Dispatcher sends codes in the background so the HTTP response never waits
// on, or reveals, delivery.
type Dispatcher struct {
	notifier    Notifier
	quota       quotaRefunder
	retryWindow time.Duration
	wg          sync.WaitGroup
}

func NewDispatcher(n Notifier, q quotaRefunder, retryWindow time.Duration) *Dispatcher {
	return &Dispatcher{notifier: n, quota: q, retryWindow: retryWindow}
}

func (d *Dispatcher) Dispatch(job *DispatchJob) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.run(job)
	}()
}

// Wait blocks until in-flight deliveries finish, for graceful shutdown.
func (d *Dispatcher) Wait() { d.wg.Wait() }

func (d *Dispatcher) run(job *DispatchJob) {
	ctx := context.Background()
	channel := ChannelWhatsApp
	if job.To.Kind == domain.IdentifierEmail {
		channel = ChannelEmail
	}
	if d.sendWithRetry(ctx, channel, job.To, job.Code) {
		return
	}
	if job.Fallback != "" {
		email := domain.Identifier{Kind: domain.IdentifierEmail, Value: job.Fallback}
		if d.sendWithRetry(ctx, ChannelEmail, email, job.Code) {
			return
		}
	}
	// Permanent failure does not count against the daily quota; the cooldown
	// still applies.
	slog.WarnContext(ctx, "otp dispatch failed permanently", "to", job.To.Masked())
	if err := d.quota.RefundOTPQuota(ctx, job.To.Value, job.QuotaDay); err != nil {
		slog.ErrorContext(ctx, "refund otp quota", "error", err)
	}
}

// sendWithRetry retries with exponential backoff until the retry window,
// which sits well inside the OTP TTL, runs out.
func (d *Dispatcher) sendWithRetry(ctx context.Context, channel string, to domain.Identifier, code string) bool {
	ctx, cancel := context.WithTimeout(ctx, d.retryWindow)
	defer cancel()

	backoff := time.Second
	for {
		err := d.notifier.SendOTP(ctx, channel, to, code)
		if err == nil {
			return true
		}
		slog.WarnContext(ctx, "otp send failed", "channel", channel, "to", to.Masked(), "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}
