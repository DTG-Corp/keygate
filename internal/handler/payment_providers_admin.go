package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/providercfg"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// PaymentProviderAdminHandler is the admin editor for the payment
// provider rows. Secrets are write-only: the response says whether one
// is stored, never what it is.
type PaymentProviderAdminHandler struct {
	Service *providercfg.Service
	// Store is optional and only used for the audit trail of the
	// write endpoints; a nil Store simply skips it.
	Store *store.Store
}

// providerView is the admin projection: secrets are never serialised,
// only whether one is set, so a stolen admin session cannot exfiltrate
// the merchant credentials.
type providerView struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	ProductID      string `json:"product_id,omitempty"`
	Mode           string `json:"mode"`
	Enabled        bool   `json:"enabled"`
	Config         any    `json:"config"`
	CredentialsSet bool   `json:"credentials_set"`
	SecretSet      bool   `json:"secret_set"`
}

func viewOf(r *model.PaymentProviderConfig) providerView {
	return providerView{
		ID: r.ID, Provider: r.Provider, ProductID: r.ProductID, Mode: r.Mode, Enabled: r.Enabled,
		Config:         r.Config,
		CredentialsSet: len(r.CredentialsEncrypted) > 0,
		SecretSet:      len(r.WebhookSecretEncrypted) > 0,
	}
}

// providerError maps the service's sentinels to the codes an admin form
// branches on. Anything else is an operator problem and stays a 500
// with no database text in the body.
func providerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, providercfg.ErrProviderUnknown):
		response.Err(c, http.StatusBadRequest, "PAYMENT_PROVIDER_UNKNOWN", "unknown payment provider")
	case errors.Is(err, providercfg.ErrProviderConfigInvalid):
		response.Err(c, http.StatusBadRequest, "PAYMENT_PROVIDER_CONFIG_INVALID", "invalid payment provider configuration")
	case errors.Is(err, providercfg.ErrProviderNotFound):
		response.Err(c, http.StatusNotFound, "PAYMENT_PROVIDER_NOT_FOUND", "payment provider config not found")
	default:
		response.Internal(c, err)
	}
}

func (h *PaymentProviderAdminHandler) List(c *gin.Context) {
	rows, err := h.Service.List(c)
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]providerView, 0, len(rows))
	for _, r := range rows {
		out = append(out, viewOf(r))
	}
	response.OK(c, gin.H{"providers": out})
}

type providerUpsertRequest struct {
	Provider  string `json:"provider"`
	ProductID string `json:"product_id"`
	Mode      string `json:"mode"`
	// Enabled nil keeps the stored value on update; a new row defaults
	// to false so a half-filled form cannot start taking money.
	Enabled       *bool  `json:"enabled"`
	Config        any    `json:"config"`
	Credentials   any    `json:"credentials"`
	WebhookSecret string `json:"webhook_secret"`
}

func (h *PaymentProviderAdminHandler) Upsert(c *gin.Context) {
	var req providerUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}

	var existing *model.PaymentProviderConfig
	enabled := false
	if req.Enabled != nil {
		enabled = *req.Enabled
	} else {
		// An omitted flag is "leave it alone", which needs the current
		// row; the lookup is scoped to this exact slot and never reads
		// the install default.
		var err error
		existing, err = h.Service.Stored(c, req.Provider, req.ProductID, req.Mode)
		if err != nil {
			providerError(c, err)
			return
		}
		if existing != nil {
			enabled = existing.Enabled
		}
	}

	row, err := h.Service.Upsert(c, providercfg.UpsertInput{
		Provider: req.Provider, ProductID: req.ProductID, Mode: req.Mode, Enabled: enabled,
		Config: req.Config, Credentials: req.Credentials, WebhookSecret: req.WebhookSecret,
	})
	if err != nil {
		providerError(c, err)
		return
	}
	// The audit trail names the fields the save actually wrote, not a
	// blanket "config": a save that only flipped `enabled` must not look
	// like it rewrote the bank details.
	fields := []string{"mode"}
	if req.Enabled != nil || existing == nil {
		fields = append(fields, "enabled")
	}
	if req.Config != nil {
		fields = append(fields, "config")
	}
	if req.Credentials != nil {
		fields = append(fields, "credentials")
	}
	if req.WebhookSecret != "" {
		fields = append(fields, "secret")
	}
	h.audit(c, &model.AuditLog{
		Entity: "payment_provider_config", EntityID: row.ID, Action: "upserted",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		ProductID: row.ProductID,
		Changes: map[string]any{
			"provider": row.Provider, "product_id": row.ProductID,
			"enabled": row.Enabled, "fields": fields,
			"credentials_set": len(row.CredentialsEncrypted) > 0,
			"secret_set":      len(row.WebhookSecretEncrypted) > 0,
		},
	})
	response.OK(c, viewOf(row))
}

// DeleteSecrets blanks chosen secret fields. It is an explicit call
// because a blank secret in Upsert means "keep the current one".
func (h *PaymentProviderAdminHandler) DeleteSecrets(c *gin.Context) {
	var req struct {
		Fields []string `json:"fields"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	for _, f := range req.Fields {
		if f != "credentials" && f != "webhook_secret" {
			response.BadRequest(c, "unknown secret field "+f)
			return
		}
	}
	if len(req.Fields) == 0 {
		response.BadRequest(c, "fields is required")
		return
	}
	id := c.Param("id")
	if err := h.Service.DeleteSecrets(c, id, req.Fields); err != nil {
		providerError(c, err)
		return
	}
	// Only a row that was actually updated is audited: a 404 above
	// never reaches this entry.
	h.audit(c, &model.AuditLog{
		Entity: "payment_provider_config", EntityID: id, Action: "secrets_deleted",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"fields": req.Fields},
	})
	response.NoContent(c)
}

func (h *PaymentProviderAdminHandler) audit(c *gin.Context, entry *model.AuditLog) {
	if h.Store == nil {
		return
	}
	h.Store.Audit(c, entry)
}
