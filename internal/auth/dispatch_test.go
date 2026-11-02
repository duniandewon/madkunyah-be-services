package auth

import (
	"context"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

type panicNotifier struct{}

func (panicNotifier) SendOTP(context.Context, string, domain.Identifier, string) error {
	panic("notifier blew up")
}

func TestDispatchSurvivesNotifierPanic(t *testing.T) {
	d := NewDispatcher(panicNotifier{}, nil, time.Second)
	d.Dispatch(&DispatchJob{To: domain.Identifier{Kind: domain.IdentifierEmail, Value: "a@example.com"}, Code: "123456"})
	d.Wait()
}
