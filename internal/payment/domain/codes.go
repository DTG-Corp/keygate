package domain

import (
	"crypto/rand"
	"encoding/binary"
	"regexp"
)

// crockford is Crockford Base32: no I, L, O or U. A customer reading a
// transfer memo to support must not be able to confuse 0/O or 1/I/L.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const orderCodeLength = 6

// NewOrderCode returns "KG" + 6 Crockford Base32 characters (30 bits).
// 2^30 codes keep collision odds negligible at this scale; the UNIQUE
// index on payment_orders.code is the real guard, and the store retries
// on a conflict.
func NewOrderCode() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("domain: crypto/rand failed: " + err.Error())
	}
	n := binary.BigEndian.Uint32(b[:]) & (1<<30 - 1)

	out := make([]byte, 0, 2+orderCodeLength)
	out = append(out, 'K', 'G')
	for i := orderCodeLength - 1; i >= 0; i-- {
		out = append(out, crockford[(n>>(5*uint(i)))&31])
	}
	return string(out)
}

var orderCodeRE = regexp.MustCompile(`^KG[0-9A-HJKMNP-TV-Z]{6}$`)

// IsOrderCode reports whether s has the shape of a NewOrderCode result.
func IsOrderCode(s string) bool { return orderCodeRE.MatchString(s) }
