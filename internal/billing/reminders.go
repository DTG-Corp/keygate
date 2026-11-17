package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
)

// sendReminders mails the pre-expiry ladder: seven days out, three days
// out, one day out. Only the most urgent rung reached at a pass fires, so
// a worker that was down for a day sends the notice that matters instead
// of a backlog.
func (s *Service) sendReminders(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	if s.deps.Email == nil || sub == nil || sub.CurrentPeriodEnd == nil {
		return nil
	}
	// A suspended or revoked licence has its own mail; a trialing one
	// gets the conversion order, not a renewal reminder for a period it
	// does not pay for yet.
	if lic == nil || (lic.Status != model.StatusActive && lic.Status != model.StatusPastDue) {
		return nil
	}
	end := sub.CurrentPeriodEnd.UTC()
	rung := reminderRung(ceilClock(now, end))
	if rung == 0 {
		return nil
	}
	// The scan may be an hour old. Re-read before mailing: a payment
	// that landed in between moved the period, and the customer must
	// not be told their subscription renews when it already did.
	fresh, err := s.deps.Store.BillingSubscriptionOrNil(ctx, lic.ID)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.CurrentPeriodEnd == nil || !fresh.CurrentPeriodEnd.Equal(end) ||
		(fresh.Status != model.StatusActive && fresh.Status != model.StatusPastDue) {
		return nil
	}
	// The mail tells the customer to pay, so there has to be something
	// to pay: an unpaid renewal order covering this period. An unpriced
	// plan is an operator's missing row, not a reason to mail a dead
	// link.
	order, err := s.deps.Store.FindOpenRenewalOrderForPeriod(ctx, lic.ID, end)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if order.Status != domain.OrderPending {
		return nil
	}
	daysLeft := ceilClock(now, end)
	tag := fmt.Sprintf("renewal_reminder_%d:%s", rung, end.Format(time.RFC3339))
	return s.queueMail(ctx, lic, "renewal_reminder", tag, func() (string, string) {
		return s.deps.Email.RenderRenewalNotice(productName(lic), planName(sub.Plan), daysLeft, s.portalURL(), end)
	})
}

// reminderRung maps the days left to the rung that covers them: one day
// or less is the last notice, then three, then seven. Zero means the
// period is still too far out for any rung.
func reminderRung(daysLeft int) int {
	switch {
	case daysLeft <= reminderRungOne:
		return 1
	case daysLeft <= reminderRungMid:
		return 3
	case daysLeft <= reminderRungMax:
		return 7
	}
	return 0
}

// queueMail claims the (licence, tag) lease, renders the mail and queues
// it with the claim closed in one transaction. The claim is the dedupe:
// two replicas reaching the same rung, or a restart in the same hour,
// cannot queue it twice.
// MailGate is the dashboard's on/off switch for an automated mail kind.
// service.EmailService implements it; an implementation that does not is
// treated as "enabled", so a test double need not carry the whole
// notification settings surface.
type MailGate interface {
	NotifyEnabled(kind string) bool
}

// mailAllowed reports whether the install still sends this kind of mail.
func (s *Service) mailAllowed(kind string) bool {
	if s.deps.Email == nil {
		return false
	}
	if gate, ok := s.deps.Email.(MailGate); ok {
		return gate.NotifyEnabled(kind)
	}
	return true
}

func (s *Service) queueMail(ctx context.Context, lic *model.License, kind, tag string, render func() (subject, body string)) error {
	if lic == nil || lic.Email == "" {
		return nil
	}
	if !s.mailAllowed(kind) {
		return nil
	}
	token, err := s.deps.Store.ClaimNotification(ctx, lic.ID, tag)
	if err != nil {
		return fmt.Errorf("claim %s: %w", tag, err)
	}
	if token == "" {
		return nil
	}
	subject, body := render()
	if err := s.deps.Store.EnqueueEmailAndCloseNotification(ctx, lic.Email, subject, body, token); err != nil {
		if errors.Is(err, store.ErrNotificationClaimLost) {
			return nil
		}
		if relErr := s.deps.Store.ReleaseNotification(ctx, token); relErr != nil {
			s.logger.Error("release mail claim failed", "tag", tag, "error", relErr)
		}
		return fmt.Errorf("queue %s: %w", tag, err)
	}
	return nil
}
