package sepay

import (
	"net/url"
	"strconv"
)

// BuildQRImageURL returns the public VietQR image for a static transfer.
// It is built locally and never calls SePay: the QR is just an image URL
// the buyer's banking app decodes, and the payment is confirmed by the
// bank webhook, so the rail works with no provider API credentials.
func BuildQRImageURL(account, bank string, amountMinor int64, description string) string {
	q := url.Values{}
	q.Set("acc", account)
	q.Set("bank", bank)
	q.Set("amount", strconv.FormatInt(amountMinor, 10))
	q.Set("des", description)
	q.Set("template", "compact")
	return "https://vietqr.app/img?" + q.Encode()
}
