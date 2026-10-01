package httpapi

import (
	"context"
	"log/slog"
)

// Mailer is intentionally small so a production provider can replace the
// development log mailer without changing password-reset behavior.
type Mailer interface {
	SendPasswordReset(ctx context.Context, to, link string) error
}

type LogMailer struct{}

func (LogMailer) SendPasswordReset(_ context.Context, to, link string) error {
	slog.Info("development_password_reset", "recipient", to, "link", link)
	return nil
}
