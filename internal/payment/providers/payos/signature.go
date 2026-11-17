// Package payos adapts PayOS (Vietnamese payment links / VietQR) to the
// provider-neutral payment contract: create a payment link, verify the
// checksum PayOS puts on webhooks, and query/cancel/confirm links.
package payos

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
)

// versionPrefix is the marker PayOS's signing libraries put in front of
// the hex digest on some surfaces ("2" + hex). It is a format marker,
// not part of the MAC, so verification accepts it but nothing else.
const versionPrefix = '2'

// CanonicalData is PayOS's webhook canonical string: keys sorted
// alphabetically, joined as key=value with '&'. Null, "null" and
// "undefined" render empty; objects and arrays are JSON-encoded.
//
// PayOS signs the data object as JSON-decoded values, so the caller must
// preserve numbers exactly (decode with json.Number) or a large order
// code would canonicalise differently from what PayOS signed.
func CanonicalData(data map[string]any) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+canonicalValue(data[k]))
	}
	return joinAmp(parts)
}

func canonicalValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		if t == "null" || t == "undefined" {
			return ""
		}
		return t
	case json.Number:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func joinAmp(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "&" + p
	}
	return out
}

// SignWebhook returns the hex HMAC-SHA256 PayOS puts in a webhook's
// top-level signature field, over the canonical form of its data object.
// The same digest signs a create-link/query response's data object.
func SignWebhook(checksumKey string, data map[string]any) string {
	m := hmac.New(sha256.New, []byte(checksumKey))
	_, _ = m.Write([]byte(CanonicalData(data)))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyDataSignature reports whether signature is PayOS's MAC over
// data under checksumKey. Comparison is constant time; a leading "2"
// version marker is tolerated, anything else must match byte for byte.
func VerifyDataSignature(checksumKey string, data map[string]any, signature string) bool {
	want := SignWebhook(checksumKey, data)
	if hmac.Equal([]byte(want), []byte(signature)) {
		return true
	}
	if len(signature) == len(want)+1 && signature[0] == versionPrefix {
		return hmac.Equal([]byte(want), []byte(signature[1:]))
	}
	return false
}

// SignCreateRequest signs the create-link request. PayOS's canonical
// order is alphabetical: amount, cancelUrl, description, orderCode,
// returnUrl; values are raw (not quoted). Buyer details, items and
// expiredAt are sent unsigned, so they are deliberately not part of it.
func SignCreateRequest(checksumKey string, r CreateRequest) string {
	canonical := "amount=" + strconv.FormatInt(r.Amount, 10) +
		"&cancelUrl=" + r.CancelURL +
		"&description=" + r.Description +
		"&orderCode=" + strconv.FormatInt(r.OrderCode, 10) +
		"&returnUrl=" + r.ReturnURL
	m := hmac.New(sha256.New, []byte(checksumKey))
	_, _ = m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}
