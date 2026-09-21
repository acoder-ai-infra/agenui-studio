package harness

import (
	"encoding/json"
)

// ExtractInteractionProposal inspects a canonical Event and, if its payload
// preview or ref describes a proposal-shaped AskUser interaction, extracts
// the InteractionProposal. The bool return distinguishes
// "extracted successfully" from "event does not carry a proposal" so callers
// can decide whether to fall back to the standard ControlRequest UI.
//
// The extractor is intentionally lenient: it accepts a payload with the
// canonical field names (prompt, kind, options, input) plus optional TaskID /
// AttemptID hints. It never returns runtime control credentials
// (control_request_id, checkpoint_id, resume_token, control_ticket); those
// are ignored during unmarshal.
//
// SDK callers can consume proposal-typed payloads without exposing Control or
// Resume credentials.
func ExtractInteractionProposal(ev Event) (*InteractionProposal, bool) {
	if len(ev.PayloadPreview) == 0 {
		return nil, false
	}
	if ev.EventType != EventControlRequestCreated && ev.EventType != EventSubAgentCompleted && ev.EventType != EventSubAgentProgress {
		return nil, false
	}
	var raw struct {
		Prompt    string              `json:"prompt"`
		Kind      string              `json:"kind"`
		Options   []InteractionOption `json:"options,omitempty"`
		Input     interactionInputRaw `json:"input,omitempty"`
		TaskID    string              `json:"task_id,omitempty"`
		AttemptID string              `json:"attempt_id,omitempty"`
	}
	if err := json.Unmarshal(ev.PayloadPreview, &raw); err != nil {
		return nil, false
	}
	if raw.Prompt == "" && len(raw.Options) == 0 {
		return nil, false
	}
	proposal := &InteractionProposal{
		Prompt:    raw.Prompt,
		Kind:      raw.Kind,
		Options:   raw.Options,
		TaskID:    raw.TaskID,
		AttemptID: raw.AttemptID,
		Input: InteractionInputConstraint{
			SchemaRef:     raw.Input.SchemaRef,
			MaxLength:     raw.Input.MaxLength,
			AllowMultiple: raw.Input.AllowMultiple,
			Required:      raw.Input.Required,
		},
	}
	return proposal, true
}

// interactionInputRaw is a private mirror used for decoding InteractionInputConstraint
// without exposing decoding-only fields to the public surface.
type interactionInputRaw struct {
	SchemaRef     string `json:"schema_ref,omitempty"`
	MaxLength     int    `json:"max_length,omitempty"`
	AllowMultiple bool   `json:"allow_multiple,omitempty"`
	Required      bool   `json:"required,omitempty"`
}
