package googlegenai

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"google.golang.org/genai"
)

const MaxFunctionResponseLength = 10000

// Keys injected into every tool call by the code. The model can also emit keys
// with these names -- we always overwrite them after the model's args are read,
// so a model-supplied value can never win.
const (
	ArgCallerChatID = "_callerChatID"
	ArgChatTitle    = "_chatTitle"
	ArgIsPrivate    = "_isPrivate"
)

// leakedLineRegex matches the shape of the context lines we feed the model:
// "[2026-04-09T07:02:35-04:00 - guilhermetmg]: `text`". leakedSuffixRegex
// matches the old ". The chatID is -100... The chat title is "..."" suffix.
//
// This is belt-and-braces, not the fix. The fix is that the model is no longer
// shown a bare line-oriented transcript it can continue -- see buildPrompt. It
// stayed in because the failure mode was so bad: when the model got confused it
// kept completing the transcript and invented messages from real users
// (bmaraujo was quoted calling the bot useless; he never said it), which went
// out to the group looking like a real quote. Anything shaped like our own
// framing is never something we want to relay.
var (
	leakedLineRegex   = regexp.MustCompile(`(?m)^\s*\[\d{4}-\d{2}-\d{2}T[^\]]*\][^\n:]*:\s*.*$`)
	leakedSuffixRegex = regexp.MustCompile(`\.?\s*The chatID is -?\d+\.?(\s*The chat title is "[^"]*"\.?)?`)
	systemNoteRegex   = regexp.MustCompile(`(?m)^\s*\[System Note:.*$`)
	blankRunRegex     = regexp.MustCompile(`\n{3,}`)
)

// scrubResponse removes any of our own prompt scaffolding the model echoed back.
func scrubResponse(text string) string {
	text = leakedLineRegex.ReplaceAllString(text, "")
	text = leakedSuffixRegex.ReplaceAllString(text, "")
	text = systemNoteRegex.ReplaceAllString(text, "")
	text = blankRunRegex.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// buildPrompt wraps the conversation context in explicit delimiters and states
// plainly which part the model is answering.
//
// The previous format was the raw transcript followed by
// ". The chatID is %d. The chat title is %q" -- one flat run of text with no
// boundary between "things people said" and "your instructions". Two things
// went wrong with it, both seen in testing:
//
//   - The model continued the pattern instead of responding to it, emitting new
//     "[timestamp - user]: `...`" lines it had made up.
//   - The chatID sat in the prose, so the model treated it as a fact to discuss
//     with users ("me informe o ID correto do grupo") and as something it could
//     get wrong. It is now never in the prompt at all; tools receive it from the
//     code (see ArgCallerChatID).
//
// The current time is included because otherwise the model's only clue about
// "today" is the timestamps in the transcript -- and once the history has been
// consumed there are none, so "amanhã às 21h" had nothing to anchor to.
//
// It is exported because the Telegram layer owns the transcript, but the prompt
// shape is decided here, in one place.
func BuildPrompt(history, current, systemNote string) string {
	var sb strings.Builder

	sb.WriteString("<current_time>")
	sb.WriteString(time.Now().Format(time.RFC3339))
	sb.WriteString("</current_time>\n\n")

	if strings.TrimSpace(history) != "" {
		sb.WriteString("<conversation_context>\n")
		sb.WriteString("Earlier messages in this chat, for background only. They are a\n")
		sb.WriteString("record of what other people said. Never continue, quote, reproduce\n")
		sb.WriteString("or imitate this format in your reply.\n")
		sb.WriteString(history)
		sb.WriteString("\n</conversation_context>\n\n")
	}

	if strings.TrimSpace(systemNote) != "" {
		sb.WriteString("<system_note>\n")
		sb.WriteString(strings.TrimSpace(systemNote))
		sb.WriteString("\n</system_note>\n\n")
	}

	sb.WriteString("<message_to_answer>\n")
	sb.WriteString(strings.TrimSpace(current))
	sb.WriteString("\n</message_to_answer>")

	return sb.String()
}

// SendMessageWithParts sends a message with multiple parts to the chat and returns the response text.
func (c *Client) SendMessageWithParts(ctx context.Context, chatID int64, parts []*genai.Part) (string, error) {
	chat, err := c.GetChat(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("failed to create new chat: %w", err)
	}
	result, err := chat.Send(ctx, parts...)
	if err != nil {
		return "", err
	}

	return result.Text(), nil
}

// SendMessage runs one conversational turn for a chat, resolving any tool calls
// the model makes along the way.
//
// chatID and chatTitle are *not* shown to the model. They are attached to every
// tool call as ArgCallerChatID / ArgChatTitle, so a tool always acts on the chat
// the message actually came from. Letting the model carry the chat ID through
// the conversation was the single largest source of failures in testing: it
// asked users to supply the ID by hand, claimed the ID was invalid, and
// "registered" a group against an ID it had made up.
func (c *Client) SendMessage(ctx context.Context, chatID int64, chatTitle string, text string) (string, error) {
	// Telegram user IDs are positive and group/supergroup IDs are negative, so
	// the sign of the chat ID is what tells a DM from a group. This used to be
	// re-derived inside each tool from a value the model supplied; it is
	// decided once, here, from the real update.
	return c.sendTurn(ctx, chatID, chatTitle, text, chatID > 0)
}

func (c *Client) sendTurn(ctx context.Context, chatID int64, chatTitle, text string, isPrivate bool) (string, error) {
	chat, err := c.GetChat(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("failed to create new chat: %w", err)
	}

	if err := c.checkChatHistory(chatID); err != nil {
		return "", err
	}

	parts := []*genai.Part{genai.NewPartFromText(text)}
	result, err := chat.Send(ctx, parts...)
	if err != nil {
		return "", fmt.Errorf("failed to send message: %w", err)
	}

	// Bound the tool loop. Without this a model that keeps re-calling a failing
	// tool spins until the API rate-limits it, holding the user's turn open.
	const maxToolRounds = 8
	for round := 0; round < maxToolRounds; round++ {
		functionCalls := result.FunctionCalls()
		if len(functionCalls) == 0 {
			break
		}

		// Gemini requires exactly one functionResponse per functionCall in the
		// turn. Emitting a bare text part for an unknown tool (which is what
		// this did) leaves the counts mismatched and corrupts the session
		// history -- which is what checkChatHistory was papering over.
		response := make([]*genai.Part, 0, len(functionCalls))
		for _, call := range functionCalls {
			response = append(response, c.runToolCall(call, chatID, chatTitle, isPrivate))
		}

		result, err = chat.Send(ctx, response...)
		if err != nil {
			return "", fmt.Errorf("failed to send function response: %w", err)
		}
	}

	responseText := scrubResponse(result.Text())

	if strings.Contains(responseText, "__SILENT__") {
		return "", nil
	}

	if responseText == "" {
		if err := c.checkChatHistory(chatID); err != nil {
			return err.Error(), nil
		}
		return "", nil
	}

	return responseText, nil
}

// runToolCall dispatches one function call and always returns a functionResponse
// part for it, whatever happens.
func (c *Client) runToolCall(call *genai.FunctionCall, chatID int64, chatTitle string, isPrivate bool) *genai.Part {
	toolConfig, exists := c.toolConfigs[call.Name]
	if !exists {
		return genai.NewPartFromFunctionResponse(call.Name, map[string]any{
			"error": fmt.Sprintf("Tool %q does not exist. Do not try to call it again.", call.Name),
		})
	}

	if call.Args == nil {
		call.Args = make(map[string]any)
	}
	// Assigned after the model's args are in the map, so these always win over
	// anything the model tried to pass under the same names.
	call.Args[ArgChatTitle] = chatTitle
	call.Args[ArgCallerChatID] = chatID
	call.Args[ArgIsPrivate] = isPrivate

	functionResult, err := toolConfig.Function(call.Args)
	if err != nil {
		return genai.NewPartFromFunctionResponse(call.Name, map[string]any{
			"error": fmt.Sprintf("%v", err),
		})
	}

	if len(functionResult) > MaxFunctionResponseLength {
		fmt.Printf("Function result too long (%d characters), truncating\n", len(functionResult))
		functionResult = functionResult[:MaxFunctionResponseLength] + "...(truncated)"
	}

	return genai.NewPartFromFunctionResponse(call.Name, map[string]any{
		"result": functionResult,
	})
}

func (c *Client) checkChatHistory(chatID int64) error {
	c.chatsLock.RLock()
	chat, exists := c.chats[chatID]
	c.chatsLock.RUnlock()
	if !exists {
		return fmt.Errorf("chat with ID %d does not exist", chatID)
	}

	history := chat.History(false)
	for _, content := range history {
		if content != nil && len(content.Parts) > 0 {
			continue
		}

		if _, err := c.NewChat(context.Background(), chatID); err != nil {
			return fmt.Errorf("failed to recover chat session: %w", err)
		}
		return fmt.Errorf("Due to a known issue, I failed to generate a response and broke the chat history. I had to start a new session. Please try again.\n\nThe issue: https://discuss.ai.google.dev/t/empty-response-text-from-gemini-2-5-pro-despite-no-safety-and-max-tokens-issues/98010/23")
	}

	return nil
}
