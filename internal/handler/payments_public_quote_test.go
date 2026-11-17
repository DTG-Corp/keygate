package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// TestPublicQuotePricesWithoutCreatingAnything pins the contract of the
// dry-run endpoint: it answers the total the checkout would charge, and
// the quote leaves no order behind.
func TestPublicQuotePricesWithoutCreatingAnything(t *testing.T) {
	f := newPaymentsFixture(t)
	amount := int64(99_000)
	f.price(t, f.planA, "payos", amount)

	r := gin.New()
	RegisterPublicQuoteRoutes(r, f.pub, 1000)

	body := `{"plan_id":"` + f.planA.ID + `","quantity":1,"email":"quote@example.com"}`
	req := httptest.NewRequest(http.MethodPost, PublicQuoteRoute, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			SubtotalMinor int64  `json:"subtotal_minor"`
			TotalMinor    int64  `json:"total_minor"`
			Currency      string `json:"currency"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.SubtotalMinor != amount || env.Data.TotalMinor != amount {
		t.Fatalf("quote = %+v, want subtotal/total %d", env.Data, amount)
	}
	if env.Data.Currency != "VND" {
		t.Fatalf("currency = %q, want VND", env.Data.Currency)
	}
	n, err := f.s.DB.NewSelect().Model((*model.PaymentOrder)(nil)).
		Where("plan_id = ?", f.planA.ID).Count(f.ctx)
	if err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if n != 0 {
		t.Fatalf("orders = %d, want 0: a quote creates nothing", n)
	}
}

func TestPublicQuoteRejectsUnknownPlan(t *testing.T) {
	f := newPaymentsFixture(t)
	r := gin.New()
	RegisterPublicQuoteRoutes(r, f.pub, 1000)
	req := httptest.NewRequest(http.MethodPost, PublicQuoteRoute,
		strings.NewReader(`{"plan_id":"does-not-exist"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PAYMENT_PRICE_NOT_CONFIGURED") {
		t.Fatalf("body missing code: %s", w.Body.String())
	}
}
