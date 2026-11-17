package hooks_test

import (
	"context"
	"testing"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/hooks"
	"github.com/dtg-corp/dsoft/internal/payment/settlement"
)

type recorder struct{ calls *[]string }

func (r recorder) AfterPaid(context.Context, *model.PaymentOrder, *model.License, string) {
	*r.calls = append(*r.calls, "ok")
}

type panicker struct{ calls *[]string }

func (p panicker) AfterPaid(context.Context, *model.PaymentOrder, *model.License, string) {
	*p.calls = append(*p.calls, "panic")
	panic("boom")
}

var _ settlement.PaidHook = hooks.Chain(nil)

func TestChainRunsEveryHookInOrderAndSurvivesPanic(t *testing.T) {
	var calls []string
	chain := hooks.Chain{
		recorder{&calls},
		panicker{&calls},
		nil,
		recorder{&calls},
	}
	chain.AfterPaid(context.Background(), &model.PaymentOrder{Code: "KG-TEST"}, nil, "payos")
	want := []string{"ok", "panic", "ok"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestNilChainIsSafe(t *testing.T) {
	var c hooks.Chain
	c.AfterPaid(context.Background(), nil, nil, "")
}
