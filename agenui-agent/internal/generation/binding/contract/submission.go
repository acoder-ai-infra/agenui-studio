package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ExecutablePlanSchema = "agenui.executable-binding/v1"
	SubmissionSchema     = "agenui.binding-submission/v1"
)

type ExecutablePlan struct {
	SchemaVersion  string           `json:"schema_version"`
	FieldMappings  []map[string]any `json:"field_mappings"`
	ActionMappings []map[string]any `json:"action_mappings"`
}

type Submission struct {
	SchemaVersion string         `json:"schema_version"`
	Result        Result         `json:"result"`
	Plan          ExecutablePlan `json:"plan"`
}

func ParseExecutablePlan(raw string) (ExecutablePlan, error) {
	var plan ExecutablePlan
	if err := decodeExactJSON([]byte(raw), &plan); err != nil || plan.SchemaVersion != ExecutablePlanSchema || plan.FieldMappings == nil || plan.ActionMappings == nil {
		return ExecutablePlan{}, errors.New("invalid executable binding plan")
	}
	return plan, nil
}

func EncodeExecutablePlan(fields, actions []map[string]any) (string, error) {
	// Arrays are part of the executable-plan wire contract. A fully blocked
	// Bind Result legitimately has no mappings; encode that state as [] rather
	// than JSON null so every producer observes the same canonical shape.
	if fields == nil {
		fields = []map[string]any{}
	}
	if actions == nil {
		actions = []map[string]any{}
	}
	encoded, err := json.Marshal(ExecutablePlan{SchemaVersion: ExecutablePlanSchema, FieldMappings: fields, ActionMappings: actions})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func ParseSubmission(raw string) (Submission, error) {
	var submission Submission
	if err := decodeExactJSON([]byte(raw), &submission); err != nil || submission.SchemaVersion != SubmissionSchema || submission.Result.SchemaVersion != ResultSchemaV1 || submission.Plan.SchemaVersion != ExecutablePlanSchema {
		return Submission{}, errors.New("invalid binding submission")
	}
	return submission, nil
}

func EncodeSubmission(result Result, planJSON string) (string, error) {
	plan, err := ParseExecutablePlan(planJSON)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(Submission{SchemaVersion: SubmissionSchema, Result: result, Plan: plan})
	if err != nil {
		return "", fmt.Errorf("encode binding submission: %w", err)
	}
	return string(encoded), nil
}

func decodeExactJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
