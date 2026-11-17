package billing_test

import (
	"github.com/dtg-corp/dsoft/internal/billing"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/service"
)

// The wiring the Lead adds to cmd/server/main.go only compiles if the
// real services satisfy the engine's seams. Asserting it here keeps the
// two packages free of an import cycle (checkout already imports service)
// and turns a wiring mistake into a test failure instead of a build
// failure on main.
var (
	_ billing.OrderCreator   = (*checkout.Service)(nil)
	_ billing.ReminderSender = (*service.EmailService)(nil)
	_ billing.EventEmitter   = (*service.WebhookService)(nil)
)
