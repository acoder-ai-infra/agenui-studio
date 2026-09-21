// Package controlticket provides opaque, authenticated credentials for replying
// to one ControlRequest. The encrypted claims bind the resume secret to the
// authenticated actor and the exact Session/Run/Request tuple.
package controlticket

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"
)

const (
	tokenVersion          byte = 1
	timestampBytes             = 8
	MaxEncodedTicketBytes      = 8192
)

var (
	ticketAAD        = []byte("harness:control-ticket:v1")
	ErrInvalidTicket = errors.New("invalid control ticket")
)

type Claims struct {
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	SessionID   string `json:"session_id"`
	RunID       string `json:"run_id"`
	RequestID   string `json:"request_id"`
	ResumeToken string `json:"resume_token"`
}

func (c Claims) valid() bool {
	return c.TenantID != "" && c.UserID != "" && c.SessionID != "" && c.RunID != "" && c.RequestID != "" && c.ResumeToken != ""
}

// Codec is safe for concurrent use.
type Codec struct {
	aead cipher.AEAD
}

func New(secret []byte) (*Codec, error) {
	if len(secret) == 0 {
		return nil, errors.New("control ticket secret is required")
	}
	// Domain-separate this key from other codecs even when deployment supplies one
	// shared root secret.
	key := sha256.Sum256(append([]byte("harness:control-ticket:key:v1\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead}, nil
}

func (c *Codec) Seal(claims Claims, expiresAt time.Time) (string, error) {
	if c == nil || !claims.valid() || expiresAt.IsZero() {
		return "", ErrInvalidTicket
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	plaintext := make([]byte, 1+timestampBytes+len(payload))
	plaintext[0] = tokenVersion
	binary.BigEndian.PutUint64(plaintext[1:1+timestampBytes], uint64(expiresAt.UnixNano()))
	copy(plaintext[1+timestampBytes:], payload)
	sealed := c.aead.Seal(nonce, nonce, plaintext, ticketAAD)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *Codec) Open(ticket string) (Claims, time.Time, error) {
	if c == nil || len(ticket) == 0 || len(ticket) > MaxEncodedTicketBytes {
		return Claims{}, time.Time{}, ErrInvalidTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil {
		return Claims{}, time.Time{}, ErrInvalidTicket
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return Claims{}, time.Time{}, ErrInvalidTicket
	}
	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], ticketAAD)
	if err != nil || len(plaintext) < 1+timestampBytes || plaintext[0] != tokenVersion {
		return Claims{}, time.Time{}, ErrInvalidTicket
	}
	expiresAt := time.Unix(0, int64(binary.BigEndian.Uint64(plaintext[1:1+timestampBytes])))
	var claims Claims
	if err := json.Unmarshal(plaintext[1+timestampBytes:], &claims); err != nil || !claims.valid() {
		return Claims{}, time.Time{}, ErrInvalidTicket
	}
	return claims, expiresAt, nil
}
