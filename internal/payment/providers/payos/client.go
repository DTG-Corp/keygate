package payos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is PayOS's production merchant API. PayOS has no
// sandbox: a test config row uses the same host with a real, small
// amount, so BaseURL is only overridden by tests.
const defaultBaseURL = "https://api-merchant.payos.vn"

// maxResponseBytes bounds how much of a response is buffered. Payment
// links are small; anything larger is a misbehaving peer, not a payload
// the adapter should try to parse.
const maxResponseBytes = 1 << 20

// Item is one line item on the payment link. PayOS only accepts the
// documented taxPercentage values (-2, -1, 0, 5, 10); the caller maps
// the order's tax rate, this type does not round it.
type Item struct {
	Name          string `json:"name"`
	Quantity      int    `json:"quantity"`
	Price         int64  `json:"price"`
	TaxPercentage int    `json:"taxPercentage,omitempty"`
}

// CreateRequest is one create-link call. OrderCode is the order's
// numeric_code (PayOS requires int64, the KG code goes in Description);
// ExpiredAt is unix seconds and must be <= 2038-01-19.
type CreateRequest struct {
	OrderCode    int64
	Amount       int64
	Description  string
	BuyerName    string
	BuyerEmail   string
	BuyerPhone   string
	BuyerAddress string
	BuyerTaxID   string
	Items        []Item
	CancelURL    string
	ReturnURL    string
	ExpiredAt    int64
}

// PaymentLink is the part of PayOS's payment-request payload the
// checkout and reconciler use.
type PaymentLink struct {
	CheckoutURL   string
	QRCode        string
	PaymentLinkID string
	Status        string
	Amount        int64
	ExpiredAt     int64
}

// Client calls PayOS with the merchant channel's credentials. BaseURL
// and HTTP are seams for tests; the zero value talks to production.
type Client struct {
	BaseURL     string
	ClientID    string
	APIKey      string
	ChecksumKey string
	HTTP        *http.Client
}

// APIError is a PayOS business failure: the call was authenticated, but
// PayOS refused it with its own code ("01" is "order not found", the
// rest mean the request or the link state is wrong).
type APIError struct {
	Code string
	Desc string
}

func (e *APIError) Error() string { return "payos: " + e.Code + ": " + e.Desc }

// ErrResponseSignature reports a response whose body signature does not
// verify. The payload was tampered with in transit or the checksum key
// does not belong to the credentials in use.
var ErrResponseSignature = errors.New("payos: response signature invalid")

// ErrRateLimited reports PayOS's 429. The caller backs off; the adapter
// never retries inside a request that may have created money movement.
var ErrRateLimited = errors.New("payos: rate limited")

// paymentLinkData is PayOS's payment-request object. Create and query
// return the same shape, so both decode through it.
type paymentLinkData struct {
	CheckoutURL   string `json:"checkoutUrl"`
	QRCode        string `json:"qrCode"`
	PaymentLinkID string `json:"paymentLinkId"`
	Status        string `json:"status"`
	Amount        int64  `json:"amount"`
	ExpiredAt     int64  `json:"expiredAt"`
}

func (d paymentLinkData) paymentLink() *PaymentLink {
	return &PaymentLink{
		CheckoutURL: d.CheckoutURL, QRCode: d.QRCode, PaymentLinkID: d.PaymentLinkID,
		Status: d.Status, Amount: d.Amount, ExpiredAt: d.ExpiredAt,
	}
}

// CreatePaymentLink creates (or, on PayOS's duplicate error, reports)
// the link for an order. The response signature over data is verified
// before checkoutUrl is returned: that URL is where the customer is sent
// with the money.
func (c *Client) CreatePaymentLink(ctx context.Context, r CreateRequest) (*PaymentLink, error) {
	body := map[string]any{
		"orderCode": r.OrderCode, "amount": r.Amount, "description": r.Description,
		"cancelUrl": r.CancelURL, "returnUrl": r.ReturnURL,
		"signature": SignCreateRequest(c.ChecksumKey, r),
	}
	if r.ExpiredAt > 0 {
		body["expiredAt"] = r.ExpiredAt
	}
	if r.BuyerName != "" {
		body["buyerName"] = r.BuyerName
	}
	if r.BuyerEmail != "" {
		body["buyerEmail"] = r.BuyerEmail
	}
	if r.BuyerPhone != "" {
		body["buyerPhone"] = r.BuyerPhone
	}
	if r.BuyerAddress != "" {
		body["buyerAddress"] = r.BuyerAddress
	}
	if r.BuyerTaxID != "" {
		body["buyerTaxCode"] = r.BuyerTaxID
	}
	if len(r.Items) > 0 {
		body["items"] = r.Items
	}
	var out struct {
		Code      string          `json:"code"`
		Desc      string          `json:"desc"`
		Data      json.RawMessage `json:"data"`
		Signature string          `json:"signature"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/payment-requests", body, &out); err != nil {
		return nil, err
	}
	if out.Code != "00" || noData(out.Data) {
		// "Đơn thanh toán đã tồn tại" (the link already exists) arrives
		// here too. The caller can query by orderCode to recover the
		// link, so the code and description are preserved for it.
		return nil, &APIError{Code: out.Code, Desc: out.Desc}
	}
	if err := c.verifyResponseData(out.Data, out.Signature); err != nil {
		return nil, err
	}
	return decodePaymentLink(out.Data)
}

// GetPaymentLink reads the current state of a link by its numeric order
// code. The reconciler uses it to settle a payment whose webhook was
// lost, so a not-found code surfaces as *APIError ("01").
func (c *Client) GetPaymentLink(ctx context.Context, orderCode int64) (*PaymentLink, error) {
	var out struct {
		Code string          `json:"code"`
		Desc string          `json:"desc"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/payment-requests/"+strconv.FormatInt(orderCode, 10), nil, &out); err != nil {
		return nil, err
	}
	if out.Code != "00" || noData(out.Data) {
		return nil, &APIError{Code: out.Code, Desc: out.Desc}
	}
	// The query response is signed too, but its data embeds a
	// transactions array whose nested objects PayOS canonicalises in
	// JSON member order while Go re-sorts map keys, so verifying here
	// would reject honest responses. Settlement re-checks the amount and
	// the order's own state, which is the authoritative guard.
	return decodePaymentLink(out.Data)
}

// CancelPaymentLink cancels an unpaid link. PayOS only accepts the
// numeric orderCode here, not the paymentLinkId.
func (c *Client) CancelPaymentLink(ctx context.Context, orderCode int64, reason string) error {
	body := map[string]any{"cancellationReason": reason}
	return c.do(ctx, http.MethodPost, "/v2/payment-requests/"+strconv.FormatInt(orderCode, 10)+"/cancel", body, nil)
}

// ConfirmWebhook registers webhookURL with PayOS. PayOS probes the URL
// immediately with a sample delivery, so the URL must be public HTTPS
// and already answer 2xx; this is an explicit admin action.
func (c *Client) ConfirmWebhook(ctx context.Context, webhookURL string) error {
	return c.do(ctx, http.MethodPost, "/confirm-webhook", map[string]any{"webhookUrl": webhookURL}, nil)
}

// TestConnection proves the credentials without creating money movement.
// Querying a non-existent order is enough: a 401 means bad keys, and
// PayOS's "not found" code (01) means they work.
func (c *Client) TestConnection(ctx context.Context) error {
	if _, err := c.GetPaymentLink(ctx, 1); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// do sends one request with PayOS's auth headers and maps the transport
// status onto errors. Business codes are left to the caller when out is
// non-nil (it carries Code/Desc); a bodyless call (cancel, confirm) has
// its envelope code checked here so a refused mutation is never silent.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("payos: encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("x-client-id", c.ClientID)
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("payos: read response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode >= 500:
		return fmt.Errorf("payos: server error %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("payos: decode response (status %d): %w", resp.StatusCode, err)
		}
		return nil
	}
	var env struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
	}
	if len(bytes.TrimSpace(payload)) > 0 && json.Unmarshal(payload, &env) == nil &&
		env.Code != "" && env.Code != "00" {
		return &APIError{Code: env.Code, Desc: env.Desc}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("payos: http %d", resp.StatusCode)
	}
	return nil
}

// verifyResponseData checks PayOS's signature over a response's data
// object. A missing signature is tolerated (the field's presence has
// varied across PayOS versions and TLS still authenticates the peer); a
// present but wrong one is a tamper signal and always fails.
func (c *Client) verifyResponseData(raw json.RawMessage, signature string) error {
	if signature == "" {
		return nil
	}
	data, err := decodeObject(raw)
	if err != nil {
		return err
	}
	if !VerifyDataSignature(c.ChecksumKey, data, signature) {
		return ErrResponseSignature
	}
	return nil
}

func decodePaymentLink(raw json.RawMessage) (*PaymentLink, error) {
	var data paymentLinkData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("payos: decode payment link: %w", err)
	}
	return data.paymentLink(), nil
}

// decodeObject decodes a data object for signature verification with
// numbers kept exact, matching what PayOS signed.
func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("payos: decode response data: %w", err)
	}
	return m, nil
}

func noData(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func isNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == "01"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}
