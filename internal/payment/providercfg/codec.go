// Package providercfg stores payment-provider secrets encrypted at rest
// and resolves which provider configuration applies to an order.
package providercfg

import (
	"fmt"

	"github.com/dtg-corp/dsoft/internal/crypto"
)

const subkeyPurpose = "payment-config"

// Codec seals each secret with its own AES-GCM key (HKDF subkey of the
// install master) and binds the ciphertext to the row and field, so a
// credentials blob copied onto another provider row fails to decrypt
// instead of silently authenticating as that provider.
type Codec struct{ aead *crypto.AESGCM }

func NewCodec(masterKey []byte) (*Codec, error) {
	sub, err := crypto.DeriveSubkey(masterKey, subkeyPurpose)
	if err != nil {
		return nil, fmt.Errorf("derive payment-config subkey: %w", err)
	}
	aead, err := crypto.NewAESGCM(sub)
	if err != nil {
		return nil, fmt.Errorf("payment-config aead: %w", err)
	}
	return &Codec{aead: aead}, nil
}

// aad follows the spec's convention, payment_provider_config:<id>:<field>,
// so the same ciphertext cannot be replayed on another row or field.
func aad(id, field string) []byte {
	return []byte("payment_provider_config:" + id + ":" + field)
}

func (c *Codec) Seal(id, field string, plaintext []byte) ([]byte, error) {
	return c.aead.Encrypt(plaintext, aad(id, field))
}

func (c *Codec) Open(id, field string, envelope []byte) ([]byte, error) {
	return c.aead.Decrypt(envelope, aad(id, field))
}
