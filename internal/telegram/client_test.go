package telegram

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// TestMain points storage at one scratch directory for the whole package
// rather than a fresh t.TempDir() per test.
//
// addToChatHistory persists through SaveChatHistoryAsync, which returns before
// its goroutine runs. With a per-test temp dir those goroutines wake up after
// the test that started them has already restored storage.BasePath -- so they
// either write into the next test's directory, into the repo's real data/
// directory, or into a directory being removed underneath them. One scratch
// dir for the package makes every late write land somewhere harmless.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "telegram-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not create a scratch dir: %v\n", err)
		os.Exit(1)
	}
	storage.BasePath, storage.DBPath = dir, "db"

	code := m.Run()

	// Best effort: a straggling async write can still hold a handle here.
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return &Client{
		storage:        storage.NewClient(),
		chatHistory:    make(map[int64][]*SavedMessage),
		maxHistorySize: 600,
		users:          make(map[string]int64),
		botName:        "kiew",
	}
}

func update(chatID int64, username, text string, at time.Time) *models.Update {
	return &models.Update{
		Message: &models.Message{
			ID:   1,
			Date: int(at.Unix()),
			Chat: models.Chat{ID: chatID},
			From: &models.User{ID: 42, Username: username},
			Text: text,
		},
	}
}

// The context block handed to the model is a line-oriented transcript, and
// googlegenai.leakedLineRegex exists to strip any of these lines the model
// echoes back -- it once invented whole messages in this shape and attributed
// them to real players. That scrubber is written against exactly this format,
// so if the format drifts here the scrubber silently stops matching.
func TestSavedMessageRendersTheShapeTheScrubberExpects(t *testing.T) {
	at := time.Date(2026, 4, 9, 7, 2, 35, 0, time.UTC)
	got := (&SavedMessage{UserName: "guilhermetmg", Text: "bora jogar", Timestamp: at}).String()

	want := "[2026-04-09T07:02:35Z - guilhermetmg]: `bora jogar`"
	if got != want {
		t.Fatalf("format changed:\n got %q\nwant %q", got, want)
	}

	// Mirrors googlegenai.leakedLineRegex. Kept as a literal rather than
	// imported because that identifier is unexported; the point is that the
	// two must agree.
	scrubber := regexp.MustCompile(`(?m)^\s*\[\d{4}-\d{2}-\d{2}T[^\]]*\][^\n:]*:\s*.*$`)
	if !scrubber.MatchString(got) {
		t.Errorf("the scrubber in googlegenai would no longer match this line: %q", got)
	}
}

func TestChatHistoryIsBoundedByMaxHistorySize(t *testing.T) {
	c := newTestClient(t)
	c.maxHistorySize = 5

	for i := range 20 {
		c.addToChatHistory(update(-100, "alice", strings.Repeat("x", i+1), time.Now()))
	}

	if got := len(c.chatHistory[-100]); got != 5 {
		t.Fatalf("expected the backlog to stay at 5, got %d", got)
	}
	// Oldest entries go first, so the newest message must survive.
	last := c.chatHistory[-100][4]
	if len(last.Text) != 20 {
		t.Errorf("expected the newest message to be kept, got %q", last.Text)
	}
}

func TestHistoryIsKeptPerChat(t *testing.T) {
	c := newTestClient(t)

	c.addToChatHistory(update(-100, "alice", "no grupo", time.Now()))
	c.addToChatHistory(update(555, "alice", "na dm", time.Now()))

	if len(c.chatHistory[-100]) != 1 || len(c.chatHistory[555]) != 1 {
		t.Fatalf("chats should not share a backlog: %v", c.chatHistory)
	}
	if c.chatHistory[-100][0].Text != "no grupo" {
		t.Errorf("wrong message stored against the group chat: %q", c.chatHistory[-100][0].Text)
	}
}

// The message being answered is rendered separately, inside
// <message_to_answer>, so skipping it here is what stops it appearing twice in
// the same prompt.
func TestGetChatHistoryBeforeExcludesTheTrailingMessages(t *testing.T) {
	c := newTestClient(t)

	for _, text := range []string{"primeira", "segunda", "terceira"} {
		c.addToChatHistory(update(-100, "alice", text, time.Now()))
	}

	got := c.getChatHistoryBefore(-100, 1)
	if strings.Contains(got, "terceira") {
		t.Errorf("the last message should have been skipped, got %q", got)
	}
	if !strings.Contains(got, "primeira") || !strings.Contains(got, "segunda") {
		t.Errorf("expected the earlier messages, got %q", got)
	}
	if lines := strings.Split(got, lineSep); len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d: %q", len(lines), got)
	}
}

func TestGetChatHistoryBeforeIsEmptyWhenThereIsNoBacklog(t *testing.T) {
	c := newTestClient(t)

	if got := c.getChatHistoryBefore(-100, 1); got != "" {
		t.Errorf("expected no context for an unknown chat, got %q", got)
	}

	c.addToChatHistory(update(-100, "alice", "só essa", time.Now()))
	if got := c.getChatHistoryBefore(-100, 1); got != "" {
		t.Errorf("the only message is the one being answered, so context should be empty, got %q", got)
	}
}

// Trimming rather than clearing is deliberate: the genai session that holds
// the conversation is in-memory only, so a restart that finds an emptied
// backlog loses the exchange entirely.
func TestTrimChatHistoryKeepsARestartTail(t *testing.T) {
	c := newTestClient(t)

	for i := range contextCarryOver + 15 {
		c.addToChatHistory(update(-100, "alice", string(rune('a'+i%26)), time.Now()))
	}
	c.trimChatHistory(-100)

	got := len(c.chatHistory[-100])
	if got != contextCarryOver {
		t.Fatalf("expected %d messages kept, got %d", contextCarryOver, got)
	}
	if got == 0 {
		t.Error("the backlog must not be emptied outright")
	}
}

func TestTrimChatHistoryLeavesAShortBacklogAlone(t *testing.T) {
	c := newTestClient(t)

	c.addToChatHistory(update(-100, "alice", "oi", time.Now()))
	c.trimChatHistory(-100)

	if len(c.chatHistory[-100]) != 1 {
		t.Fatalf("expected the single message to survive, got %d", len(c.chatHistory[-100]))
	}
}

func TestTrackUserRecordsTheHandleWithAnAtPrefix(t *testing.T) {
	c := newTestClient(t)

	c.trackUser(&models.User{ID: 99, Username: "bmaraujo"})

	id, known := c.users["@bmaraujo"]
	if !known {
		t.Fatalf("expected the user to be recorded under @bmaraujo, got %v", c.users)
	}
	if id != 99 {
		t.Errorf("expected the Telegram ID to be stored, got %d", id)
	}
}

// A Telegram account with no @handle cannot be addressed by one, so recording
// it under "@" would give every such user the same key -- and DMs meant for
// one of them would go to whoever was seen last.
func TestTrackUserIgnoresAccountsWithNoHandle(t *testing.T) {
	c := newTestClient(t)

	c.trackUser(&models.User{ID: 1, Username: ""})
	c.trackUser(&models.User{ID: 2, Username: ""})

	if len(c.users) != 0 {
		t.Fatalf("expected no users to be recorded, got %v", c.users)
	}
}

func TestTrackUserKeepsTheFirstIDForAHandle(t *testing.T) {
	c := newTestClient(t)

	c.trackUser(&models.User{ID: 1, Username: "alice"})
	c.trackUser(&models.User{ID: 1, Username: "alice"})

	if len(c.users) != 1 || c.users["@alice"] != 1 {
		t.Fatalf("unexpected users map: %v", c.users)
	}
}

func TestGetCopyOfChatHistoryDoesNotAliasTheLiveMap(t *testing.T) {
	c := newTestClient(t)
	c.addToChatHistory(update(-100, "alice", "original", time.Now()))

	snapshot := c.getCopyOfChatHistory()
	c.addToChatHistory(update(-100, "alice", "depois", time.Now()))

	// The copy is what gets handed to the async JSON encoder while the
	// handler goroutine keeps appending; sharing the slice would be a
	// concurrent map/slice write during encoding.
	if len(snapshot[-100]) != 1 {
		t.Fatalf("the snapshot changed after it was taken: %d entries", len(snapshot[-100]))
	}
	if len(c.chatHistory[-100]) != 2 {
		t.Fatalf("expected the live history to have grown, got %d", len(c.chatHistory[-100]))
	}
}
