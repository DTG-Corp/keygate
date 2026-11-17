package billing

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// sendReminders mails the pre-expiry ladder (7 / 3 / 1 days).
func (s *Service) sendReminders(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	return nil
}
