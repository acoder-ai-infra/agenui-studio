package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	PublishNextStepsName   = "agenui_publish_next_steps"
	NextStepsSchemaVersion = "agenui.next_steps.v2"
	NextStepsTitle         = "下一步建议"

	nextStepsMaxItems            = 3
	nextStepsMaxLabelRunes       = 24
	nextStepsMaxDescriptionRunes = 160
	nextStepsMaxPromptRunes      = 160
	// Harness keeps a bounded public process preview. The presentation is the
	// durable display fallback when the full tool result does not fit that
	// preview, so keep its encoded form comfortably below the outer envelope.
	// The completed tool event has a 4 KiB public preview budget. Reserve roughly
	// 1 KiB for canonical tool identity and process metadata while allowing three
	// concrete Chinese descriptions to survive in the durable presentation.
	nextStepsMaxPresentationBytes = 3072
)

var genericNextStepLabels = map[string]struct{}{
	"调整样式":                {},
	"优化样式":                {},
	"调整卡片样式":              {},
	"优化卡片样式":              {},
	"切换数据源":               {},
	"更换数据源":               {},
	"修改展示内容":              {},
	"调整展示内容":              {},
	"继续调整":                {},
	"继续优化":                {},
	"完善界面":                {},
	"优化交互":                {},
	"adjust styles":       {},
	"improve styling":     {},
	"change data source":  {},
	"continue optimizing": {},
}

type nextStepDraft struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
}

type nextStepsDraft struct {
	SchemaVersion string          `json:"schema_version"`
	Items         []nextStepDraft `json:"items"`
}

type nextStepView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt"`
}

type nextStepsView struct {
	SchemaVersion string         `json:"schema_version"`
	PlanID        string         `json:"plan_id"`
	Title         string         `json:"title"`
	Items         []nextStepView `json:"items"`
}

type nextStepsPresentationValue struct {
	Description string `json:"d,omitempty"`
	Prompt      string `json:"p"`
}

// PublishNextSteps turns a model proposal into a small, host-validated UI
// contract. The ordinary tool_call_completed event is the durable record; the
// tool deliberately creates no second event stream and stores no process state.
type PublishNextSteps struct{}

func (*PublishNextSteps) Name() string { return PublishNextStepsName }

func (*PublishNextSteps) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if call.Name != PublishNextStepsName || call.Ctx.AgentID != agenuiextensions.MainAgent || call.Ctx.ParentRunID != "" {
		return nil, errors.New("publish next steps: caller is not allowed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var draft nextStepsDraft
	decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&draft); err != nil {
		return nil, fmt.Errorf("publish next steps: decode: %w", err)
	}
	if err := ensureNextStepsJSONEnd(decoder); err != nil {
		return nil, err
	}
	if draft.SchemaVersion != NextStepsSchemaVersion {
		return nil, fmt.Errorf("publish next steps: schema_version must be %q", NextStepsSchemaVersion)
	}
	if len(draft.Items) == 0 || len(draft.Items) > nextStepsMaxItems {
		return nil, fmt.Errorf("publish next steps: items must contain 1 to %d entries", nextStepsMaxItems)
	}

	normalized := make([]nextStepDraft, 0, len(draft.Items))
	for index, item := range draft.Items {
		item.Label = strings.TrimSpace(item.Label)
		item.Description = strings.TrimSpace(item.Description)
		item.Prompt = strings.TrimSpace(item.Prompt)
		if err := validateNextStepsText("label", index, item.Label, nextStepsMaxLabelRunes, true); err != nil {
			return nil, err
		}
		if _, generic := genericNextStepLabels[strings.ToLower(strings.Join(strings.Fields(item.Label), " "))]; generic {
			return nil, fmt.Errorf("publish next steps: item %d label must name a concrete target and change", index+1)
		}
		if err := validateNextStepsText("description", index, item.Description, nextStepsMaxDescriptionRunes, true); err != nil {
			return nil, err
		}
		if err := validateNextStepsText("prompt", index, item.Prompt, nextStepsMaxPromptRunes, true); err != nil {
			return nil, err
		}
		normalized = append(normalized, item)
	}

	canonical, err := json.Marshal(nextStepsDraft{SchemaVersion: NextStepsSchemaVersion, Items: normalized})
	if err != nil {
		return nil, fmt.Errorf("publish next steps: canonicalize: %w", err)
	}
	identity := strings.Join([]string{
		call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
		call.Ctx.RunID, call.Ctx.StepID, string(canonical),
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	planID := "next_" + hex.EncodeToString(sum[:12])

	items := make([]nextStepView, 0, len(normalized))
	for index, item := range normalized {
		items = append(items, nextStepView{
			ID:          planID + "_" + strconv.Itoa(index+1),
			Label:       item.Label,
			Description: item.Description,
			Prompt:      item.Prompt,
		})
	}
	encoded, err := json.Marshal(nextStepsView{
		SchemaVersion: NextStepsSchemaVersion,
		PlanID:        planID,
		Title:         NextStepsTitle,
		Items:         items,
	})
	if err != nil {
		return nil, fmt.Errorf("publish next steps: encode: %w", err)
	}
	presentation, err := buildNextStepsPresentation(planID, items)
	if err != nil {
		return nil, err
	}
	return &extension.FunctionResult{
		Data: encoded, MimeType: "application/json",
		Presentation: presentation,
	}, nil
}

func buildNextStepsPresentation(planID string, items []nextStepView) (*extension.ResultPresentation, error) {
	build := func(compact bool) (*extension.ResultPresentation, error) {
		details := make([]extension.ResultPresentationDetail, 0, len(items))
		for _, item := range items {
			prompt := item.Prompt
			if compact {
				// The label is already a complete action. It is a safe prefill
				// fallback when the richer prompt would exceed the public event.
				prompt = item.Label
			}
			value, err := json.Marshal(nextStepsPresentationValue{
				Description: item.Description,
				Prompt:      prompt,
			})
			if err != nil {
				return nil, fmt.Errorf("publish next steps: encode presentation item: %w", err)
			}
			details = append(details, extension.ResultPresentationDetail{
				Label: item.Label,
				Value: string(value),
			})
		}
		return &extension.ResultPresentation{
			Title:   NextStepsTitle,
			Summary: NextStepsSchemaVersion + ":" + planID,
			Details: details,
		}, nil
	}
	presentation, err := build(false)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeNextStepsPresentation(presentation)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= nextStepsMaxPresentationBytes {
		return presentation, nil
	}

	// Keep descriptions intact in both the authoritative output and durable UI
	// fallback. Only the prefill prompt is compacted when the event is too large.
	presentation, err = build(true)
	if err != nil {
		return nil, err
	}
	encoded, err = encodeNextStepsPresentation(presentation)
	if err != nil {
		return nil, err
	}
	if len(encoded) > nextStepsMaxPresentationBytes {
		return nil, fmt.Errorf("publish next steps: plan is too verbose for the user-facing event (%d/%d bytes); shorten descriptions", len(encoded), nextStepsMaxPresentationBytes)
	}
	return presentation, nil
}

func encodeNextStepsPresentation(presentation *extension.ResultPresentation) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		Title   string                               `json:"title"`
		Summary string                               `json:"summary"`
		Details []extension.ResultPresentationDetail `json:"details"`
	}{Title: presentation.Title, Summary: presentation.Summary, Details: presentation.Details})
	if err != nil {
		return nil, fmt.Errorf("publish next steps: encode presentation: %w", err)
	}
	return encoded, nil
}

func ensureNextStepsJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("publish next steps: decode trailing data: %w", err)
	}
	return errors.New("publish next steps: multiple JSON values are not allowed")
}

func validateNextStepsText(field string, index int, value string, maxRunes int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("publish next steps: item %d %s is required", index+1, field)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("publish next steps: item %d %s exceeds %d characters", index+1, field, maxRunes)
	}
	return nil
}
