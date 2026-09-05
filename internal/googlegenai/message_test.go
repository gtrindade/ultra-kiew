package googlegenai

import (
	"fmt"
	"testing"

	"google.golang.org/genai"
)

func textResponse(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
			FinishReason: genai.FinishReasonStop,
		}},
	}
}

// TestHasUsableContentRejectsEmptyReplies covers the shapes an empty Gemini
// reply actually arrives in. Each of these used to reach the user as "I broke
// the chat history"; all of them should now be retried instead.
func TestHasUsableContentRejectsEmptyReplies(t *testing.T) {
	cases := []struct {
		name   string
		result *genai.GenerateContentResponse
		want   bool
	}{
		{"nil response", nil, false},
		{"no candidates", &genai.GenerateContentResponse{}, false},
		{
			"candidate with nil content",
			&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{}}},
			false,
		},
		{
			"candidate with no parts",
			&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{}},
			}}},
			false,
		},
		{
			// A thinking model that spent its whole budget on thoughts: valid
			// content as far as the SDK is concerned, but Text() skips thought
			// parts, so there is nothing to send to the user.
			"thought-only response",
			&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "hmm...", Thought: true}}},
				FinishReason: genai.FinishReasonMaxTokens,
			}}},
			false,
		},
		{"whitespace only", textResponse("   \n  "), false},
		{"real text", textResponse("Bom dia!"), true},
		{
			"function call with no text",
			&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{Name: "event_manage"},
				}}},
			}}},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUsableContent(tc.result); got != tc.want {
				t.Errorf("hasUsableContent = %v, want %v", got, tc.want)
			}
		})
	}
}

// A deliberate refusal must be distinguishable from a failure, because only
// one of the two is worth retrying.
func TestRefusalReasonOnlyFiresForDeliberateRefusals(t *testing.T) {
	blocked := func(reason genai.FinishReason) *genai.GenerateContentResponse {
		return &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{{FinishReason: reason}},
		}
	}

	for _, reason := range []genai.FinishReason{
		genai.FinishReasonSafety,
		genai.FinishReasonProhibitedContent,
		genai.FinishReasonBlocklist,
		genai.FinishReasonSPII,
		genai.FinishReasonRecitation,
		genai.FinishReasonMalformedFunctionCall,
	} {
		if refusalReason(blocked(reason)) == "" {
			t.Errorf("finish reason %q should be reported as a refusal, not retried", reason)
		}
	}

	// These are failures or successes, not refusals: retrying is the right
	// move, so they must not be reported as a decision the model made.
	for _, reason := range []genai.FinishReason{
		genai.FinishReasonUnspecified,
		genai.FinishReasonStop,
		genai.FinishReasonMaxTokens,
	} {
		if got := refusalReason(blocked(reason)); got != "" {
			t.Errorf("finish reason %q should be retriable, got refusal %q", reason, got)
		}
	}

	if refusalReason(nil) != "" {
		t.Error("a nil response is a failure, not a refusal")
	}
	if refusalReason(&genai.GenerateContentResponse{}) != "" {
		t.Error("a response with no candidates is a failure, not a refusal")
	}

	promptBlocked := &genai.GenerateContentResponse{
		PromptFeedback: &genai.GenerateContentResponsePromptFeedback{
			BlockReason: genai.BlockedReasonSafety,
		},
	}
	if refusalReason(promptBlocked) == "" {
		t.Error("a blocked prompt should be reported rather than retried")
	}
}

// The history shape that bricks a chat: a model turn whose function call was
// never answered. Recognising it is what stops one empty reply from poisoning
// every subsequent turn.
func TestHasUnansweredToolCall(t *testing.T) {
	userTurn := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "cria um evento"}}}
	toolCallTurn := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{Name: "event_manage"},
	}}}
	toolResponseTurn := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{Name: "event_manage"},
	}}}
	modelTextTurn := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "Feito!"}}}

	cases := []struct {
		name    string
		history []*genai.Content
		want    bool
	}{
		{"empty history", nil, false},
		{"plain conversation", []*genai.Content{userTurn, modelTextTurn}, false},
		{
			"completed tool round",
			[]*genai.Content{userTurn, toolCallTurn, toolResponseTurn, modelTextTurn},
			false,
		},
		{
			// Exactly what the SDK leaves behind when the reply to a
			// functionResponse comes back empty.
			"dangling tool call",
			[]*genai.Content{userTurn, toolCallTurn},
			true,
		},
		{
			"tool response present but unanswered",
			[]*genai.Content{userTurn, toolCallTurn, toolResponseTurn},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUnansweredToolCall(tc.history); got != tc.want {
				t.Errorf("hasUnansweredToolCall = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsTransientAPIError(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{429, true},
		{500, true},
		{503, true},
		{400, false},
		{403, false},
		{404, false},
	}

	for _, tc := range cases {
		err := genai.APIError{Code: tc.code, Message: "test"}
		if got := isTransientAPIError(err); got != tc.want {
			t.Errorf("code %d: isTransientAPIError = %v, want %v", tc.code, got, tc.want)
		}
	}

	if isTransientAPIError(fmt.Errorf("some non-api error")) {
		t.Error("a non-API error should not be treated as transient")
	}
}

func TestScrubResponse(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"simple text",
			"Olá pessoal!",
			"Olá pessoal!",
		},
		{
			"enclosed in response tags",
			"<response>A sessão começa em 1 hora! @user</response>",
			"A sessão começa em 1 hora! @user",
		},
		{
			"case insensitive response tags with newlines",
			"<RESPONSE>\nA sessão vai começar agora!\n</Response>",
			"A sessão vai começar agora!",
		},
		{
			"leaked lines and response tags",
			"<response>[2026-04-09T07:02:35-04:00 - user]: `oi`\nAviso da sessão!</response>",
			"Aviso da sessão!",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrubResponse(tc.input); got != tc.want {
				t.Errorf("scrubResponse(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
