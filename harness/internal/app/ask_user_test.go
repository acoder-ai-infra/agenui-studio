package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestAskUserResumeNormalizesControlAnswerToToolOutput(t *testing.T) {
	handler := askUserFunctionHandler()
	result, err := handler(context.Background(), toolgateway.FunctionCall{
		Arguments: json.RawMessage(`{"questions":[{"header":"Card type","question":"Choose a card type","options":[{"label":"Product","description":"A product card"},{"label":"Article","description":"An article card"}]}]}`),
		Resume: &toolgateway.ToolCallResume{
			WasInterrupted: true,
			IsResumeTarget: true,
			Payload:        json.RawMessage(`{"answers":[{"question_index":0,"question_id":"q0","selected_option":{"id":"q0_opt0","label":"Product"}}]}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Response struct {
			Answers []askUserNormalizedAnswer `json:"answers"`
		} `json:"response"`
	}
	if err := json.Unmarshal(result.Data, &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Response.Answers) != 1 {
		t.Fatalf("answers=%#v", output.Response.Answers)
	}
	answer := output.Response.Answers[0]
	if answer.QuestionID != "q0" || answer.Question != "Choose a card type" || answer.OptionID != "q0_opt0" || answer.Answer != "Product" {
		t.Fatalf("answer=%#v", answer)
	}
}

func TestAskUserResumeRejectsUnknownQuestion(t *testing.T) {
	handler := askUserFunctionHandler()
	_, err := handler(context.Background(), toolgateway.FunctionCall{
		Arguments: json.RawMessage(`{"questions":[{"question":"Choose","options":[]}]}`),
		Resume: &toolgateway.ToolCallResume{
			WasInterrupted: true,
			IsResumeTarget: true,
			Payload:        json.RawMessage(`{"answers":[{"question_index":2,"text":"Other"}]}`),
		},
	})
	if err == nil || !toolgateway.IsErrorType(err, toolgateway.ErrorTypeSchemaValidationFailed) {
		t.Fatalf("error=%v", err)
	}
}
