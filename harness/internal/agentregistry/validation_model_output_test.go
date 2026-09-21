package agentregistry

import (
	"testing"
)

// validation_model_output_test.go 覆盖 G-A/G-B 的 model 块校验与
// ToAgentDefinition 透传：response_format 与 image_detail。

func TestValidateResponseFormatAndImageDetail(t *testing.T) {
	valid := []struct {
		format string
		detail string
	}{
		{"", ""},
		{"json_object", "low"},
		{"", "high"},
		{"json_object", "auto"},
	}
	for _, tc := range valid {
		cfg := extensionsBaseConfig()
		cfg.Model.ResponseFormat = tc.format
		cfg.Model.ImageDetail = tc.detail
		if err := ValidateAgentConfig(cfg); err != nil {
			t.Fatalf("response_format=%q image_detail=%q must pass: %v", tc.format, tc.detail, err)
		}
	}

	// json_schema 尚未开放，与其他非法值一并 fail closed。
	for _, bad := range []string{"json_schema", "text", "yaml"} {
		cfg := extensionsBaseConfig()
		cfg.Model.ResponseFormat = bad
		if err := ValidateAgentConfig(cfg); err == nil {
			t.Fatalf("response_format=%q must fail closed", bad)
		}
	}
	for _, bad := range []string{"lowest", "medium", "true"} {
		cfg := extensionsBaseConfig()
		cfg.Model.ImageDetail = bad
		if err := ValidateAgentConfig(cfg); err == nil {
			t.Fatalf("image_detail=%q must fail closed", bad)
		}
	}
}

func TestModelOutputOptionsThreadIntoDefinition(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.Model.ResponseFormat = "json_object"
	cfg.Model.ImageDetail = "low"
	def := cfg.ToAgentDefinition()
	if def.ModelOptions.ResponseFormat != "json_object" {
		t.Fatalf("response_format must thread into definition, got %q", def.ModelOptions.ResponseFormat)
	}
	if def.ModelOptions.ImageDetail != "low" {
		t.Fatalf("image_detail must thread into definition, got %q", def.ModelOptions.ImageDetail)
	}
}
