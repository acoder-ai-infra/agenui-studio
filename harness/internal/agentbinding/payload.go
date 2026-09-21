package agentbinding

import "errors"

type SuccessPayload struct {
	SchemaVersion string           `json:"schema_version"`
	Binding       EffectiveBinding `json:"binding"`
}

type FailurePayload struct {
	SchemaVersion    string                `json:"schema_version"`
	BindingID        string                `json:"binding_id,omitempty"`
	SessionID        string                `json:"session_id,omitempty"`
	RunID            string                `json:"run_id,omitempty"`
	RequestSelection *Selection            `json:"request_selection,omitempty"`
	ControlRuleRef   string                `json:"control_rule_ref,omitempty"`
	ControlRevision  string                `json:"control_rule_revision,omitempty"`
	Stage            FailureStage          `json:"failure_stage"`
	Code             ErrorCode             `json:"code"`
	SafeMessage      string                `json:"safe_message"`
	Retryable        bool                  `json:"retryable"`
	Clarification    *BindingClarification `json:"binding_clarification,omitempty"`
}

func NewSuccessPayload(result Result) SuccessPayload {
	binding := result.Binding
	binding.CapabilitySnapshotRefs = append([]string(nil), result.Binding.CapabilitySnapshotRefs...)
	return SuccessPayload{SchemaVersion: BindingSchemaVersion, Binding: binding}
}

func NewFailurePayload(req BindingRequest, err error) FailurePayload {
	payload := FailurePayload{
		SchemaVersion: BindingSchemaVersion,
		BindingID:     req.BindingID,
		SessionID:     req.SessionID,
		RunID:         req.RunID,
		Stage:         StageOf(err),
		Code:          CodeOf(err),
		SafeMessage:   SafeMessageOf(err),
		Retryable:     RetryableOf(err),
	}
	if req.Request != nil {
		selection := *req.Request
		payload.RequestSelection = &selection
	}
	if req.Control != nil {
		payload.ControlRuleRef = req.Control.Ref
		payload.ControlRevision = req.Control.Revision
	}
	var askUser *AskUserError
	if errors.As(err, &askUser) {
		clarification := askUser.Clarification
		clarification.MissingFields = append([]string(nil), askUser.Clarification.MissingFields...)
		payload.Clarification = &clarification
	}
	return payload
}
