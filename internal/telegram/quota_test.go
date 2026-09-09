package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/gtrindade/ultra-kiew/internal/event"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"github.com/gtrindade/ultra-kiew/internal/usage"
)

// newUsageClient gives one test a client with a clean usage log.
//
// The log has to be cleared explicitly because TestMain points the whole
// package at a single scratch directory -- which it does so that async chat
// history writes always land somewhere harmless. Usage writes are synchronous,
// so truncating here is enough and does not race with anything.
func newUsageClient(t *testing.T) *Client {
	t.Helper()
	path := filepath.Join(storage.BasePath, storage.DBPath, usage.LogFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("could not clear the usage log: %v", err)
	}

	c := newTestClient(t)
	c.SetUsage(usage.NewRecorder(storage.NewClient()))
	return c
}

// spend writes n prompts for a user straight into the log, standing in for n
// turns already served.
func spend(c *Client, user string, n int) {
	for range n {
		c.usage.Record(usage.Entry{User: user, ChatID: -100, Kind: usage.KindPrompt})
	}
}

func TestUsageUserPrefersTheHandle(t *testing.T) {
	cases := []struct {
		name string
		from *models.User
		want string
	}{
		{"with a handle", &models.User{ID: 7, Username: "alice"}, "@alice"},
		{"no handle, has a name", &models.User{ID: 7, FirstName: "Alice"}, "Alice (id 7)"},
		{"nothing at all", &models.User{ID: 7}, "id 7"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageUser(tc.from); got != tc.want {
				t.Errorf("usageUser = %q, want %q", got, tc.want)
			}
		})
	}
}

// Two handle-less accounts must not share one allowance, which is what keying
// them both on an empty handle would do.
func TestHandlelessAccountsAreToldApart(t *testing.T) {
	a := usageUser(&models.User{ID: 1, FirstName: "Ana"})
	b := usageUser(&models.User{ID: 2, FirstName: "Ana"})
	if a == b {
		t.Fatalf("two different accounts got the same usage key: %q", a)
	}
}

func TestATurnIsServedWhileTheAllowanceHolds(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit-1)

	u := update(-100, "alice", "e aí", time.Now())
	if !c.allowTurn(context.Background(), nil, u, "Shadowrun", false) {
		t.Fatal("expected the turn to be served with one message left")
	}
}

func TestATurnIsRefusedOnceTheAllowanceIsSpent(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit)

	u := update(-100, "alice", "e aí", time.Now())
	if c.allowTurn(context.Background(), nil, u, "Shadowrun", false) {
		t.Fatal("expected the turn to be refused at the limit")
	}
}

// The refusal is logged so "is the limit actually biting anyone?" is
// answerable -- but it must never count towards the quota that caused it, or
// every retry would dig the hole deeper.
func TestARefusalIsLoggedWithoutSpendingQuota(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit)

	u := update(-100, "alice", "e aí", time.Now())
	c.allowTurn(context.Background(), nil, u, "Shadowrun", false)
	c.allowTurn(context.Background(), nil, u, "Shadowrun", false)

	_, used := c.usage.Allowance("@alice")
	if used != usage.DailyPromptLimit {
		t.Fatalf("refusals should not spend quota, got %d used", used)
	}

	rep, err := c.usage.Build(time.Now().Add(-time.Hour), time.Now(), -100)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, _, blocked := rep.Totals(); blocked != 2 {
		t.Fatalf("expected both refusals recorded, got %d", blocked)
	}
}

// The core exemption. Classification as a confirmation can only happen after
// the turn, from the tools it called -- so without this, someone who had spent
// their allowance would be refused before the code could ever discover that
// answering an invite was all they were doing.
func TestAPendingInviteSuspendsTheQuota(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit+10)

	u := update(555, "alice", "vou sim", time.Now())
	if !c.allowTurn(context.Background(), nil, u, "", true) {
		t.Fatal("someone with a pending invite must always be able to answer it")
	}
}

// With no recorder wired in the bot behaves exactly as it did before quotas
// existed.
func TestWithoutARecorderEverythingIsServed(t *testing.T) {
	c := newTestClient(t) // no SetUsage

	u := update(-100, "alice", "e aí", time.Now())
	if !c.allowTurn(context.Background(), nil, u, "Shadowrun", false) {
		t.Fatal("expected every turn to be served with no recorder")
	}
	c.recordTurn(u, "Shadowrun", googlegenai.TurnResult{}) // must not panic
}

// A turn whose only tool call was recording an answer is a confirmation,
// however the message was phrased -- "sim", "bora" and a reply to the card are
// all the same act and none is recognisable from its text.
func TestATurnThatOnlyConfirmedIsRecordedAsAConfirmation(t *testing.T) {
	c := newUsageClient(t)

	u := update(555, "alice", "vou sim", time.Now())
	c.recordTurn(u, "", googlegenai.TurnResult{
		Text: "Anotado!",
		ToolCalls: []googlegenai.ToolCall{
			{Name: event.EventManageToolName, Action: event.UpdateStatusAction},
		},
	})

	if _, used := c.usage.Allowance("@alice"); used != 0 {
		t.Fatalf("a confirmation must not spend quota, got %d used", used)
	}
}

// Answering two invites in one turn is still nothing but confirming.
func TestSeveralConfirmationsInOneTurnAreStillAConfirmation(t *testing.T) {
	c := newUsageClient(t)

	u := update(555, "alice", "vou nos dois", time.Now())
	c.recordTurn(u, "", googlegenai.TurnResult{
		ToolCalls: []googlegenai.ToolCall{
			{Name: event.EventManageToolName, Action: event.UpdateStatusAction},
			{Name: event.EventManageToolName, Action: event.UpdateStatusAction},
		},
	})

	if _, used := c.usage.Allowance("@alice"); used != 0 {
		t.Fatalf("expected no quota spent, got %d used", used)
	}
}

// The exemption is for confirming and nothing else. A turn that confirmed AND
// did real work is a prompt, or "sim, e marca outra pra sexta" would be free.
func TestConfirmingPlusRealWorkIsAPrompt(t *testing.T) {
	c := newUsageClient(t)

	u := update(-100, "alice", "vou sim, e marca outra pra sexta", time.Now())
	c.recordTurn(u, "Shadowrun", googlegenai.TurnResult{
		ToolCalls: []googlegenai.ToolCall{
			{Name: event.EventManageToolName, Action: event.UpdateStatusAction},
			{Name: event.EventManageToolName, Action: "create"},
		},
	})

	if _, used := c.usage.Allowance("@alice"); used != 1 {
		t.Fatalf("expected the turn to spend quota, got %d used", used)
	}
}

func TestAnOrdinaryTurnIsRecordedAsAPrompt(t *testing.T) {
	c := newUsageClient(t)

	u := update(-100, "alice", "rola 1d20", time.Now())
	c.recordTurn(u, "Shadowrun", googlegenai.TurnResult{
		Text:      "17",
		ToolCalls: []googlegenai.ToolCall{{Name: "roll_dice"}},
	})

	if _, used := c.usage.Allowance("@alice"); used != 1 {
		t.Fatalf("expected 1 prompt, got %d", used)
	}
}

// A turn with no tool calls at all is plain conversation, and must not be
// mistaken for a confirmation just because it called nothing that wasn't one.
func TestATurnWithNoToolCallsIsAPrompt(t *testing.T) {
	c := newUsageClient(t)

	u := update(-100, "alice", "quem foi Vecna?", time.Now())
	c.recordTurn(u, "Shadowrun", googlegenai.TurnResult{Text: "Vecna era..."})

	if _, used := c.usage.Allowance("@alice"); used != 1 {
		t.Fatalf("expected 1 prompt, got %d", used)
	}
}

func TestOnlyCalledRequiresAtLeastOneCall(t *testing.T) {
	empty := googlegenai.TurnResult{}
	if empty.OnlyCalled(event.EventManageToolName, event.UpdateStatusAction) {
		t.Error("a turn that called nothing has not confirmed anything")
	}
}

// Usage is attributed to the chat it happened in, which is what makes the
// group-scoped report possible.
func TestATurnIsAttributedToItsChat(t *testing.T) {
	c := newUsageClient(t)

	c.recordTurn(update(-100, "alice", "oi", time.Now()), "Shadowrun", googlegenai.TurnResult{})
	c.recordTurn(update(-200, "bmaraujo", "oi", time.Now()), "Outro Grupo", googlegenai.TurnResult{})

	rep, err := c.usage.Build(time.Now().Add(-time.Hour), time.Now(), -100)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].User != "@alice" {
		t.Fatalf("expected only this chat's traffic, got %+v", rep.Rows)
	}
	if rep.ChatTitle != "Shadowrun" {
		t.Errorf("expected the chat title recorded, got %q", rep.ChatTitle)
	}
}
