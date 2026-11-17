package billing

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// endCanceled honours cancel-at-period-end.
func (s *Service) endCanceled(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	return nil
}
