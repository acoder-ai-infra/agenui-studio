package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
)

const DesignArtifactSchemaVersion = "agenui.design-artifact/v1"

// DesignArtifact is the typed, internal hand-off between Workspace, editing,
// binding and materialization. It is not an alternate AGenUI wire protocol.
type DesignArtifact struct {
	SchemaVersion          string           `json:"schema_version"`
	Messages               []map[string]any `json:"messages"`
	FieldSlots             []map[string]any `json:"field_slots"`
	ActionSlots            []map[string]any `json:"action_slots"`
	DesignKnowledgeReceipt map[string]any   `json:"design_knowledge_receipt,omitempty"`
}

func EncodeDesignArtifact(document Document) (string, error) {
	artifact := DesignArtifact{
		SchemaVersion: DesignArtifactSchemaVersion,
		Messages:      document.Messages(), FieldSlots: cloneMaps(document.FieldSlots),
		ActionSlots:            cloneMaps(document.ActionSlots),
		DesignKnowledgeReceipt: cloneMap(document.DesignKnowledgeReceipt),
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("agenui workspace: encode design artifact: %w", err)
	}
	return string(raw), nil
}

func DecodeDesignArtifact(raw string) (DesignArtifact, error) {
	var artifact DesignArtifact
	if err := json.Unmarshal([]byte(raw), &artifact); err != nil {
		return DesignArtifact{}, fmt.Errorf("agenui workspace: decode design artifact: %w", err)
	}
	if artifact.SchemaVersion != DesignArtifactSchemaVersion || len(artifact.Messages) == 0 {
		return DesignArtifact{}, errors.New("agenui workspace: invalid typed design artifact")
	}
	if artifact.FieldSlots == nil {
		artifact.FieldSlots = []map[string]any{}
	}
	if artifact.ActionSlots == nil {
		artifact.ActionSlots = []map[string]any{}
	}
	return artifact, nil
}

func ArtifactMessagesJSON(raw string) (string, error) {
	artifact, err := DecodeDesignArtifact(raw)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(artifact.Messages)
	return string(encoded), err
}

func ArtifactSlotsJSON(raw string) (fields, actions string, err error) {
	artifact, err := DecodeDesignArtifact(raw)
	if err != nil {
		return "", "", err
	}
	fieldJSON, err := json.Marshal(artifact.FieldSlots)
	if err != nil {
		return "", "", err
	}
	actionJSON, err := json.Marshal(artifact.ActionSlots)
	if err != nil {
		return "", "", err
	}
	return string(fieldJSON), string(actionJSON), nil
}

func ReplaceArtifactMessages(raw, messagesJSON string) (string, error) {
	artifact, err := DecodeDesignArtifact(raw)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(messagesJSON), &artifact.Messages); err != nil {
		return "", fmt.Errorf("agenui workspace: decode replacement messages: %w", err)
	}
	encoded, err := json.Marshal(artifact)
	return string(encoded), err
}
