package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
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
	Provider      string `json:"provider"`
	ProductID     string `json:"product_id"`
	Mode          string `json:"mode"`
	Enabled       bool   `json:"enabled"`
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
	row, err := h.Service.Upsert(c, providercfg.UpsertInput{
		Provider: req.Provider, ProductID: req.ProductID, Mode: req.Mode, Enabled: req.Enabled,
		Config: req.Config, Credentials: req.Credentials, WebhookSecret: req.WebhookSecret,
	})
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	h.audit(c, &model.AuditLog{
		Entity: "payment_provider_config", EntityID: row.ID, Action: "upserted",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		ProductID: row.ProductID,
		Changes: map[string]any{
			"provider": row.Provider, "mode": row.Mode, "product_id": row.ProductID,
			"enabled": row.Enabled, "config": "written",
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
		response.Internal(c, err)
		return
	}
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
