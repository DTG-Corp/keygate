package handler

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPlanPriceAdminUpsertAndList(t *testing.T) {
	f := newAPIFixture(t)
	h := &PlanPriceAdminHandler{Store: f.s}

	code, out := f.call(h.Upsert, "PUT", "/api/v1/admin/plans/"+f.planA.ID+"/prices",
		`{"provider":"sepay","currency":"VND","amount_minor":1190000}`, nil, gin.Param{Key: "id", Value: f.planA.ID})
	if code != 200 {
		t.Fatalf("upsert status %d: %v", code, out)
	}
	code, out = f.call(h.List, "GET", "/api/v1/admin/plans/"+f.planA.ID+"/prices", "", nil,
		gin.Param{Key: "id", Value: f.planA.ID})
	if code != 200 {
		t.Fatalf("list status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	rows, _ := data["prices"].([]any)
	if len(rows) != 1 {
		t.Fatalf("prices = %v", data["prices"])
	}
	// Missing plan is a 404, not a silent create.
	code, out = f.call(h.Upsert, "PUT", "/api/v1/admin/plans/does-not-exist/prices",
		`{"provider":"sepay","currency":"VND","amount_minor":1}`, nil, gin.Param{Key: "id", Value: "does-not-exist"})
	if code != 404 || errCode(out) != "PLAN_NOT_FOUND" {
		t.Fatalf("missing plan: %d %v", code, out)
	}
}
