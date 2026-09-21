// Package processpresentation defines the small, runtime-neutral contract used
// to describe user-facing process stages and activities. It intentionally has
// no dependency on agent registry, runtime, tool gateway, or protocol layers.
package processpresentation

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DetailLevelPrimary   = "primary"
	DetailLevelSecondary = "secondary"
	DetailLevelDebug     = "debug"
)

var stableIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type Stage struct {
	ID    string `json:"id" yaml:"id"`
	Label string `json:"label" yaml:"label"`
	Order int    `json:"order,omitempty" yaml:"order,omitempty"`
}

type Activity struct {
	Key         string `json:"key" yaml:"key"`
	Label       string `json:"label" yaml:"label"`
	DetailLevel string `json:"detail_level,omitempty" yaml:"detail_level,omitempty"`
}

type Metadata struct {
	Stage    Stage    `json:"stage" yaml:"stage"`
	Activity Activity `json:"activity" yaml:"activity"`
}

// StagePresentation is the Agent-authored portion of Metadata. Activities are
// resolved later from the trusted ToolDefinition, so Agent manifests cannot
// override tool identity or labels.
type StagePresentation struct {
	Stage Stage `json:"stage" yaml:"stage"`
}

func (s Stage) Empty() bool {
	return strings.TrimSpace(s.ID) == "" && strings.TrimSpace(s.Label) == "" && s.Order == 0
}

func (a Activity) Empty() bool {
	return strings.TrimSpace(a.Key) == "" && strings.TrimSpace(a.Label) == "" && strings.TrimSpace(a.DetailLevel) == ""
}

func (m Metadata) Empty() bool {
	return m.Stage.Empty() && m.Activity.Empty()
}

func DefaultStage() Stage {
	return Stage{ID: "execution", Label: "执行处理", Order: 500}
}

func ValidateStage(stage Stage) error {
	if stage.Empty() {
		return nil
	}
	if !stableIDPattern.MatchString(strings.TrimSpace(stage.ID)) {
		return fmt.Errorf("stage id must match %s", stableIDPattern.String())
	}
	if err := validateLabel(stage.Label, 80); err != nil {
		return fmt.Errorf("stage label: %w", err)
	}
	if stage.Order < 0 || stage.Order > 1000 {
		return fmt.Errorf("stage order must be from 0 through 1000")
	}
	return nil
}

func ValidateActivity(activity Activity) error {
	if activity.Empty() {
		return nil
	}
	if activity.Key != "" && !stableIDPattern.MatchString(strings.TrimSpace(activity.Key)) {
		return fmt.Errorf("activity key must match %s", stableIDPattern.String())
	}
	if activity.Label != "" {
		if err := validateLabel(activity.Label, 160); err != nil {
			return fmt.Errorf("activity label: %w", err)
		}
	}
	switch activity.DetailLevel {
	case "", DetailLevelPrimary, DetailLevelSecondary, DetailLevelDebug:
	default:
		return fmt.Errorf("activity detail level must be primary, secondary, or debug")
	}
	return nil
}

func validateLabel(value string, maxRunes int) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("is required")
	}
	if utf8.RuneCountInString(trimmed) > maxRunes {
		return fmt.Errorf("must not exceed %d characters", maxRunes)
	}
	if strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return fmt.Errorf("must not contain control characters")
	}
	return nil
}
