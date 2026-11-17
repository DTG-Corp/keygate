package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The SPA's api helper prefixes every request with /api/v1, while the
// spec's public status route is root-mounted. Both spellings must reach
// the same handler with the same headers and payload; otherwise the pay
// page polls a 404 forever.
func TestPublicStatusServedAtRootAndAPIAlias(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, "alias@example.com", "sepay", 1_190_000, true)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterPublicStatusRoutes(r, f.pub, 600)

	get := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		return w
	}
	root := get("/pay/" + order.Code + "/status")
	alias := get("/api/v1/pay/" + order.Code + "/status")

	for name, w := range map[string]*httptest.ResponseRecorder{"root": root, "api/v1 alias": alias} {
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d %s", name, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", name, got)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s: Content-Type = %q, want JSON", name, ct)
		}
	}
	// Same capability, same answer: the alias is not a second view.
	if root.Body.String() != alias.Body.String() {
		t.Fatalf("alias body differs from the root route:\nroot  %s\nalias %s",
			root.Body.String(), alias.Body.String())
	}

	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(alias.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode alias body %q: %v", alias.Body.String(), err)
	}
	// The T12 pay page reads these names; renaming one breaks it.
	for _, key := range []string{"status", "amount_minor", "currency", "expires_at", "qr_image_url", "bank"} {
		if _, ok := env.Data[key]; !ok {
			t.Errorf("alias payload missing %q: %v", key, env.Data)
		}
	}
	if env.Data["status"] != "pending" || env.Data["amount_minor"] != float64(1_190_000) {
		t.Fatalf("alias payload = %v", env.Data)
	}
}
