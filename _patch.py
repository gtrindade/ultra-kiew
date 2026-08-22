import io
p='internal/telegram/client.go'
s=io.open(p,encoding='utf-8',newline='').read()

def cut(anchor, endanchor, new):
    global s
    i=s.index(anchor)
    j=s.index(endanchor,i)+len(endanchor)
    s=s[:i]+new+s[j:]

def rep(old,new):
    global s
    assert old in s, old[:70]
    s=s.replace(old,new,1)

cut('systemNote = fmt.Sprintf(', 'p.Date, p.GroupID, p.User)',
    'systemNote = fmt.Sprintf("This user has exactly ONE pending event invite: %q on %s. If this message is an answer to that invite, work out whether it is yes, no or late and call event_manage with action=\'update_status\' RIGHT NOW. Do not reply that you will note it down without calling the tool. The system already knows who is speaking and which event this is, so you do not pass a username or an event id.", p.Summary, p.Date)')

cut('systemNote = fmt.Sprintf(', 'pendingEvents[0].User)',
    'systemNote = fmt.Sprintf("This user has MULTIPLE pending event invites: %s. If they have made clear which one they are answering, call event_manage with action=\'update_status\' now, passing the matching event_group_id. If they say \'todos\' or \'ambos\', call it once per event. If it is not yet clear which one they mean, ask them before calling the tool. Do not reply that you will note it down without calling the tool.", strings.Join(evStrings, " | "))')

rep('fmt.Sprintf("%q (Date: %s, Group ID: %s)", p.Summary, p.Date, p.GroupID)',
    'fmt.Sprintf("%q on %s (event_group_id %s)", p.Summary, p.Date, p.GroupID)')

rep('update.Message.ReplyToMessage.From.Username == c.botName',
    'strings.EqualFold(update.Message.ReplyToMessage.From.Username, c.botName)')

cut('\ttext = c.getChatHistory(chatID)', 'c.ai.SendMessage(ctx, chatID, chatTitle, text)',
'''	// The message being answered is recorded too, and the backlog is trimmed
	// rather than emptied.
	//
	// It used to be cleared outright on every turn the bot answered, on the
	// theory that the genai session now holds it. That session is in-memory
	// only: after a restart the bot has neither the session nor the backlog it
	// threw away, so it loses the conversation entirely -- which is exactly
	// what happened when it was restarted mid-test. Keeping a bounded tail
	// means a restart costs recent context instead of all of it.
	c.addToChatHistory(update)
	prompt := googlegenai.BuildPrompt(
		c.getChatHistoryBefore(chatID, 1),
		getMessageFromUpdate(update).String(),
		systemNote,
	)
	c.trimChatHistory(chatID)

	chatTitle := update.Message.Chat.Title
	response, err = c.ai.SendMessage(ctx, chatID, chatTitle, prompt)''')

cut('func (c *Client) getChatHistory(chatID int64) string {', '''func (c *Client) clearChatHistory(chatID int64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.chatHistory[chatID] = make([]*SavedMessage, 0)
	c.storage.SaveChatHistoryAsync(c.getCopyOfChatHistory())
}''',
'''// getChatHistoryBefore renders the backlog for a chat, excluding the last
// `skip` entries -- normally 1, so the message being answered does not also
// appear inside the context block.
func (c *Client) getChatHistoryBefore(chatID int64, skip int) string {
	c.lock.RLock()
	defer c.lock.RUnlock()
	messages := c.chatHistory[chatID]
	if len(messages) <= skip {
		return ""
	}
	messages = messages[:len(messages)-skip]

	historyLines := make([]string, len(messages))
	for i, msg := range messages {
		historyLines[i] = msg.String()
	}
	return strings.Join(historyLines, LineSep)
}

// trimChatHistory keeps a bounded tail of recent messages as restart insurance.
func (c *Client) trimChatHistory(chatID int64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	messages := c.chatHistory[chatID]
	if len(messages) > contextCarryOver {
		c.chatHistory[chatID] = messages[len(messages)-contextCarryOver:]
	}
	c.storage.SaveChatHistoryAsync(c.getCopyOfChatHistory())
}''')

rep('// Client represents the Telegram bot client.',
'''// contextCarryOver is how many recent messages survive a turn the bot answered.
// Enough to keep a short exchange coherent across a restart, small enough that
// the model is not re-reading the same backlog on every mention.
const contextCarryOver = 20

// LineSep joins rendered history lines.
const LineSep = "\n"

// Client represents the Telegram bot client.''')

io.open(p,'w',encoding='utf-8',newline='').write(s)
print('ok')
