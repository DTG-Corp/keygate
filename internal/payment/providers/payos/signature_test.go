package payos_test

import (
	"encoding/json"
	"testing"

	"github.com/dtg-corp/dsoft/internal/payment/providers/payos"
)

// PayOS publishes the canonical form, not a Go helper, so these tests
// pin the digest of the documented string rather than recomputing it
// with the same code under test.
func TestSignCreateRequestPinned(t *testing.T) {
	// Vector computed from the documented canonical string
	// amount=…&cancelUrl=…&description=…&orderCode=…&returnUrl=… .
	req := payos.CreateRequest{
		OrderCode: 123, Amount: 1200000, Description: "KG7F3K2Q",
		CancelURL: "https://keygate.example.com/pay/KG7F3K2Q",
		ReturnURL: "https://keygate.example.com/pay/KG7F3K2Q",
	}
	got := payos.SignCreateRequest("checksum-key", req)
	want := "459f1d7f8ea39ccdd1927f95a478bdf304c7014bbbff1dac653388e2ff8c7c30"
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

// PayOS signs exactly five fields. Buyer details, line items and
// expiredAt travel in the body unsigned, so adding them must not change
// the signature — otherwise a retry with a filled-in buyer would be
// rejected by PayOS as a bad signature.
func TestSignCreateRequestIgnoresUnsignedFields(t *testing.T) {
	base := payos.CreateRequest{
		OrderCode: 123, Amount: 1200000, Description: "KG7F3K2Q",
		CancelURL: "https://keygate.example.com/pay/KG7F3K2Q",
		ReturnURL: "https://keygate.example.com/pay/KG7F3K2Q",
	}
	withExtras := base
	withExtras.BuyerName = "Nguyen Van A"
	withExtras.BuyerEmail = "buyer@example.com"
	withExtras.BuyerPhone = "+84900000000"
	withExtras.BuyerAddress = "1 Le Loi, District 1"
	withExtras.BuyerTaxID = "0312345678"
	withExtras.ExpiredAt = 1762900000
	withExtras.Items = []payos.Item{{Name: "Pro", Quantity: 1, Price: 1200000}}

	if got, want := payos.SignCreateRequest("checksum-key", withExtras), payos.SignCreateRequest("checksum-key", base); got != want {
		t.Fatalf("unsigned fields changed the signature: %s != %s", got, want)
	}
}

func TestVerifyWebhookSignaturePinned(t *testing.T) {
	data := map[string]any{
		"orderCode": 123.0, "amount": 1200000.0, "description": "KG7F3K2Q",
		"accountNumber": "113366668888", "reference": "TF230204212323",
		"transactionDateTime": "2026-11-12 15:04:05", "currency": "VND",
		"paymentLinkId": "abc", "code": "00", "desc": "Thanh cong",
	}
	const want = "64f02615976df601952342f2aa8c1a669777c46d8cf792193cbb2683a6063d6b"
	if got := payos.SignWebhook("checksum-key", data); got != want {
		t.Fatalf("webhook signature = %s, want %s", got, want)
	}
	if !payos.VerifyDataSignature("checksum-key", data, want) {
		t.Fatal("pinned signature did not verify")
	}
	if payos.VerifyDataSignature("checksum-key", data, "deadbeef") {
		t.Fatal("bad signature verified")
	}
	if payos.VerifyDataSignature("other-key", data, want) {
		t.Fatal("signature verified under the wrong checksum key")
	}
}

// PayOS's signing libraries emit the hex digest behind a "2" version
// marker on some surfaces (create-link and webhook responses). Accept
// that form, and only that form: one extra leading byte.
func TestVerifyDataSignatureAcceptsVersionPrefix(t *testing.T) {
	data := map[string]any{"orderCode": 123.0, "amount": 1200000.0}
	sig := payos.SignWebhook("checksum-key", data)

	if !payos.VerifyDataSignature("checksum-key", data, "2"+sig) {
		t.Fatal("signature with PayOS version prefix did not verify")
	}
	for _, bad := range []string{"3" + sig, "22" + sig, "2" + sig[1:] + "00", "2"} {
		if payos.VerifyDataSignature("checksum-key", data, bad) {
			t.Fatalf("bad signature %q verified", bad)
		}
	}
}

func TestCanonicalDataNullsAndArrays(t *testing.T) {
	got := payos.CanonicalData(map[string]any{
		"b": nil, "a": "x", "c": []any{"1", "2"}, "d": map[string]any{"k": "v"},
	})
	want := `a=x&b=&c=["1","2"]&d={"k":"v"}`
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
}

// The webhook decoder keeps JSON numbers exact (json.Number) and the
// canonical form must reproduce them verbatim, or a signature over a
// large order code would differ from the one PayOS computed.
func TestCanonicalDataScalars(t *testing.T) {
	got := payos.CanonicalData(map[string]any{
		"int": 1200000, "int64": int64(1200000), "float": 12.5, "bool": true,
		"zero": 0, "number": json.Number("9007199254740993"),
	})
	want := `bool=true&float=12.5&int=1200000&int64=1200000&number=9007199254740993&zero=0`
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	if got := payos.CanonicalData(nil); got != "" {
		t.Fatalf("canonical(nil) = %q, want empty", got)
	}
}
