package googlegenai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"
)

const MaxFunctionResponseLength = 10000

// Retry policy for a single exchange with the model.
//
// An empty reply from Gemini is usually a transient fault: the model hands
// back a candidate with nothing usable in it, most often right after a tool
// round, and asking again with the identical input works. But not always --
// see FallbackModel below for the case where it does not.
const (
	maxSendAttempts = 3
	sendRetryDelay  = 700 * time.Millisecond
)

// FallbackModel is tried once, after every attempt on Model has come back
// with an empty completion.
//
// This exists for a specific, observed shape of failure that retrying the
// same model cannot fix: a request that gets finishReason STOP with zero
// candidate tokens, byte-for-byte identical across every retry -- meaning the
// model is not flaky on this input, it is deterministically wrong for it. This
// is a known issue with no acknowledged root cause:
// https://discuss.ai.google.dev/t/empty-response-text-from-gemini-2-5-pro-despite-no-safety-and-max-tokens-issues/98010/23
// Community reports agree on one workaround: a different model asked the exact
// same question usually does not reproduce whatever degenerate state produced
// the empty completion, because it is a different model. This is that
// workaround, used only as a last resort since it costs more per call than
// Model.
//
// Was gemini-2.5-flash, which the API then started rejecting outright with
// "no longer available to new users" -- while still listing it as available
// from ListModels. That mismatch is the operative lesson: a model's presence
// in the catalog does not mean generateContent will actually accept it for
// this account, so don't trust ListModels to pick a replacement -- pick the
// one the rejection error itself names, which for that specific 404 was
// gemini-3.6-flash.
const FallbackModel = "gemini-3.6-flash"

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

// BuildPrompt wraps the conversation context in explicit delimiters and states
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
	result, _, err := c.sendWithRetry(ctx, chat, chatID, parts...)
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
	result, chat, err := c.sendWithRetry(ctx, chat, chatID, parts...)
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
		result, chat, err = c.sendWithRetry(ctx, chat, chatID, response...)
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
// sendWithRetry performs one exchange with the model, retrying when it comes
// back with nothing usable or fails transiently, and falling back to a
// different model as a last resort. It returns the chat the caller should use
// from here on: ordinarily the same one passed in, but a new *genai.Chat when
// the fallback path had to splice a different model's answer into a rebuilt
// session (see sendWithFallbackModel). Every caller must keep using whatever
// chat comes back, not the one it passed in, or a later turn in the same
// exchange would diverge from what is actually stored in c.chats.
func (c *Client) sendWithRetry(ctx context.Context, chat *genai.Chat, chatID int64, parts ...*genai.Part) (*genai.GenerateContentResponse, *genai.Chat, error) {
	var lastResult *genai.GenerateContentResponse

	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		result, err := chat.Send(ctx, parts...)
		if err != nil {
			if attempt < maxSendAttempts && isTransientAPIError(err) {
				log.Printf("chat %d: transient API error on attempt %d/%d, retrying: %v", chatID, attempt, maxSendAttempts, err)
				if !sleepOrDone(ctx, sendRetryDelay*time.Duration(attempt)) {
					return nil, chat, ctx.Err()
				}
				continue
			}
			return nil, chat, err
		}

		lastResult = result

		if hasUsableContent(result) {
			return result, chat, nil
		}

		// A deliberate refusal is deterministic: asking again spends quota to
		// be refused again. Hand it back so the caller can say what happened.
		if refusalReason(result) != "" {
			return result, chat, nil
		}

		if attempt < maxSendAttempts {
			log.Printf("chat %d: empty response on attempt %d/%d (%s), retrying",
				chatID, attempt, maxSendAttempts, describeEmptyResponse(result))
			if !sleepOrDone(ctx, sendRetryDelay*time.Duration(attempt)) {
				return nil, chat, ctx.Err()
			}
		} else {
			log.Printf("chat %d: empty response on final attempt %d/%d (%s)",
				chatID, attempt, maxSendAttempts, describeEmptyResponse(result))
		}
	}

	if fallbackResult, fallbackChat, err := c.sendWithFallbackModel(ctx, chat, chatID, parts...); err != nil {
		log.Printf("chat %d: fallback model attempt failed: %v", chatID, err)
	} else if hasUsableContent(fallbackResult) {
		log.Printf("chat %d: %s produced a usable response after %s returned only empty ones", chatID, FallbackModel, Model)
		return fallbackResult, fallbackChat, nil
	}

	// Out of attempts, fallback included. This is not an error: the caller
	// decides what an empty turn means, and for a tool round it may
	// legitimately be nothing at all.
	return lastResult, chat, nil
}

// sendWithFallbackModel re-asks the exact same question on FallbackModel, over
// the same curated history, and -- only if that produces something usable --
// splices the exchange into a freshly built session on Model so later turns
// carry it forward on the cheaper model. A failed or still-empty attempt
// leaves the original chat and its history completely untouched.
func (c *Client) sendWithFallbackModel(ctx context.Context, chat *genai.Chat, chatID int64, parts ...*genai.Part) (*genai.GenerateContentResponse, *genai.Chat, error) {
	log.Printf("chat %d: %s produced only empty responses, trying %s instead", chatID, Model, FallbackModel)

	history := chat.History(true)
	inputContent := &genai.Content{Role: genai.RoleUser, Parts: parts}
	contents := append(append([]*genai.Content{}, history...), inputContent)

	result, err := c.client.Models.GenerateContent(ctx, FallbackModel, contents, c.aiConfig)
	if err != nil {
		return nil, chat, err
	}
	if !hasUsableContent(result) {
		return result, chat, nil
	}

	var outputContents []*genai.Content
	if len(result.Candidates) > 0 && result.Candidates[0].Content != nil {
		outputContents = append(outputContents, result.Candidates[0].Content)
	}
	newHistory := append(contents, outputContents...)

	newChat, err := c.client.Chats.Create(ctx, Model, c.aiConfig, newHistory)
	if err != nil {
		return nil, chat, fmt.Errorf("failed to rebuild chat session after fallback: %w", err)
	}

	c.chatsLock.Lock()
	c.chats[chatID] = newChat
	c.chatsLock.Unlock()

	return result, newChat, nil
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
		log.Printf("function result too long (%d bytes), truncating", len(functionResult))
		functionResult = truncateAtRuneBoundary(functionResult, MaxFunctionResponseLength) + "...(truncated)"
	}

	return genai.NewPartFromFunctionResponse(call.Name, map[string]any{
		"result": functionResult,
	})
}

// truncateAtRuneBoundary cuts s to at most maxBytes without splitting a
// character in half.
//
// The naive s[:maxBytes] is a byte slice, and almost everything this bot
// passes through here is multi-byte: Portuguese prose, and the confirmation
// emoji on every event card. Landing mid-rune leaves an invalid UTF-8 tail,
// which encoding/json then rewrites as U+FFFD -- so a truncated tool result
// reached the model ending in a replacement character instead of a clean cut.
func truncateAtRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
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
