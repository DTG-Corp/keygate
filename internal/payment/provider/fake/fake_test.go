package fake_test

import (
	"context"
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
)

func TestFakeProviderRoundTrip(t *testing.T) {
	f := fake.New("sepay")
	f.CreateFn = func(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{ProviderRef: "ref-1", QRImageURL: "https://qr.example/" + req.OrderCode}, nil
	}
	got, err := f.CreateAttempt(context.Background(), provider.CreateAttemptRequest{OrderCode: domain.NewOrderCode()})
	if err != nil || got.ProviderRef != "ref-1" {
		t.Fatalf("CreateAttempt = %+v, %v", got, err)
	}
	if f.Name() != "sepay" || len(f.Calls) != 1 {
		t.Fatalf("name=%q calls=%d", f.Name(), len(f.Calls))
	}
}
