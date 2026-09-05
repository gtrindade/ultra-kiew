package diceroller

import (
	"strings"
	"testing"
)

func TestRollProducesAResultInRange(t *testing.T) {
	// 1d1 has exactly one possible outcome, so this asserts on the value
	// rather than on "it did not error".
	got, err := Roll("1d1")
	if err != nil {
		t.Fatalf("Roll failed: %v", err)
	}
	if !strings.Contains(got, "1") {
		t.Errorf("expected a result of 1, got %q", got)
	}
}

func TestRollAppliesAModifier(t *testing.T) {
	got, err := Roll("1d1+4")
	if err != nil {
		t.Fatalf("Roll failed: %v", err)
	}
	if !strings.Contains(got, "5") {
		t.Errorf("expected 1d1+4 to total 5, got %q", got)
	}
}

func TestRollRejectsNonsense(t *testing.T) {
	for _, prompt := range []string{"", "banana", "d", "20"} {
		if _, err := Roll(prompt); err == nil {
			t.Errorf("expected %q to be rejected", prompt)
		}
	}
}

// Documented, not endorsed: the dice library accepts a die with no side count
// instead of rejecting it, and what it rolls for one is not well defined --
// observed answers include both "0 [0]" and "1 [1]" for the same input. So
// "rola 1d" gets a confident, meaningless number rather than "não entendi".
//
// This is the library's call, not something worth working around here, but it
// is pinned so that a dependency upgrade which starts rejecting the input
// shows up in a test rather than in the group chat.
func TestRollAcceptsADieWithNoSideCount(t *testing.T) {
	got, err := Roll("1d")
	if err != nil {
		t.Fatalf("the library currently accepts this: %v", err)
	}
	if got == "" {
		t.Error("expected some rendered result")
	}
}

// The model supplies these args, so every shape it can get wrong has to be
// handled here rather than panicking inside the tool dispatcher.
func TestRollWithArgsRequiresAStringPrompt(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"no prompt at all", map[string]any{}},
		{"prompt is a number", map[string]any{"prompt": 20}},
		{"prompt is nil", map[string]any{"prompt": nil}},
		{"prompt is a list", map[string]any{"prompt": []any{"1d20"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RollWithArgs(tc.args); err == nil {
				t.Fatalf("expected an error for %v", tc.args)
			}
		})
	}
}

func TestRollWithArgsIgnoresTheInjectedContextKeys(t *testing.T) {
	// Every tool call carries the caller's chat context alongside the model's
	// own arguments; a tool that does not use them must simply not trip on
	// their presence.
	got, err := RollWithArgs(map[string]any{
		"prompt":        "1d1",
		"_callerChatID": int64(-100),
		"_chatTitle":    "Shadowrun",
		"_isPrivate":    false,
	})
	if err != nil {
		t.Fatalf("RollWithArgs failed: %v", err)
	}
	if got == "" {
		t.Error("expected a rendered roll result")
	}
}

func TestToolConfigDeclaresThePromptParameter(t *testing.T) {
	tool := GetToolConfig()
	if len(tool.FunctionDeclarations) != 1 {
		t.Fatalf("expected exactly one declaration, got %d", len(tool.FunctionDeclarations))
	}

	decl := tool.FunctionDeclarations[0]
	if decl.Name != RollDice {
		t.Errorf("declared name %q does not match the dispatch key %q", decl.Name, RollDice)
	}
	// The dispatcher looks the function up by name, so a declaration whose
	// name drifts from the constant would register a tool nothing can run.
	if _, ok := decl.Parameters.Properties["prompt"]; !ok {
		t.Errorf("the declaration must expose a prompt parameter, got %v", decl.Parameters.Properties)
	}
	if len(decl.Parameters.Required) != 1 || decl.Parameters.Required[0] != "prompt" {
		t.Errorf("prompt must be required, got %v", decl.Parameters.Required)
	}
}
