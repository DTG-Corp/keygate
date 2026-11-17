package billing

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// dun runs the post-expiry ladder.
func (s *Service) dun(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	return nil
}

// EndGraceExpiredLicense is the billing engine's say in the generic
// grace sweep.
func (s *Service) EndGraceExpiredLicense(ctx context.Context, lic *model.License, cutoff time.Time) (bool, error) {
	return false, nil
}
