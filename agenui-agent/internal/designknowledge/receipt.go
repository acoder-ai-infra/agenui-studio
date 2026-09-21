package designknowledge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const ReceiptSchemaVersion = "agenui.design-knowledge-receipt/v1"

type ReceiptScope struct {
	TenantID, UserID, SessionID, RunID, AgentID string
}

type ReceiptClaims struct {
	SchemaVersion string   `json:"schema_version"`
	RevisionID    string   `json:"revision_id"`
	RevisionHash  string   `json:"revision_hash"`
	LayoutIDs     []string `json:"layout_ids"`
	ClosureHash   string   `json:"closure_hash"`
	ScopeHash     string   `json:"scope_hash"`
}

type Receipt struct {
	SchemaVersion string   `json:"schema_version"`
	Attestation   string   `json:"attestation"`
	RevisionID    string   `json:"revision_id"`
	RevisionHash  string   `json:"revision_hash"`
	LayoutIDs     []string `json:"layout_ids"`
	ClosureHash   string   `json:"closure_hash"`
	ScopeHash     string   `json:"scope_hash"`
}

type ReceiptAuthority struct{ key []byte }

func NewReceiptAuthority() (*ReceiptAuthority, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("design knowledge receipt: generate key: %w", err)
	}
	return NewReceiptAuthorityWithKey(key)
}

func NewReceiptAuthorityWithKey(key []byte) (*ReceiptAuthority, error) {
	if len(key) < 32 {
		return nil, errors.New("design knowledge receipt: key must contain at least 32 bytes")
	}
	return &ReceiptAuthority{key: append([]byte(nil), key...)}, nil
}

func (a *ReceiptAuthority) Issue(scope ReceiptScope, revisionID, revisionHash string, layoutIDs []string, documents []Document) (Receipt, error) {
	if a == nil || len(a.key) == 0 {
		return Receipt{}, errors.New("design knowledge receipt: authority is required")
	}
	if err := validateReceiptScope(scope); err != nil {
		return Receipt{}, err
	}
	claims := ReceiptClaims{
		SchemaVersion: ReceiptSchemaVersion,
		RevisionID:    strings.TrimSpace(revisionID),
		RevisionHash:  strings.TrimSpace(revisionHash),
		LayoutIDs:     normalizedIDs(layoutIDs),
		ClosureHash:   closureHash(documents),
		ScopeHash:     a.scopeHash(scope),
	}
	if err := validateReceiptClaims(claims); err != nil {
		return Receipt{}, err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return Receipt{}, fmt.Errorf("design knowledge receipt: encode claims: %w", err)
	}
	return Receipt{
		SchemaVersion: claims.SchemaVersion,
		Attestation:   base64.RawURLEncoding.EncodeToString(a.sign(payload)),
		RevisionID:    claims.RevisionID,
		RevisionHash:  claims.RevisionHash,
		LayoutIDs:     append([]string(nil), claims.LayoutIDs...),
		ClosureHash:   claims.ClosureHash,
		ScopeHash:     claims.ScopeHash,
	}, nil
}

func (a *ReceiptAuthority) Verify(receipt Receipt, scope *ReceiptScope) (ReceiptClaims, error) {
	if a == nil || len(a.key) == 0 {
		return ReceiptClaims{}, errors.New("design knowledge receipt: authority is required")
	}
	claims := ReceiptClaims{
		SchemaVersion: receipt.SchemaVersion,
		RevisionID:    strings.TrimSpace(receipt.RevisionID),
		RevisionHash:  strings.TrimSpace(receipt.RevisionHash),
		LayoutIDs:     normalizedIDs(receipt.LayoutIDs),
		ClosureHash:   strings.TrimSpace(receipt.ClosureHash),
		ScopeHash:     strings.TrimSpace(receipt.ScopeHash),
	}
	if err := validateReceiptClaims(claims); err != nil {
		return ReceiptClaims{}, err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return ReceiptClaims{}, errors.New("design knowledge receipt: invalid claims")
	}
	signature, err := base64.RawURLEncoding.DecodeString(receipt.Attestation)
	if err != nil || !hmac.Equal(signature, a.sign(payload)) {
		return ReceiptClaims{}, errors.New("design knowledge receipt: invalid signature")
	}
	if scope != nil {
		if err := validateReceiptScope(*scope); err != nil {
			return ReceiptClaims{}, err
		}
		if claims.ScopeHash != a.scopeHash(*scope) {
			return ReceiptClaims{}, errors.New("design knowledge receipt: invocation scope mismatch")
		}
	}
	return claims, nil
}

func ParseReceiptJSON(raw []byte) (Receipt, error) {
	var receipt Receipt
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(string(raw))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, errors.New("design knowledge receipt: JSON is invalid")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Receipt{}, errors.New("design knowledge receipt: JSON is invalid")
	}
	if receipt.SchemaVersion != ReceiptSchemaVersion || receipt.Attestation == "" ||
		receipt.RevisionID == "" || receipt.RevisionHash == "" || len(receipt.LayoutIDs) == 0 ||
		receipt.ClosureHash == "" || receipt.ScopeHash == "" {
		return Receipt{}, errors.New("design knowledge receipt: JSON is incomplete or unsupported")
	}
	return receipt, nil
}

func (a *ReceiptAuthority) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func (a *ReceiptAuthority) scopeHash(scope ReceiptScope) string {
	raw, _ := json.Marshal([]string{scope.TenantID, scope.UserID, scope.SessionID, scope.RunID, scope.AgentID})
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte("scope\x00"))
	_, _ = mac.Write(raw)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

func closureHash(documents []Document) string {
	type closureDocument struct {
		ID          string `json:"id"`
		Version     string `json:"version"`
		ContentHash string `json:"content_hash"`
	}
	values := make([]closureDocument, 0, len(documents))
	for _, document := range documents {
		values = append(values, closureDocument{document.Metadata.ID, document.Metadata.Version, document.Metadata.ContentHash})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	raw, _ := json.Marshal(values)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizedIDs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateReceiptClaims(claims ReceiptClaims) error {
	if claims.SchemaVersion != ReceiptSchemaVersion {
		return errors.New("design knowledge receipt: unsupported schema version")
	}
	if claims.RevisionID == "" || claims.RevisionHash == "" || len(claims.LayoutIDs) == 0 || claims.ClosureHash == "" || claims.ScopeHash == "" {
		return errors.New("design knowledge receipt: incomplete claims")
	}
	return nil
}

func validateReceiptScope(scope ReceiptScope) error {
	if strings.TrimSpace(scope.TenantID) == "" || strings.TrimSpace(scope.UserID) == "" || strings.TrimSpace(scope.SessionID) == "" || strings.TrimSpace(scope.RunID) == "" || strings.TrimSpace(scope.AgentID) == "" {
		return errors.New("design knowledge receipt: incomplete invocation scope")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}
