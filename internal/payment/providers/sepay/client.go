package sepay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is the SePay user-API client used for connection checks and
// reconciliation. BaseURL is separate so tests can point it at httptest
// and the worker can select sandbox from the resolved mode.
type Client struct {
	BaseURL  string // https://userapi.sepay.vn/v2 (live), …-sandbox… in test
	APIToken string
	HTTP     *http.Client
}

// BankTransaction is one bank-account transaction from the user API,
// normalised onto the fields the reconciler matches.
type BankTransaction struct {
	ID              int64
	TransactionDate time.Time
	AccountNumber   string
	Code            string
	Content         string
	TransferType    string
	TransferAmount  int64
	ReferenceCode   string
}

// TestConnection proves the token can read the transaction list without
// moving money.
func (c *Client) TestConnection(ctx context.Context) error {
	if c.APIToken == "" {
		return ErrMissingAPIKey
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/transactions?per_page=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sepay: test connection status %d", resp.StatusCode)
	}
	return nil
}

// ListTransactions reads one page of bank transactions, ascending by id
// and date. sinceID resumes after a previously seen transaction so the
// reconciler never reprocesses a page it already ingested.
func (c *Client) ListTransactions(ctx context.Context, from, to time.Time, sinceID int64) ([]BankTransaction, error) {
	if c.APIToken == "" {
		return nil, ErrMissingAPIKey
	}
	q := url.Values{}
	q.Set("transaction_date_from", from.Format("2006-01-02 15:04:05"))
	q.Set("transaction_date_to", to.Format("2006-01-02 15:04:05"))
	q.Set("transaction_date_sort", "asc")
	q.Set("timestamp_format", "iso8601")
	q.Set("per_page", "100")
	if sinceID > 0 {
		q.Set("since_id", strconv.FormatInt(sinceID, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/transactions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sepay: list transactions status %d", resp.StatusCode)
	}
	var body struct {
		Transactions []struct {
			ID              int64  `json:"id"`
			TransactionDate string `json:"transaction_date"`
			AccountNumber   string `json:"account_number"`
			Code            string `json:"code"`
			Content         string `json:"content"`
			TransferType    string `json:"transfer_type"`
			TransferAmount  int64  `json:"transfer_amount"`
			ReferenceCode   string `json:"reference_code"`
		} `json:"transactions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]BankTransaction, 0, len(body.Transactions))
	for _, t := range body.Transactions {
		// An unparsable date is an error, not a zero time: the reconciler
		// advances its since-cursor from these rows, and year 1 would make
		// every later pass re-read the whole table.
		ts, err := time.Parse(time.RFC3339, t.TransactionDate)
		if err != nil {
			return nil, fmt.Errorf("sepay: transaction %d has invalid transaction_date %q: %w", t.ID, t.TransactionDate, err)
		}
		out = append(out, BankTransaction{ID: t.ID, TransactionDate: ts, AccountNumber: t.AccountNumber,
			Code: t.Code, Content: t.Content, TransferType: t.TransferType,
			TransferAmount: t.TransferAmount, ReferenceCode: t.ReferenceCode})
	}
	return out, nil
}

// parseRetryAfter reads the HTTP Retry-After header in both its forms:
// delay-seconds and the HTTP-date form. An absent or unusable value
// yields zero, and the caller applies its own default backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// ErrRateLimited is returned on HTTP 429 so a caller can back off
// instead of treating the page as empty.
var ErrRateLimited = errors.New("sepay: rate limited")

// RateLimitError is the 429 error: it unwraps to ErrRateLimited and
// carries the Retry-After hint, when the server sent a usable one.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v (retry after %s)", ErrRateLimited, e.RetryAfter)
	}
	return ErrRateLimited.Error()
}

// Unwrap keeps errors.Is(err, ErrRateLimited) true for callers that only
// switch on the sentinel.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}
