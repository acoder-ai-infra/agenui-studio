package downloadtoken

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func newTestCodec(t *testing.T) *Codec {
	t.Helper()
	codec, err := New([]byte("unit-test-secret"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return codec
}

func TestSealOpenRoundTrip(t *testing.T) {
	codec := newTestCodec(t)
	payload := "tenants/acme/sessions/s1/runs/r1/a1"
	exp := time.Now().Add(5 * time.Minute).Truncate(time.Nanosecond)

	token, err := codec.Seal(payload, exp)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	gotPayload, gotExp, err := codec.Open(token)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if gotPayload != payload {
		t.Fatalf("Open() payload = %q, want %q", gotPayload, payload)
	}
	if !gotExp.Equal(exp) {
		t.Fatalf("Open() expiresAt = %v, want %v", gotExp, exp)
	}
}

func TestSealIsFreshEveryCall(t *testing.T) {
	codec := newTestCodec(t)
	payload := "tenants/acme/sessions/s1/runs/r1/a1"
	exp := time.Now().Add(time.Minute)

	first, err := codec.Seal(payload, exp)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	second, err := codec.Seal(payload, exp)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if first == second {
		t.Fatal("Seal() produced identical tokens for the same payload; nonce is not fresh")
	}
}

func TestTokenLeaksNoPayloadMarkers(t *testing.T) {
	codec := newTestCodec(t)
	payload := "tenants/secret-tenant/sessions/sess/runs/run/artifact-id"
	token, err := codec.Seal(payload, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not raw URL base64: %v", err)
	}
	for _, marker := range []string{"secret-tenant", "sess", "run", "artifact-id", "tenants"} {
		if strings.Contains(token, marker) {
			t.Fatalf("token %q contains plaintext marker %q", token, marker)
		}
		if strings.Contains(string(decoded), marker) {
			t.Fatalf("decoded token contains plaintext marker %q", marker)
		}
	}
}

func TestOpenRejectsTamperedToken(t *testing.T) {
	codec := newTestCodec(t)
	token, err := codec.Seal("payload", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if _, _, err := codec.Open(tampered); err != ErrInvalidToken {
		t.Fatalf("Open(tampered) error = %v, want ErrInvalidToken", err)
	}
}

func TestOpenRejectsForeignKey(t *testing.T) {
	minter := newTestCodec(t)
	token, err := minter.Seal("payload", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	other, err := New([]byte("a-completely-different-secret"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, _, err := other.Open(token); err != ErrInvalidToken {
		t.Fatalf("Open(foreign key) error = %v, want ErrInvalidToken", err)
	}
}

func TestOpenRejectsWrongVersionAndAAD(t *testing.T) {
	codec := newTestCodec(t)
	sealRaw := func(version byte, aad []byte) string {
		nonce := make([]byte, codec.aead.NonceSize())
		plaintext := make([]byte, 1+timestampBytes+len("payload"))
		plaintext[0] = version
		binary.BigEndian.PutUint64(plaintext[1:1+timestampBytes], uint64(time.Now().Add(time.Minute).Unix()))
		copy(plaintext[1+timestampBytes:], "payload")
		return base64.RawURLEncoding.EncodeToString(codec.aead.Seal(nonce, nonce, plaintext, aad))
	}
	for name, token := range map[string]string{
		"wrong version": sealRaw(tokenVersion+1, tokenAAD),
		"wrong AAD":     sealRaw(tokenVersion, []byte("another-audience")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := codec.Open(token); err != ErrInvalidToken {
				t.Fatalf("Open() error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	codec := newTestCodec(t)
	for _, token := range []string{"", "not base64 %%%", "AAAA", strings.Repeat("A", MaxEncodedTokenBytes+1)} {
		if _, _, err := codec.Open(token); err != ErrInvalidToken {
			t.Fatalf("Open(%q) error = %v, want ErrInvalidToken", token, err)
		}
	}
}

func TestNewRejectsEmptySecret(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) error = nil, want non-nil")
	}
}
