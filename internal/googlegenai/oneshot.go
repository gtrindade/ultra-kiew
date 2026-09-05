package googlegenai

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// GenerateText produces one piece of text from a self-contained instruction,
// with no tools and no conversation history.
//
// This exists because the reminder and confirmation announcements used to go
// through SendMessage, which pushed them into the *user's* live chat session
// with the full toolset attached. The model therefore answered them in the
// context of whatever conversation was half-finished, and with the ability to
// act on it. In testing that produced exactly what you would expect and nobody
// predicted: the 08:00 reminder came out as "para criar o evento, preciso que
// você me diga o fuso horário" -- the model had picked up an abandoned
// scheduling thread from hours earlier and answered that instead. It could just
// as easily have created or removed an event as a side effect of a reminder.
//
// Text generation for a fixed, code-decided message is a pure function of the
// prompt. Treating it as one means a reminder can never do anything but
// produce a string, and can never be steered by the chat it is about to be
// posted into.
func (c *Client) GenerateText(ctx context.Context, instruction string) (string, error) {
	return c.GenerateTextWithModel(ctx, Model, instruction)
}

func (c *Client) GenerateTextWithModel(ctx context.Context, model, instruction string) (string, error) {
	config := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{
				genai.NewPartFromText(fmt.Sprintf(
					`You are %q, a bot in a Brazilian tabletop RPG group chat. You write short, fun messages in Brazilian Portuguese (pt-BR).

You are being asked for ONE message. Output only the message text itself, as raw text that goes straight to Telegram: no quotes around it, no markdown fences, no preamble, no explanation of what you wrote, no <response> tags, and never a "[timestamp - user]:" prefix.`,
					c.config.BotName)),
			},
		},
	}

	result, err := c.client.Models.GenerateContent(
		ctx,
		model,
		[]*genai.Content{genai.NewContentFromText(instruction, genai.RoleUser)},
		config,
	)
	if err != nil {
		return "", err
	}

	return scrubResponse(result.Text()), nil
}
