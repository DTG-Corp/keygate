package providercfg_test

import (
	"bytes"
	"testing"

	"github.com/tabloy/keygate/internal/payment/providercfg"
)

func TestCodecRoundTripBindsAAD(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, 32)
	c, err := providercfg.NewCodec(master)
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	sealed, err := c.Seal("cfg-1", "credentials", []byte(`{"api_key":"secret"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("plaintext survived in the ciphertext")
	}
	got, err := c.Open("cfg-1", "credentials", sealed)
	if err != nil || string(got) != `{"api_key":"secret"}` {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Same bytes under a different row/field must not decrypt: AAD is
	// the anti-swap guard.
	if _, err := c.Open("cfg-2", "credentials", sealed); err == nil {
		t.Fatal("decrypt with wrong AAD succeeded")
	}
	if _, err := c.Open("cfg-1", "webhook_secret", sealed); err == nil {
		t.Fatal("decrypt with wrong field succeeded")
	}
}

func TestCodecRejectsShortMaster(t *testing.T) {
	if _, err := providercfg.NewCodec([]byte("short")); err == nil {
		t.Fatal("short master accepted")
	}
}
