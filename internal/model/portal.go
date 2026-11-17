package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// CustomerBillingProfile is the buyer/tax block a customer saves in the
// portal and DSoft reuses at checkout (spec §3.8, §6.4). One row per
// user; an empty buyer object means "nothing saved".
type CustomerBillingProfile struct {
	bun.BaseModel `bun:"table:customer_billing_profiles"`

	UserID    string          `bun:",pk" json:"user_id"`
	Buyer     json.RawMessage `bun:",notnull,type:jsonb" json:"buyer"`
	CreatedAt time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}
