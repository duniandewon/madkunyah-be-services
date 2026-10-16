// Package notifier delivers one-time codes. Only a logging implementation
// exists today; WhatsApp and SMTP will satisfy the same method.
package notifier

import (
	"context"
	"log/slog"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// Log writes OTP deliveries to the structured log instead of sending them.
// The recipient is always masked. The code itself is logged only when
// exposeCode is set, which must be limited to local development.
type Log struct {
	exposeCode bool
}

func NewLog(exposeCode bool) *Log { return &Log{exposeCode: exposeCode} }

func (l *Log) SendOTP(ctx context.Context, channel string, to domain.Identifier, code string) error {
	attrs := []any{"channel", channel, "to", to.Masked()}
	if l.exposeCode {
		attrs = append(attrs, "code", code)
	}
	slog.InfoContext(ctx, "otp delivery (log notifier)", attrs...)
	return nil
}
