package sepay_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
)

func TestBuildQRImageURL(t *testing.T) {
	raw := sepay.BuildQRImageURL("0978482700", "MB", 50000, "KG7F3K2Q")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "vietqr.app" || u.Path != "/img" {
		t.Fatalf("url = %s", raw)
	}
	q := u.Query()
	if q.Get("acc") != "0978482700" || q.Get("bank") != "MB" || q.Get("amount") != "50000" {
		t.Fatalf("query = %v", q)
	}
	if q.Get("des") != "KG7F3K2Q" || q.Get("template") != "compact" {
		t.Fatalf("query = %v", q)
	}
	// A description with spaces/& must be escaped, not break the URL.
	raw = sepay.BuildQRImageURL("1", "MB", 1000, "KG7F3K2Q chuyen tien")
	u, err = url.Parse(raw)
	if err != nil || u.Query().Get("des") != "KG7F3K2Q chuyen tien" {
		t.Fatalf("escaped url = %s (%v)", raw, err)
	}
}

func TestProviderCreateAttemptRejectsMissingBankConfig(t *testing.T) {
	full := provider.BankAccount{BankName: "MB Bank", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"}
	cases := []struct {
		name string
		cfg  sepay.Config
	}{
		{"missing account number", sepay.Config{Bank: provider.BankAccount{BankName: "MB Bank"}, BankCode: "MB"}},
		{"missing bank code", sepay.Config{Bank: full}},
		{"blank account number", sepay.Config{Bank: provider.BankAccount{BankName: "MB Bank", AccountNo: "   "}, BankCode: "MB"}},
		{"blank bank code", sepay.Config{Bank: full, BankCode: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := sepay.New(tc.cfg).CreateAttempt(context.Background(), provider.CreateAttemptRequest{
				OrderCode: "KG7F3K2Q", AmountMinor: 50000, Currency: "VND",
			})
			if !errors.Is(err, sepay.ErrMissingBankConfig) {
				t.Fatalf("err = %v, want ErrMissingBankConfig", err)
			}
			if res.QRImageURL != "" || res.ProviderRef != "" {
				t.Fatalf("unusable result returned: %+v", res)
			}
		})
	}
}

func TestProviderCreateAttemptBuildsStaticQR(t *testing.T) {
	p := sepay.New(sepay.Config{
		Bank:     provider.BankAccount{BankName: "MB Bank", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		BankCode: "MB",
	})
	if p.Name() != "sepay" {
		t.Fatalf("name = %q", p.Name())
	}
	expires := time.Now().Add(15 * time.Minute)
	res, err := p.CreateAttempt(context.Background(), provider.CreateAttemptRequest{
		OrderCode:   "KG7F3K2Q",
		AmountMinor: 1190000,
		Currency:    "VND",
		Description: "this memo must be replaced by the order code",
		ExpiresAt:   expires,
	})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if res.ProviderRef != "KG7F3K2Q" || res.Description != "KG7F3K2Q" {
		t.Fatalf("ref/description = %q/%q", res.ProviderRef, res.Description)
	}
	if res.Bank.AccountNo != "0978482700" || res.Bank.BankName != "MB Bank" || res.Bank.AccountName != "NGUYEN VAN A" {
		t.Fatalf("bank = %+v", res.Bank)
	}
	if !res.ExpiresAt.Equal(expires) {
		t.Fatalf("expires = %v, want %v", res.ExpiresAt, expires)
	}
	if res.QRPayload != res.QRImageURL {
		t.Fatalf("qr payload %q != image url %q", res.QRPayload, res.QRImageURL)
	}
	u, err := url.Parse(res.QRImageURL)
	if err != nil {
		t.Fatalf("parse qr url: %v", err)
	}
	q := u.Query()
	if u.Host != "vietqr.app" || q.Get("acc") != "0978482700" || q.Get("bank") != "MB" ||
		q.Get("amount") != "1190000" || q.Get("des") != "KG7F3K2Q" || q.Get("template") != "compact" {
		t.Fatalf("qr url = %s", res.QRImageURL)
	}
	if res.CheckoutURL != "" || res.VANumber != "" {
		t.Fatalf("unexpected gateway fields = %+v", res)
	}
	// A bank transfer cannot be cancelled, but the method must not error.
	if err := p.CancelAttempt(context.Background(), res.ProviderRef); err != nil {
		t.Fatalf("cancel attempt: %v", err)
	}
}
