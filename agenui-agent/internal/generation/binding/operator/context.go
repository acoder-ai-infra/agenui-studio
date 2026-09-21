package operator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxExecutionIdentityRunes = 256
	maxCanonicalJSONDepth     = 32
	maxCanonicalJSONNodes     = 32_768
)

type BindingExecutionContextConfig struct {
	TenantID       string
	UserID         string
	SessionID      string
	RunID          string
	AgentID        string
	ToolCallID     string
	IdempotencyKey string
}

// BindingExecutionContext keeps server-owned identity private so neither
// model JSON nor an operator response can substitute scope or replay keys.
type BindingExecutionContext struct {
	tenantID       string
	userID         string
	sessionID      string
	runID          string
	agentID        string
	toolCallID     string
	idempotencyKey string
}

func NewBindingExecutionContext(config BindingExecutionContextConfig) (BindingExecutionContext, error) {
	values := []string{
		config.TenantID, config.UserID, config.SessionID, config.RunID,
		config.AgentID, config.ToolCallID, config.IdempotencyKey,
	}
	for _, value := range values {
		if !validIdentity(value) {
			return BindingExecutionContext{}, ErrInvalidExecutionContext
		}
	}
	return BindingExecutionContext{
		tenantID: config.TenantID, userID: config.UserID, sessionID: config.SessionID,
		runID: config.RunID, agentID: config.AgentID, toolCallID: config.ToolCallID,
		idempotencyKey: config.IdempotencyKey,
	}, nil
}

func (c BindingExecutionContext) valid() bool {
	return validIdentity(c.tenantID) && validIdentity(c.userID) &&
		validIdentity(c.sessionID) && validIdentity(c.runID) &&
		validIdentity(c.agentID) && validIdentity(c.toolCallID) &&
		validIdentity(c.idempotencyKey)
}

func (c BindingExecutionContext) runtimeScope() OperatorRuntimeScope {
	return OperatorRuntimeScope{
		TenantID: c.tenantID, UserID: c.userID, SessionID: c.sessionID,
		RunID: c.runID, AgentID: c.agentID, ToolCallID: c.toolCallID,
	}
}

func (c BindingExecutionContext) idempotencyScopeHash() string {
	parts := []string{
		c.tenantID, c.userID, c.sessionID, c.runID, c.agentID, c.toolCallID,
		hashBytes([]byte(c.idempotencyKey)),
	}
	return hashBytes([]byte(strings.Join(parts, "\x00")))
}

// CanonicalJSONHash rejects duplicate keys and trailing values before hashing.
func CanonicalJSONHash(raw json.RawMessage) (string, error) {
	canonical, _, err := canonicalizeJSON(raw, false)
	if err != nil {
		return "", err
	}
	return hashBytes(canonical), nil
}

func validIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		len([]rune(value)) > maxExecutionIdentityRunes {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func validSHA256Hash(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	encoded := value[len("sha256:"):]
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size && encoded == strings.ToLower(encoded)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hashPrefix(value string, length int) string {
	value = strings.TrimPrefix(value, "sha256:")
	if length < 0 || length > len(value) {
		return ""
	}
	return value[:length]
}

func canonicalizeJSON(raw []byte, requireObject bool) ([]byte, any, error) {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, nil, fmt.Errorf("operator: invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	budget := jsonDecodeBudget{remainingNodes: maxCanonicalJSONNodes}
	value, err := decodeUniqueJSONValue(decoder, &budget, 1)
	if err != nil {
		return nil, nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, nil, fmt.Errorf("operator: trailing JSON")
	}
	if requireObject {
		if _, ok := value.(map[string]any); !ok {
			return nil, nil, fmt.Errorf("operator: JSON value must be an object")
		}
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("operator: canonical JSON is unavailable")
	}
	return canonical, value, nil
}

type jsonDecodeBudget struct{ remainingNodes int }

func decodeUniqueJSONValue(decoder *json.Decoder, budget *jsonDecodeBudget, depth int) (any, error) {
	if budget == nil || budget.remainingNodes <= 0 || depth > maxCanonicalJSONDepth {
		return nil, fmt.Errorf("operator: JSON exceeds safe structural bounds")
	}
	budget.remainingNodes--
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("operator: invalid JSON")
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		switch value := token.(type) {
		case nil, bool, string, json.Number:
			return value, nil
		default:
			return nil, fmt.Errorf("operator: unsupported JSON token")
		}
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return nil, fmt.Errorf("operator: invalid JSON object")
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("operator: invalid JSON object key")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("operator: duplicate JSON object key")
			}
			value, valueErr := decodeUniqueJSONValue(decoder, budget, depth+1)
			if valueErr != nil {
				return nil, valueErr
			}
			object[key] = value
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("operator: invalid JSON object")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, valueErr := decodeUniqueJSONValue(decoder, budget, depth+1)
			if valueErr != nil {
				return nil, valueErr
			}
			array = append(array, value)
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("operator: invalid JSON array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("operator: invalid JSON delimiter")
	}
}
