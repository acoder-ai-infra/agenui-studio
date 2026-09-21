package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const SchemaVersion = "content_contract.v1"

const (
	DeliveryModeExecutable    = "executable"
	DeliveryModeDesignPreview = "design_preview"
)

// Draft is the minimal business content contract proposed by the Main Agent.
// Revision identity and hashes are host-owned and intentionally absent here.
type Draft struct {
	Goal         string        `json:"goal"`
	Type         string        `json:"type"`
	DeliveryMode string        `json:"delivery_mode,omitempty"`
	Contents     []ContentItem `json:"contents"`
	Actions      []ActionItem  `json:"actions,omitempty"`
	Constraints  []string      `json:"constraints,omitempty"`
}

// ContentItem describes one business value the card must display. ID is a
// stable semantic identity, not an API field name or JSONPath.
type ContentItem struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

// ActionItem describes one user-visible action. It contains no URL, runtime
// event name, parameter mapping, or other binding implementation.
type ActionItem struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// Revision is the host-owned, immutable identity around one canonical Draft.
type Revision struct {
	ContractID    string `json:"contract_id"`
	Revision      int64  `json:"revision"`
	BaseRevision  int64  `json:"base_revision,omitempty"`
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	ChangeOrigin  string `json:"change_origin"`
	ContentHash   string `json:"content_hash"`
	Draft
}

// Canonicalize validates a draft and produces stable ordering and whitespace.
func Canonicalize(source Draft) (Draft, error) {
	result := Draft{
		Goal:         strings.TrimSpace(source.Goal),
		Type:         strings.TrimSpace(source.Type),
		DeliveryMode: strings.TrimSpace(source.DeliveryMode),
	}
	if result.Goal == "" {
		return Draft{}, errors.New("content contract: goal is required")
	}
	if result.Type != "single" && result.Type != "list" {
		return Draft{}, errors.New("content contract: type must be single or list")
	}
	if result.DeliveryMode != "" && result.DeliveryMode != DeliveryModeExecutable && result.DeliveryMode != DeliveryModeDesignPreview {
		return Draft{}, errors.New("content contract: delivery_mode must be executable or design_preview")
	}
	if len(source.Contents) == 0 {
		return Draft{}, errors.New("content contract: at least one content item is required")
	}
	contentIDs := make(map[string]struct{}, len(source.Contents))
	for _, item := range source.Contents {
		item.ID = strings.TrimSpace(item.ID)
		item.Description = strings.TrimSpace(item.Description)
		if item.ID == "" || item.Description == "" {
			return Draft{}, errors.New("content contract: content id and description are required")
		}
		if _, exists := contentIDs[item.ID]; exists {
			return Draft{}, fmt.Errorf("content contract: duplicate content id %q", item.ID)
		}
		contentIDs[item.ID] = struct{}{}
		result.Contents = append(result.Contents, item)
	}
	actionIDs := make(map[string]struct{}, len(source.Actions))
	for _, action := range source.Actions {
		action.ID = strings.TrimSpace(action.ID)
		action.Description = strings.TrimSpace(action.Description)
		if action.ID == "" || action.Description == "" {
			return Draft{}, errors.New("content contract: action id and description are required")
		}
		if _, exists := actionIDs[action.ID]; exists {
			return Draft{}, fmt.Errorf("content contract: duplicate action id %q", action.ID)
		}
		actionIDs[action.ID] = struct{}{}
		result.Actions = append(result.Actions, action)
	}
	seenConstraints := make(map[string]struct{}, len(source.Constraints))
	for _, constraint := range source.Constraints {
		constraint = strings.TrimSpace(constraint)
		if constraint == "" {
			continue
		}
		if _, exists := seenConstraints[constraint]; exists {
			continue
		}
		seenConstraints[constraint] = struct{}{}
		result.Constraints = append(result.Constraints, constraint)
	}
	sort.Slice(result.Contents, func(i, j int) bool { return result.Contents[i].ID < result.Contents[j].ID })
	sort.Slice(result.Actions, func(i, j int) bool { return result.Actions[i].ID < result.Actions[j].ID })
	sort.Strings(result.Constraints)
	return result, nil
}

// IsDesignPreview reports the explicit Host-governed delivery intent. An empty
// value preserves the historical executable default for existing contracts.
func (d Draft) IsDesignPreview() bool {
	return d.DeliveryMode == DeliveryModeDesignPreview
}

// Hash returns the canonical content hash used for idempotency and references.
func Hash(source Draft) (string, error) {
	canonical, err := Canonicalize(source)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
