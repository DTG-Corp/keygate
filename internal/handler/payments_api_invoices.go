package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/invoice"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// ConsumerInvoiceHandler serves the merchant API's invoice read
// (spec §6.2): GET /api/v1/payments/invoices/{id} as JSON, or as the PDF
// with ?format=pdf. It is scoped by the API key's product binding, like
// every other consumer payments route.
type ConsumerInvoiceHandler struct {
	Invoices *invoice.Service
	// Store answers the invoice-with-order read the product check needs;
	// the invoice service itself never exposes the order.
	Store *store.Store
}

// RegisterConsumerInvoiceRoutes mounts the route on g, which the server
// passes as the payRead group (/api/v1/payments, payments:read).
func RegisterConsumerInvoiceRoutes(g *gin.RouterGroup, h *ConsumerInvoiceHandler) {
	g.GET("/invoices/:id", h.Get)
}

// consumerInvoiceView is the merchant-facing projection. It leaves out
// the operator fields — the e-invoice reference, the storage path, the
// audit dates of the queue — and keeps what a buyer's backend needs to
// show or store the document.
type consumerInvoiceView struct {
	ID               string              `json:"id"`
	Number           string              `json:"number"`
	Status           string              `json:"status"`
	Buyer            any                 `json:"buyer"`
	Items            []model.InvoiceItem `json:"items"`
	SubtotalMinor    int64               `json:"subtotal_minor"`
	DiscountMinor    int64               `json:"discount_minor"`
	TaxMinor         int64               `json:"tax_minor"`
	TotalMinor       int64               `json:"total_minor"`
	Currency         string              `json:"currency"`
	CurrencyExponent int16               `json:"currency_exponent"`
	PDFURL           string              `json:"pdf_url"`
	IssuedAt         any                 `json:"issued_at"`
	CreatedAt        any                 `json:"created_at"`
}

func consumerInvoiceViewOf(inv *model.Invoice) consumerInvoiceView {
	items := inv.Items
	if items == nil {
		items = []model.InvoiceItem{}
	}
	return consumerInvoiceView{
		ID: inv.ID, Number: inv.Number, Status: inv.Status, Buyer: inv.Buyer, Items: items,
		SubtotalMinor: inv.SubtotalMinor, DiscountMinor: inv.DiscountMinor,
		TaxMinor: inv.TaxMinor, TotalMinor: inv.TotalMinor,
		Currency: inv.Currency, CurrencyExponent: inv.CurrencyExp,
		PDFURL:   "/api/v1/payments/invoices/" + inv.ID + "?format=pdf",
		IssuedAt: inv.IssuedAt, CreatedAt: inv.CreatedAt,
	}
}

// Get answers the invoice of one order. A key bound to another product
// gets the same 404 as an unknown id: whether an invoice exists is not
// something a merchant should be able to probe across products.
func (h *ConsumerInvoiceHandler) Get(c *gin.Context) {
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	switch format {
	case "", "json", "pdf":
	default:
		response.BadRequest(c, "format must be json or pdf")
		return
	}

	inv, order, err := h.Store.GetInvoiceWithOrder(c, c.Param("id"))
	if err != nil {
		writeInvoiceError(c, err)
		return
	}
	if key := apiKeyFrom(c); key != nil && key.ProductID != "" && key.ProductID != order.ProductID {
		response.Err(c, http.StatusNotFound, "INVOICE_NOT_FOUND", "invoice not found")
		return
	}

	c.Header("Cache-Control", "no-store")
	if format == "pdf" {
		body, pdfInv, err := h.Invoices.PDF(c, inv.ID)
		if err != nil {
			writeInvoiceError(c, err)
			return
		}
		c.Header("Content-Disposition", `inline; filename="`+pdfInv.Number+`.pdf"`)
		c.Data(http.StatusOK, "application/pdf", body)
		return
	}
	response.OK(c, gin.H{"invoice": consumerInvoiceViewOf(inv)})
}
