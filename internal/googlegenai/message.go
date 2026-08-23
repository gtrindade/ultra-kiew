package googlegenai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"google.golang.org/genai"
)

const MaxFunctionResponseLength = 10000

// Retry policy for a single exchange with the model.
//
// An empty reply from Gemini is a known transient fault, not an answer: the
// model hands back a candidate with nothing usable in it, most often right
// after a tool round. Asking again with the identical input usually just
// works, so it is retried here instead of being reported to the user.
const (
	maxSendAttempts = 3
	sendRetryDelay  = 700 * time.Millisecond
)

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
	responseTagRegex  = regexp.MustCompile(`(?i)</?response>`)
	leakedLineRegex   = regexp.MustCompile(`(?m)^\s*\[\d{4}-\d{2}-\d{2}T[^\]]*\][^\n:]*:\s*.*$`)
	leakedSuffixRegex = regexp.MustCompile(`\.?\s*The chatID is -?\d+\.?(\s*The chat title is "[^"]*"\.?)?`)
	systemNoteRegex   = regexp.MustCompile(`(?m)^\s*\[System Note:.*$`)
	blankRunRegex     = regexp.MustCompile(`\n{3,}`)
)

// scrubResponse removes any of our own prompt scaffolding the model echoed back.
func scrubResponse(text string) string {
	text = responseTagRegex.ReplaceAllString(text, "")
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
	result, err := c.sendWithRetry(ctx, chat, chatID, parts...)
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
	// A session poisoned by an earlier failed tool round answers every turn
	// with nothing until it is rebuilt, so it is repaired before the turn
	// rather than after it goes wrong again. This no-ops when there is no
	// session yet, or nothing wrong with it.
	if err := c.repairSession(ctx, chatID, nil); err != nil {
		log.Printf("chat %d: %v", chatID, err)
	}

	chat, err := c.GetChat(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("failed to create new chat: %w", err)
	}

	parts := []*genai.Part{genai.NewPartFromText(text)}
	result, err := c.sendWithRetry(ctx, chat, chatID, parts...)
	if err != nil {
		return "", fmt.Errorf("failed to send message: %w", err)
	}

	// Bound the tool loop. Without this a model that keeps re-calling a failing
	// tool spins until the API rate-limits it, holding the user's turn open.
	const maxToolRounds = 8
	toolRoundsUsed := 0
	var lastToolResponses []*genai.Part
	for round := 0; round < maxToolRounds; round++ {
		functionCalls := result.FunctionCalls()
		if len(functionCalls) == 0 {
			break
		}
		toolRoundsUsed = round + 1

		// Gemini requires exactly one functionResponse per functionCall in the
		// turn. Emitting a bare text part for an unknown tool (which is what
		// this did) leaves the counts mismatched and corrupts the session
		// history.
		response := make([]*genai.Part, 0, len(functionCalls))
		for _, call := range functionCalls {
			response = append(response, c.runToolCall(call, chatID, chatTitle, isPrivate))
		}

		// Retrying happens per-exchange, never per-turn, and this is why: the
		// tool calls above have already run and had their real-world effects
		// (an event card posted, DMs sent). Replaying the whole turn to
		// recover from an empty reply would run them a second time. Retrying
		// only the exchange that came back empty leaves those effects alone.
		lastToolResponses = response
		result, err = c.sendWithRetry(ctx, chat, chatID, response...)
		if err != nil {
			return "", fmt.Errorf("failed to send function response: %w", err)
		}
	}

	if toolRoundsUsed == maxToolRounds && len(result.FunctionCalls()) > 0 {
		log.Printf("chat %d: gave up after %d tool rounds with the model still calling tools", chatID, maxToolRounds)
	}

	// The tool ran and its response never made it into the transmitted
	// history, so put it back before the next turn inherits a malformed
	// conversation. Doing this here, rather than on the next turn's
	// defensive pass, means the responses are still in hand and the pair can
	// be completed instead of discarded.
	if !hasUsableContent(result) && len(lastToolResponses) > 0 {
		if err := c.repairSession(ctx, chatID, lastToolResponses); err != nil {
			log.Printf("chat %d: %v", chatID, err)
		}
	}

	responseText := scrubResponse(result.Text())

	if strings.Contains(responseText, "__SILENT__") {
		return "", nil
	}

	if responseText == "" {
		// Everything retriable has already been retried by this point, so
		// whatever is left is worth saying out loud rather than answering a
		// direct question with silence.
		if reason := refusalReason(result); reason != "" {
			log.Printf("chat %d: model declined to answer (%s)", chatID, reason)
			return fmt.Sprintf("Não consegui responder isso: %s.", reason), nil
		}
		log.Printf("chat %d: model returned nothing usable after %d attempts", chatID, maxSendAttempts)
		return "Não consegui gerar uma resposta agora. Pode tentar de novo?", nil
	}

	return responseText, nil
}

// sendWithRetry performs one exchange with the model, retrying when it comes
// back with nothing usable or fails transiently.
//
// Retrying the identical input is safe with respect to the session: on an API
// error the SDK records no history at all, and on an invalid (empty) response
// it records the failed turn only in the *comprehensive* history, which is
// bookkeeping -- the curated history, which is what actually gets transmitted
// on the next request, never contains it. So a retry re-sends exactly the same
// conversation the first attempt did.
func (c *Client) sendWithRetry(ctx context.Context, chat *genai.Chat, chatID int64, parts ...*genai.Part) (*genai.GenerateContentResponse, error) {
	var lastResult *genai.GenerateContentResponse

	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		result, err := chat.Send(ctx, parts...)
		if err != nil {
			if attempt < maxSendAttempts && isTransientAPIError(err) {
				log.Printf("chat %d: transient API error on attempt %d/%d, retrying: %v", chatID, attempt, maxSendAttempts, err)
				if !sleepOrDone(ctx, sendRetryDelay*time.Duration(attempt)) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, err
		}

		lastResult = result

		if hasUsableContent(result) {
			return result, nil
		}

		// A deliberate refusal is deterministic: asking again spends quota to
		// be refused again. Hand it back so the caller can say what happened.
		if refusalReason(result) != "" {
			return result, nil
		}

		if attempt < maxSendAttempts {
			log.Printf("chat %d: empty response on attempt %d/%d (%s), retrying",
				chatID, attempt, maxSendAttempts, describeEmptyResponse(result))
			if !sleepOrDone(ctx, sendRetryDelay*time.Duration(attempt)) {
				return nil, ctx.Err()
			}
		} else {
			log.Printf("chat %d: empty response on final attempt %d/%d (%s)",
				chatID, attempt, maxSendAttempts, describeEmptyResponse(result))
		}
	}

	// Out of attempts. This is not an error: the caller decides what an empty
	// turn means, and for a tool round it may legitimately be nothing at all.
	return lastResult, nil
}

// hasUsableContent reports whether a response carries anything worth acting
// on: a tool call to run, or text to send.
//
// A response can be structurally valid and still useless here. A thinking
// model that spends its whole output budget on thoughts returns Content whose
// only parts are marked Thought, and Text() skips those, so it reads as empty.
// That case and the no-parts-at-all case both warrant the same treatment --
// ask again -- so they are deliberately not distinguished.
func hasUsableContent(result *genai.GenerateContentResponse) bool {
	if result == nil {
		return false
	}
	if len(result.FunctionCalls()) > 0 {
		return true
	}
	return strings.TrimSpace(result.Text()) != ""
}

// refusalReason explains why the model deliberately declined, or returns ""
// when it simply failed to produce anything.
//
// This is the line between "retry" and "report": a content block is a decision
// the model will make again given the same input, while an empty response is a
// coin flip worth re-tossing.
func refusalReason(result *genai.GenerateContentResponse) string {
	if result == nil {
		return ""
	}
	if result.PromptFeedback != nil && result.PromptFeedback.BlockReason != "" {
		return "o pedido foi bloqueado pelo filtro de conteúdo"
	}
	if len(result.Candidates) == 0 {
		return ""
	}
	switch result.Candidates[0].FinishReason {
	case genai.FinishReasonSafety, genai.FinishReasonProhibitedContent,
		genai.FinishReasonBlocklist, genai.FinishReasonSPII:
		return "a resposta foi bloqueada pelo filtro de conteúdo"
	case genai.FinishReasonRecitation:
		return "a resposta foi bloqueada por citar conteúdo protegido"
	case genai.FinishReasonMalformedFunctionCall:
		return "o modelo tentou usar uma ferramenta de um jeito inválido"
	}
	return ""
}

// isTransientAPIError reports whether an API error is worth another attempt:
// rate limiting and server-side faults are, a malformed request is not.
func isTransientAPIError(err error) bool {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 429 || apiErr.Code >= 500
	}
	return false
}

// sleepOrDone waits for d, reporting false if the context was cancelled first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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

// hasUnansweredToolCall reports whether a history ends with a model turn whose
// function calls were never answered.
//
// This is the shape that quietly bricks a chat. Gemini requires every
// functionCall to be followed by its functionResponse, and the SDK only adds a
// turn to the curated history -- the one it actually transmits -- when the
// model's reply to it was valid. So an empty reply to a functionResponse drops
// that response from history while leaving the functionCall that provoked it
// in place. Every later turn then ships a malformed conversation, and the
// model answers with nothing, every single time, until the session is rebuilt.
func hasUnansweredToolCall(history []*genai.Content) bool {
	if len(history) == 0 {
		return false
	}
	last := history[len(history)-1]
	if last == nil || last.Role != genai.RoleModel {
		return false
	}
	for _, part := range last.Parts {
		if part != nil && part.FunctionCall != nil {
			return true
		}
	}
	return false
}

// repairSession rebuilds a chat whose history ends with an unanswered tool
// call, so the next turn starts from a conversation the API will accept.
//
// pending is the functionResponse we were unable to get answered, when we
// still have it: replaying it completes the call/response pair and keeps the
// record that the tool actually ran. Without it there is nothing to pair the
// call with, so the dangling turn is dropped instead -- losing one exchange,
// which beats losing the session.
//
// Note this repairs rather than resets. The predecessor of this code threw the
// entire conversation away on any hint of trouble, which did unbrick the chat
// but cost every earlier message with it.
func (c *Client) repairSession(ctx context.Context, chatID int64, pending []*genai.Part) error {
	c.chatsLock.RLock()
	chat, exists := c.chats[chatID]
	c.chatsLock.RUnlock()
	if !exists {
		return nil
	}

	history := chat.History(true)
	if !hasUnansweredToolCall(history) {
		return nil
	}

	repaired := make([]*genai.Content, len(history))
	copy(repaired, history)

	if len(pending) > 0 {
		repaired = append(repaired, &genai.Content{Role: genai.RoleUser, Parts: pending})
		log.Printf("chat %d: completing an unanswered tool call in history", chatID)
	} else {
		repaired = repaired[:len(repaired)-1]
		if len(repaired) > 0 && repaired[len(repaired)-1] != nil && repaired[len(repaired)-1].Role == genai.RoleUser {
			repaired = repaired[:len(repaired)-1]
		}
		log.Printf("chat %d: dropping an unanswered tool call from history", chatID)
	}

	newChat, err := c.client.Chats.Create(ctx, Model, c.aiConfig, repaired)
	if err != nil {
		return fmt.Errorf("failed to rebuild chat session: %w", err)
	}

	c.chatsLock.Lock()
	c.chats[chatID] = newChat
	c.chatsLock.Unlock()
	return nil
}

// describeEmptyResponse renders whatever the API told us about a reply that
// carried nothing usable, so the cause is visible in the log instead of being
// guessed at.
func describeEmptyResponse(result *genai.GenerateContentResponse) string {
	if result == nil {
		return "no response at all"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "candidates=%d", len(result.Candidates))
	if len(result.Candidates) > 0 && result.Candidates[0] != nil {
		candidate := result.Candidates[0]
		fmt.Fprintf(&sb, " finishReason=%q", candidate.FinishReason)
		if candidate.FinishMessage != "" {
			fmt.Fprintf(&sb, " finishMessage=%q", candidate.FinishMessage)
		}
		if candidate.Content == nil {
			sb.WriteString(" content=nil")
		} else {
			fmt.Fprintf(&sb, " parts=%d", len(candidate.Content.Parts))
		}
	}
	if usage := result.UsageMetadata; usage != nil {
		fmt.Fprintf(&sb, " tokens(prompt=%d candidates=%d thoughts=%d total=%d)",
			usage.PromptTokenCount, usage.CandidatesTokenCount, usage.ThoughtsTokenCount, usage.TotalTokenCount)
	}
	return sb.String()
}

// There used to be a checkChatHistory here that scanned chat.History(false)
// for a Content with no Parts, concluded the session was corrupt, threw the
// whole session away and told the user "I had to start a new session".
//
// It was reading the SDK's own bookkeeping as damage. The genai Chat keeps two
// histories: the comprehensive one, which deliberately records a failed turn
// as an empty Content marker, and the curated one, which excludes it. Send
// transmits the *curated* history, so an empty response never reaches a
// subsequent request and the session is never actually broken. History(false)
// asks for the comprehensive one -- so the check fired on healthy sessions,
// every time the model returned an empty reply, and the "recovery" discarded a
// conversation that was fine.
//
// The empty reply itself is real and worth handling; it is handled where it
// happens now, by retrying the exchange. See sendWithRetry.
