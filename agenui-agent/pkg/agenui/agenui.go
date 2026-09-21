// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package agenui provides the Go SDK for AGenUI (Agent-to-UI) protocol.
package agenui

// Version is the current version of the AGenUI Go SDK.
const Version = "0.2.2"

// SystemPromptOptions holds the parameters for generating a system prompt.
type SystemPromptOptions struct {
	// RoleDescription describes the agent's role.
	RoleDescription string
	// WorkflowDescription describes the workflow.
	WorkflowDescription string
	// UIDescription describes the UI.
	UIDescription string
	// ClientUICapabilities are capabilities reported by the client for targeted schema pruning.
	ClientUICapabilities map[string]any
	// AllowedComponents is the list of allowed catalog components.
	AllowedComponents []string
	// AllowedMessages is the list of allowed messages.
	AllowedMessages []string
	// IncludeSchema controls whether to include the schema.
	IncludeSchema bool
	// IncludeExamples controls whether to include examples.
	IncludeExamples bool
	// ValidateExamples controls whether to validate examples.
	ValidateExamples bool
}

// InferenceStrategy generates system prompts for all LLM requests.
type InferenceStrategy interface {
	// GenerateSystemPrompt generates a system prompt based on the given options.
	GenerateSystemPrompt(opts SystemPromptOptions) string
}
