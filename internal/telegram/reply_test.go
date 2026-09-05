package telegram

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// replyTo marks an update as a reply to a message from someone else.
func replyTo(u *models.Update, author *models.User, text string) *models.Update {
	u.Message.ReplyToMessage = &models.Message{
		ID:   u.Message.ID - 1,
		Chat: u.Message.Chat,
		From: author,
		Text: text,
	}
	return u
}

// The reply has to ride inside the existing single-line format. A second line
// would slip past googlegenai.leakedLineRegex, which strips echoed transcript
// lines one at a time -- the mechanism that stops the bot quoting messages
// real users never sent.
func TestAReplyStaysOnOneLineAndTheScrubberStillMatchesIt(t *testing.T) {
	at := time.Date(2026, 4, 9, 7, 2, 35, 0, time.UTC)
	msg := &SavedMessage{
		UserName:    "guilhermetmg",
		Text:        "bora",
		Timestamp:   at,
		ReplyToUser: "kiew",
		ReplyToText: "Sessao 12 - Sexta-feira, 10/04/2026 as 21:00\n\n\t@alice 💪\n\t@bob ❔\n",
	}

	got := msg.String()
	if strings.Contains(got, "\n") {
		t.Fatalf("the rendered line must not contain a newline:\n%q", got)
	}

	scrubber := regexp.MustCompile(`(?m)^\s*\[\d{4}-\d{2}-\d{2}T[^\]]*\][^\n:]*:\s*.*$`)
	if !scrubber.MatchString(got) {
		t.Errorf("the scrubber in googlegenai would no longer match this line: %q", got)
	}
	if !strings.Contains(got, "em resposta a kiew") {
		t.Errorf("the reply marker is missing: %q", got)
	}
	if !strings.Contains(got, "Sessao 12") {
		t.Errorf("the quoted text is missing: %q", got)
	}
}

func TestAMessageWithNoReplyRendersExactlyAsBefore(t *testing.T) {
	at := time.Date(2026, 4, 9, 7, 2, 35, 0, time.UTC)
	got := (&SavedMessage{UserName: "guilhermetmg", Text: "bora jogar", Timestamp: at}).String()

	if got != "[2026-04-09T07:02:35Z - guilhermetmg]: `bora jogar`" {
		t.Fatalf("the no-reply format changed: %q", got)
	}
}

func TestTheInlineQuoteIsTruncated(t *testing.T) {
	msg := &SavedMessage{
		UserName:    "alice",
		Text:        "isso",
		Timestamp:   time.Now(),
		ReplyToUser: "bob",
		ReplyToText: strings.Repeat("palavra ", 200),
	}

	got := msg.String()
	// A backlog of 20 lines each quoting an entire other message would undo
	// the point of bounding the backlog at all.
	if len([]rune(got)) > 200 {
		t.Errorf("the backlog line grew to %d runes: %q", len([]rune(got)), got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("expected a truncation marker: %q", got)
	}
}

// ReplyContext feeds the <replying_to> block, which is not line-bound, so it
// keeps the line breaks that make an event card readable.
func TestReplyContextKeepsLineBreaks(t *testing.T) {
	msg := &SavedMessage{
		ReplyToUser: "kiew",
		ReplyToText: "Sessao 12\n\t@alice 💪\n\t@bob ❔",
	}

	got := msg.ReplyContext()
	if !strings.HasPrefix(got, "kiew: ") {
		t.Errorf("expected the author named first, got %q", got)
	}
	if !strings.Contains(got, "\n\t@bob") {
		t.Errorf("expected the card layout preserved, got %q", got)
	}
}

func TestReplyContextIsEmptyWhenThereWasNoReply(t *testing.T) {
	msg := &SavedMessage{UserName: "alice", Text: "oi"}
	if got := msg.ReplyContext(); got != "" {
		t.Errorf("expected no reply context, got %q", got)
	}
}

func TestGetMessageFromUpdateCapturesTheReply(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "bora", time.Now()),
		&models.User{ID: 7, Username: "bmaraujo"}, "alguem topa sabado?")

	got := c.getMessageFromUpdate(u)
	if got.ReplyToUser != "bmaraujo" {
		t.Errorf("expected the replied-to author, got %q", got.ReplyToUser)
	}
	if got.ReplyToText != "alguem topa sabado?" {
		t.Errorf("expected the replied-to text, got %q", got.ReplyToText)
	}
}

// The bot's Telegram handle is not necessarily what it is called in the
// prompt, and the model has to recognise its own past messages as its own --
// especially the event card, which is the single most replied-to message this
// bot sends and the one least likely to still be in any session history.
func TestAReplyToTheBotIsAttributedToTheConfiguredName(t *testing.T) {
	c := newTestClient(t) // botName is "kiew"

	// Deliberately a handle that does NOT match botName: identity comes from
	// the ID, so a bot whose Telegram username differs from its configured
	// name is still recognised as itself.
	u := replyTo(update(-100, "alice", "eu vou", time.Now()),
		&models.User{ID: testBotID, Username: "ultra_kiew_prod_bot", IsBot: true},
		"Sessao 12 - Sexta-feira")

	got := c.getMessageFromUpdate(u)
	if got.ReplyToUser != "kiew" {
		t.Errorf("expected the bot named %q, got %q", "kiew", got.ReplyToUser)
	}
}

// Another bot in the group is not this bot, and must not be relabelled as it.
func TestAReplyToADifferentBotIsNotAttributedToUs(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "kkk", time.Now()),
		&models.User{ID: 999, Username: "outro_bot", IsBot: true}, "beep boop")

	got := c.getMessageFromUpdate(u)
	if got.ReplyToUser != "outro_bot" {
		t.Errorf("expected the other bot's own handle, got %q", got.ReplyToUser)
	}
}

// A human who happens to be called like the bot must not be mistaken for it.
func TestAHumanNamedLikeTheBotIsNotMistakenForIt(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "oi", time.Now()),
		&models.User{ID: 123, Username: "kiew", IsBot: false}, "fala")

	got := c.getMessageFromUpdate(u)
	if got.ReplyToUser != "kiew" {
		t.Errorf("expected the human's own handle, got %q", got.ReplyToUser)
	}
	if c.isSelf(&models.User{ID: 123, Username: "kiew"}) {
		t.Error("a human must never be identified as the bot")
	}
}

// Telegram's partial-quote reply is a stronger statement of intent than the
// whole message: the user literally highlighted the part they meant.
func TestAPartialQuoteWinsOverTheWholeMessage(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "esse", time.Now()),
		&models.User{ID: 7, Username: "bmaraujo"},
		"podemos jogar sabado, domingo ou terca")
	u.Message.Quote = &models.TextQuote{Text: "domingo", IsManual: true}

	got := c.getMessageFromUpdate(u)
	if got.ReplyToText != "domingo" {
		t.Errorf("expected the highlighted fragment, got %q", got.ReplyToText)
	}
}

// Naming the kind of thing beats silence: it is the difference between the
// model knowing there is a referent it cannot read and it thinking there was
// no reply at all.
func TestANonTextReplyIsStillDescribed(t *testing.T) {
	c := newTestClient(t)

	cases := map[string]func(*models.Message){
		"(uma foto)":    func(m *models.Message) { m.Photo = []models.PhotoSize{{FileID: "x"}} },
		"(um sticker)":  func(m *models.Message) { m.Sticker = &models.Sticker{FileID: "x"} },
		"(um audio)":    func(m *models.Message) { m.Voice = &models.Voice{FileID: "x"} },
		"(uma enquete)": func(m *models.Message) { m.Poll = &models.Poll{ID: "x"} },
	}

	for want, attach := range cases {
		t.Run(want, func(t *testing.T) {
			u := replyTo(update(-100, "alice", "kkkk", time.Now()),
				&models.User{ID: 7, Username: "bmaraujo"}, "")
			attach(u.Message.ReplyToMessage)

			got := c.getMessageFromUpdate(u)
			if got.ReplyToText != want {
				t.Errorf("expected %q, got %q", want, got.ReplyToText)
			}
		})
	}
}

func TestACaptionIsUsedWhenTheRepliedToMessageHasOne(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "quem e esse", time.Now()),
		&models.User{ID: 7, Username: "bmaraujo"}, "")
	u.Message.ReplyToMessage.Photo = []models.PhotoSize{{FileID: "x"}}
	u.Message.ReplyToMessage.Caption = "meu novo personagem"

	got := c.getMessageFromUpdate(u)
	if got.ReplyToText != "meu novo personagem" {
		t.Errorf("expected the caption, got %q", got.ReplyToText)
	}
}

// Anonymous admins and channel posts arrive with no From at all.
func TestAReplyWithNoIdentifiableAuthorStillCarriesItsText(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "isso", time.Now()), nil, "regras da mesa")

	got := c.getMessageFromUpdate(u)
	if got.ReplyToUser != "alguem" {
		t.Errorf("expected a placeholder author, got %q", got.ReplyToUser)
	}
	if got.ReplyToText != "regras da mesa" {
		t.Errorf("expected the text to survive, got %q", got.ReplyToText)
	}
	if !strings.Contains(got.String(), "em resposta a alguem") {
		t.Errorf("expected the marker to render: %q", got.String())
	}
}

func TestAMessageThatIsNotAReplyCarriesNothing(t *testing.T) {
	c := newTestClient(t)

	got := c.getMessageFromUpdate(update(-100, "alice", "oi", time.Now()))
	if got.ReplyToUser != "" || got.ReplyToText != "" {
		t.Errorf("expected no reply fields, got %q / %q", got.ReplyToUser, got.ReplyToText)
	}
	if strings.Contains(got.String(), "em resposta") {
		t.Errorf("a non-reply must not render a marker: %q", got.String())
	}
}

// Replies are captured on every message, including ones that do not trigger
// the bot -- otherwise the backlog would lose the referent for exactly the
// conversation the bot is later asked to catch up on.
func TestRepliesAreRecordedInTheBacklogToo(t *testing.T) {
	c := newTestClient(t)

	c.addToChatHistory(replyTo(update(-100, "alice", "concordo", time.Now()),
		&models.User{ID: 7, Username: "bmaraujo"}, "sabado ta ruim pra mim"))

	stored := c.chatHistory[-100]
	if len(stored) != 1 {
		t.Fatalf("expected one stored message, got %d", len(stored))
	}
	if stored[0].ReplyToText != "sabado ta ruim pra mim" {
		t.Errorf("the reply was not stored: %+v", stored[0])
	}
	if !strings.Contains(c.getChatHistoryBefore(-100, 0), "em resposta a bmaraujo") {
		t.Errorf("the rendered backlog lost the reply:\n%s", c.getChatHistoryBefore(-100, 0))
	}
}

// Older chat_history.json files were written before these fields existed. They
// have to keep decoding, or a deploy would drop everyone's backlog.
func TestHistoryWrittenBeforeRepliesExistedStillDecodes(t *testing.T) {
	var msg SavedMessage
	old := `{"UserID":42,"UserName":"alice","Text":"oi","Timestamp":"2026-04-09T07:02:35Z"}`
	if err := json.Unmarshal([]byte(old), &msg); err != nil {
		t.Fatalf("an older record no longer decodes: %v", err)
	}
	if msg.Text != "oi" || msg.ReplyToUser != "" {
		t.Errorf("unexpected decode: %+v", msg)
	}
}

func TestTruncateRunesNeverSplitsACharacter(t *testing.T) {
	s := strings.Repeat("💪", 5) + strings.Repeat("ã", 5)

	for limit := range 12 {
		got := truncateRunes(s, limit)
		for _, r := range got {
			if r == '�' {
				t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
			}
		}
	}
	if got := truncateRunes("curto", 100); got != "curto" {
		t.Errorf("expected the string untouched, got %q", got)
	}
}

// The exact reported scenario: a reply to ANOTHER user's message, with the bot
// tagged in the text so it is triggered by the mention rather than by the
// reply. Nothing in the capture path looks at who was replied to, so this must
// behave identically to a reply aimed at the bot.
func TestAReplyToAnotherUserWithTheBotTaggedIsStillCaptured(t *testing.T) {
	c := newTestClient(t)

	u := replyTo(update(-100, "alice", "@UltraKiewTestBot o que voce acha?", time.Now()),
		&models.User{ID: 7, Username: "bmaraujo"}, "vamos jogar sabado?")

	got := c.getMessageFromUpdate(u)
	if got.ReplyToUser != "bmaraujo" || got.ReplyToText != "vamos jogar sabado?" {
		t.Fatalf("the reply was lost: %+v", got)
	}
	if !strings.Contains(got.ReplyContext(), "vamos jogar sabado?") {
		t.Errorf("the reply context is empty: %q", got.ReplyContext())
	}
	if !strings.Contains(got.String(), "em resposta a bmaraujo") {
		t.Errorf("the backlog line lost the marker: %q", got.String())
	}
}

// A reply to a message in another chat -- a forwarded channel post, say --
// arrives with ReplyToMessage nil and ExternalReply set. ExternalReply carries
// no text of its own, so Quote is the only place the words are, and dropping
// it because ReplyToMessage was nil threw the whole case away.
func TestAnExternalReplyKeepsItsQuotedText(t *testing.T) {
	c := newTestClient(t)

	u := update(-100, "alice", "isso aqui", time.Now())
	u.Message.ExternalReply = &models.ExternalReplyInfo{MessageID: 99}
	u.Message.Quote = &models.TextQuote{Text: "sessao adiada para domingo", IsManual: true}

	got := c.getMessageFromUpdate(u)
	if got.ReplyToText != "sessao adiada para domingo" {
		t.Fatalf("the quoted text was dropped: %+v", got)
	}
	if got.ReplyToUser == "" {
		t.Error("expected some attribution rather than none")
	}
	if !strings.Contains(got.ReplyContext(), "sessao adiada") {
		t.Errorf("the reply context is empty: %q", got.ReplyContext())
	}
}

func TestAnExternalReplyWithNoQuoteIsStillAnnounced(t *testing.T) {
	c := newTestClient(t)

	u := update(-100, "alice", "kkkk", time.Now())
	u.Message.ExternalReply = &models.ExternalReplyInfo{MessageID: 99}

	got := c.getMessageFromUpdate(u)
	if got.ReplyToText == "" {
		t.Error("expected the reply to be described rather than silently dropped")
	}
}

// A standalone Quote with no ReplyToMessage and no ExternalReply should still
// not be thrown away.
func TestAStandaloneQuoteIsNotDropped(t *testing.T) {
	c := newTestClient(t)

	u := update(-100, "alice", "esse trecho", time.Now())
	u.Message.Quote = &models.TextQuote{Text: "as 21h em ponto", IsManual: true}

	got := c.getMessageFromUpdate(u)
	if got.ReplyToText != "as 21h em ponto" {
		t.Fatalf("the quote was dropped: %+v", got)
	}
}
