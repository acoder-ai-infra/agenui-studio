package agentregistry

import (
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

func TestProcessPresentationStageValidatesAndThreadsIntoDefinition(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.ProcessPresentation = processpresentation.StagePresentation{Stage: processpresentation.Stage{
		ID: "ui_generation", Label: "生成界面", Order: 20,
	}}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatalf("valid process presentation: %v", err)
	}
	if got := cfg.ToAgentDefinition().ProcessPresentation.Stage; got != cfg.ProcessPresentation.Stage {
		t.Fatalf("process presentation was not compiled: got=%#v want=%#v", got, cfg.ProcessPresentation.Stage)
	}

	invalid := cfg
	invalid.ProcessPresentation.Stage.ID = "UI Generation"
	if err := ValidateAgentConfig(invalid); err == nil {
		t.Fatal("invalid process stage id must fail closed")
	}
}
