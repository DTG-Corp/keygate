package billing

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// convertTrial turns a live trial into its first paid order.
func (s *Service) convertTrial(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	return nil
}
