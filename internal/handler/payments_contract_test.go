package handler

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file pins the payment API's error vocabulary. pkg/response's
// TestErrorCodeMapsToExactlyOneStatus proves mechanically that no code
// is written with two statuses anywhere in the server; this test proves
// that the payment routes' vocabulary itself is the one in
// docs/api-contract.md, and that no payment code quietly disappears or
// arrives from a helper with a different status.
//
// It reads the handler sources rather than calling the routes: a call
// site for every code is visible there, including the branches no test
// can reach without a provider, a live ledger row or a broken config.
// New codes must be added here and in the contract together.

// paymentSourceFiles are the files that write the payment API's
// responses. Adding a payment handler in another file without adding it
// here would hide its codes, so a test below also refuses a payment
// handler file that the list does not name.
var paymentSourceFiles = []string{
	"payments_public.go",
	"payments_api.go",
	"payments_webhook.go",
	"payments_admin.go",
	"payment_providers_admin.go",
	"plan_prices_admin.go",
}

// paymentCode pins one documented code to its one status.
//
// emitted is false for a code the contract documents but no handler
// writes. PAYMENT_WEBHOOK_UNAUTHORIZED is that case in phase 1a: all
// three webhook routes answer a bare 401 with no envelope, because the
// provider, not an SDK, reads that response (see
// TestPaymentWebhookAuthIsBare401). The entry keeps the documented
// status pinned, and fails this test if a handler starts emitting the
// code without the docs being updated.
type paymentCode struct {
	status  int
	emitted bool
}

var paymentVocabulary = map[string]paymentCode{
	// Shared envelope codes these handlers use for a malformed body or
	// an internal fault. Their global statuses are fixed everywhere.
	"BAD_REQUEST":    {status: 400, emitted: true},
	"NOT_FOUND":      {status: 404, emitted: true},
	"INTERNAL_ERROR": {status: 500, emitted: true},

	"PRODUCT_SCOPE_MISMATCH": {status: 403, emitted: true},

	"INVALID_AMOUNT":                  {status: 400, emitted: true},
	"INVALID_CURRENCY_EXPONENT":       {status: 400, emitted: true},
	"INVALID_QUANTITY":                {status: 400, emitted: true},
	"INVALID_STATUS":                  {status: 400, emitted: true},
	"PAYMENT_PROVIDER_UNKNOWN":        {status: 400, emitted: true},
	"PAYMENT_PROVIDER_CONFIG_INVALID": {status: 400, emitted: true},
	"PAYMENT_WEBHOOK_URL_UNSET":       {status: 400, emitted: true},

	"PAYMENT_ORDER_NOT_FOUND":       {status: 404, emitted: true},
	"PAYMENT_PROVIDER_NOT_FOUND":    {status: 404, emitted: true},
	"PAYMENT_TRANSACTION_NOT_FOUND": {status: 404, emitted: true},
	"PLAN_NOT_FOUND":                {status: 404, emitted: true},
	"PRICE_NOT_FOUND":               {status: 404, emitted: true},

	"PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN":   {status: 409, emitted: true},
	"PAYMENT_ORDER_NOT_PAYABLE":             {status: 409, emitted: true},
	"PAYMENT_ORDER_NOT_REFUNDABLE":          {status: 409, emitted: true},
	"PAYMENT_TRANSACTION_NOT_UNMATCHED":     {status: 409, emitted: true},
	"PAYMENT_TRANSACTION_PROVIDER_MISMATCH": {status: 409, emitted: true},
	"PRICE_SLOT_CONFLICT":                   {status: 409, emitted: true},

	"PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE": {status: 422, emitted: true},
	"PAYMENT_PRICE_NOT_CONFIGURED":           {status: 422, emitted: true},
	"PAYMENT_REFUND_EXCEEDS_ORDER":           {status: 422, emitted: true},
	"PAYMENT_REFUND_INVALID":                 {status: 422, emitted: true},
	"PAYMENT_REFUND_UNSUPPORTED":             {status: 422, emitted: true},
	"PAYMENT_TRANSACTION_MISMATCH":           {status: 422, emitted: true},
	"PAYMENT_TRANSACTION_NOT_INCOMING":       {status: 422, emitted: true},

	"PAYMENT_PROVIDER_ERROR":    {status: 502, emitted: true},
	"PAYMENT_PROVIDER_DISABLED": {status: 503, emitted: true},

	// Documented, not emitted: the raw-body webhook routes answer 401
	// without a JSON error envelope. See the paymentCode comment.
	"PAYMENT_WEBHOOK_UNAUTHORIZED": {status: 401, emitted: false},
}

// httpStatusCode resolves the http.Status* selectors the handlers use.
var httpStatusCode = map[string]int{
	"StatusBadRequest":            400,
	"StatusUnauthorized":          401,
	"StatusForbidden":             403,
	"StatusNotFound":              404,
	"StatusRequestEntityTooLarge": 413,
	"StatusConflict":              409,
	"StatusUnprocessableEntity":   422,
	"StatusInternalServerError":   500,
	"StatusBadGateway":            502,
	"StatusServiceUnavailable":    503,
}

// fixedResponseCodes are the package helpers that write a status and
// code fixed in their name.
var fixedResponseCodes = map[string]struct {
	code   string
	status int
}{
	"BadRequest":   {"BAD_REQUEST", 400},
	"Unauthorized": {"UNAUTHORIZED", 401},
	"Forbidden":    {"FORBIDDEN", 403},
	"NotFound":     {"NOT_FOUND", 404},
	"Internal":     {"INTERNAL_ERROR", 500},
}

// successResponseWriters are the response helpers these files may call
// without writing an error. Anything else under response.* is refused
// below so a new error helper cannot slip past this scan.
var successResponseWriters = map[string]bool{
	"OK": true, "Created": true, "NoContent": true, "Array": true,
}

type errorSite struct {
	code   string
	status int
	where  string
}

func TestPaymentErrorVocabulary(t *testing.T) {
	sites, files := scanPaymentErrorSites(t)

	// An emitted code must be in the vocabulary with exactly the status
	// it is written with, and a vocabulary entry marked emitted must be
	// present. Both directions matter: an unpinned code reaches clients
	// undocumented, and an "emitted" entry that no longer exists means
	// the docs describe a branch the server does not have.
	byCode := map[string]map[int][]string{}
	for _, s := range sites {
		if byCode[s.code] == nil {
			byCode[s.code] = map[int][]string{}
		}
		byCode[s.code][s.status] = append(byCode[s.code][s.status], s.where)
	}

	scanned := make(map[string]bool, len(byCode))
	for code, statuses := range byCode {
		scanned[code] = true
		spec, ok := paymentVocabulary[code]
		if !ok {
			t.Errorf("%s is emitted by a payment handler but is not pinned in paymentVocabulary", code)
			continue
		}
		if !spec.emitted {
			t.Errorf("%s is documented as not emitted, but a handler writes it", code)
		}
		if len(statuses) != 1 {
			t.Errorf("%s is written with %d statuses; a client branches on one", code, len(statuses))
		}
		for status, where := range statuses {
			sort.Strings(where)
			if status != spec.status {
				t.Errorf("%s is written with %d at %s; the pinned status is %d",
					code, status, strings.Join(where, ", "), spec.status)
			}
		}
	}
	for code, spec := range paymentVocabulary {
		if spec.emitted && !scanned[code] {
			t.Errorf("%s is pinned as emitted, but no payment handler writes it; update paymentVocabulary and docs/api-contract.md", code)
		}
		if spec.status == 0 {
			t.Errorf("%s has no status", code)
		}
	}

	// The payment handlers are a known set. A new payments_*.go file
	// with error call sites would otherwise be invisible here.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read handler directory: %v", err)
	}
	known := make(map[string]bool, len(paymentSourceFiles))
	for _, f := range paymentSourceFiles {
		known[f] = true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(name, "payments_") && name != "payment_providers_admin.go" && name != "plan_prices_admin.go" {
			continue
		}
		if !known[name] {
			t.Errorf("%s is a payment handler file not listed in paymentSourceFiles", name)
		}
	}

	if files["payments_webhook.go"] != 0 {
		t.Errorf("payments_webhook.go writes %d enveloped error(s); the raw-body webhook routes answer a bare status, not the envelope", files["payments_webhook.go"])
	}
}

// TestPaymentWebhookAuthIsBare401 pins why PAYMENT_WEBHOOK_UNAUTHORIZED is
// documented but not emitted: a provider reads the webhook response, not
// an SDK, so an authentication failure is a bare 401 (no JSON code) and a
// body the adapter cannot parse is a 400. The webhook package's adapters
// prove the signature check itself; this test proves the route maps its
// failure to the status providers retry on.
func TestPaymentWebhookAuthIsBare401(t *testing.T) {
	src, err := os.ReadFile("payments_webhook.go")
	if err != nil {
		t.Fatalf("read payments_webhook.go: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"c.Status(http.StatusUnauthorized)",
		"c.Status(http.StatusBadRequest)",
		`gin.H{"success": true}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("payments_webhook.go no longer contains %q; update docs/api-contract.md and paymentVocabulary", want)
		}
	}
}

// scanPaymentErrorSites parses every payment handler file and returns
// the (code, status) call sites plus, per file, how many enveloped
// errors it writes. The webhook file must be zero.
func scanPaymentErrorSites(t *testing.T) ([]errorSite, map[string]int) {
	t.Helper()
	fset := token.NewFileSet()
	var sites []errorSite
	perFile := make(map[string]int, len(paymentSourceFiles))

	for _, name := range paymentSourceFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var scanErr string
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "response" {
				return true
			}
			pos := fset.Position(call.Pos())
			switch sel.Sel.Name {
			case "Err", "ErrWithDetails":
				code, status, err := errorCallArgs(call)
				if err != nil {
					scanErr = pos.String() + ": " + err.Error()
					return false
				}
				sites = append(sites, errorSite{code: code, status: status, where: pos.String()})
				perFile[name]++
			default:
				fixed, ok := fixedResponseCodes[sel.Sel.Name]
				if !ok {
					if !successResponseWriters[sel.Sel.Name] {
						scanErr = pos.String() + ": response." + sel.Sel.Name + " is not an error writer this scan understands"
						return false
					}
					return true
				}
				sites = append(sites, errorSite{code: fixed.code, status: fixed.status, where: pos.String()})
				perFile[name]++
			}
			return true
		})
		if scanErr != "" {
			t.Fatal(scanErr)
		}
	}
	return sites, perFile
}

// errorCallArgs reads response.Err(c, http.StatusX, "CODE", ...).
func errorCallArgs(call *ast.CallExpr) (string, int, error) {
	if len(call.Args) < 3 {
		return "", 0, errAt(call, "response.Err has fewer than three arguments")
	}
	status, ok := httpStatus(call.Args[1])
	if !ok {
		return "", 0, errAt(call, "response.Err status is not an http.Status constant")
	}
	lit, ok := call.Args[2].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", 0, errAt(call, "response.Err code is not a string literal")
	}
	code, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", 0, errAt(call, "response.Err code is not a valid string")
	}
	return code, status, nil
}

func httpStatus(expr ast.Expr) (int, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "http" {
		return 0, false
	}
	status, ok := httpStatusCode[sel.Sel.Name]
	return status, ok
}

func errAt(_ ast.Node, msg string) error { return errors.New(msg) }
