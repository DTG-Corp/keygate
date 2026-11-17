package fake_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
)

func TestFakeProviderRoundTrip(t *testing.T) {
	f := fake.New("sepay")
	f.CreateFn = func(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{ProviderRef: "ref-1"}, nil
	}
	req := provider.CreateAttemptRequest{
		OrderCode:   domain.NewOrderCode(),
		AmountMinor: 1_190_000,
		Currency:    "VND",
	}

	got, err := f.CreateAttempt(context.Background(), req)
	if err != nil || got.ProviderRef != "ref-1" {
		t.Fatalf("CreateAttempt = %+v, %v; want ProviderRef %q and nil error", got, err, "ref-1")
	}
	if got, want := f.Name(), "sepay"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(f.Calls))
	}
	if !reflect.DeepEqual(f.Calls[0], req) {
		t.Errorf("recorded call = %+v, want %+v", f.Calls[0], req)
	}
}
