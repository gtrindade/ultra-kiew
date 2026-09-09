package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
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

// spend writes n messages for a user straight into the log, standing in for n
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
	if !c.allowTurn(context.Background(), nil, u, "Shadowrun") {
		t.Fatal("expected the turn to be served with one message left")
	}
}

func TestATurnIsRefusedOnceTheAllowanceIsSpent(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit)

	u := update(-100, "alice", "e aí", time.Now())
	if c.allowTurn(context.Background(), nil, u, "Shadowrun") {
		t.Fatal("expected the turn to be refused at the limit")
	}
}

// There are no exemptions, deliberately. Answering an event invite is a
// message like any other: it is served while the allowance holds and refused
// once it does not, with no inspection of what the turn was "really" for.
//
// The alternative was classifying turns after the fact, from whichever tools
// the model chose to call -- a quota rule resting on a model's judgement. This
// bot's event card is plain text with no inline buttons, so there is no
// callback to key off and no cheap deterministic signal to use instead.
func TestConfirmingAnEventIsNotExempt(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit)

	// A DM that is plainly an answer to an invite, from someone out of quota.
	u := update(555, "alice", "vou sim", time.Now())
	if c.allowTurn(context.Background(), nil, u, "") {
		t.Fatal("a confirmation must be refused like anything else once the quota is gone")
	}
}

func TestEveryServedTurnCostsTheSame(t *testing.T) {
	c := newUsageClient(t)

	c.recordTurn(update(555, "alice", "vou sim", time.Now()), "")
	c.recordTurn(update(-100, "alice", "rola 1d20", time.Now()), "Shadowrun")
	c.recordTurn(update(-100, "alice", "quem foi Vecna?", time.Now()), "Shadowrun")

	if used := c.usage.Standing("@alice").Used; used != 3 {
		t.Fatalf("expected all three to count, got %d", used)
	}
}

// The refusal is logged so "is the limit actually biting anyone?" is
// answerable -- but it must never count towards the quota that caused it, or
// every retry would dig the hole deeper.
func TestARefusalIsLoggedWithoutSpendingQuota(t *testing.T) {
	c := newUsageClient(t)
	spend(c, "@alice", usage.DailyPromptLimit)

	u := update(-100, "alice", "e aí", time.Now())
	c.allowTurn(context.Background(), nil, u, "Shadowrun")
	c.allowTurn(context.Background(), nil, u, "Shadowrun")

	used := c.usage.Standing("@alice").Used
	if used != usage.DailyPromptLimit {
		t.Fatalf("refusals should not spend quota, got %d used", used)
	}

	rep, err := c.usage.Build(time.Now().Add(-time.Hour), time.Now(), -100)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, blocked := rep.Totals(); blocked != 2 {
		t.Fatalf("expected both refusals recorded, got %d", blocked)
	}
}

// With no recorder wired in the bot behaves exactly as it did before quotas
// existed.
func TestWithoutARecorderEverythingIsServed(t *testing.T) {
	c := newTestClient(t) // no SetUsage

	u := update(-100, "alice", "e aí", time.Now())
	if !c.allowTurn(context.Background(), nil, u, "Shadowrun") {
		t.Fatal("expected every turn to be served with no recorder")
	}
	c.recordTurn(u, "Shadowrun") // must not panic
}

// Usage is attributed to the chat it happened in, which is what makes the
// group-scoped report possible.
func TestATurnIsAttributedToItsChat(t *testing.T) {
	c := newUsageClient(t)

	c.recordTurn(update(-100, "alice", "oi", time.Now()), "Shadowrun")
	c.recordTurn(update(-200, "bmaraujo", "oi", time.Now()), "Outro Grupo")

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
