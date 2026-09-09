package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

const testChatID = int64(-1001234567890)

func setupRecorder(t *testing.T) *Recorder {
	t.Helper()
	tempDir := t.TempDir()
	storage.BasePath, storage.DBPath = tempDir, "db"
	if err := os.MkdirAll(filepath.Join(tempDir, "db"), 0o755); err != nil {
		t.Fatalf("could not create the db dir: %v", err)
	}
	return NewRecorder(storage.NewClient())
}

// record writes one entry a given number of minutes ago.
func record(r *Recorder, user string, kind Kind, minutesAgo int, chatID int64) {
	r.Record(Entry{
		Timestamp: time.Now().Add(-time.Duration(minutesAgo) * time.Minute).Unix(),
		User:      user,
		ChatID:    chatID,
		Kind:      kind,
	})
}

func reportArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		googlegenai.ArgCallerChatID: testChatID,
		googlegenai.ArgChatTitle:    "Shadowrun",
		googlegenai.ArgIsPrivate:    false,
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestRecordAndCountRoundTrip(t *testing.T) {
	r := setupRecorder(t)

	for range 3 {
		record(r, "@alice", KindPrompt, 10, testChatID)
	}

	got, err := r.PromptsSince("@alice", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("PromptsSince: %v", err)
	}
	if got != 3 {
		t.Fatalf("expected 3 prompts, got %d", got)
	}
}

// The log is append-only, so nothing may be lost between writes -- that is the
// entire reason for the storage shape.
func TestEveryRecordSurvivesTheNextOne(t *testing.T) {
	r := setupRecorder(t)

	for i := range 40 {
		record(r, fmt.Sprintf("@user%d", i%4), KindPrompt, i, testChatID)
	}

	total := 0
	if err := r.scan(func(Entry) { total++ }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if total != 40 {
		t.Fatalf("expected all 40 records, got %d", total)
	}
}

// The window is rolling, not a calendar day: something logged 25 hours ago is
// outside it even though "yesterday" has not fully rolled over.
func TestTheQuotaWindowIsRolling(t *testing.T) {
	r := setupRecorder(t)

	record(r, "@alice", KindPrompt, 23*60, testChatID) // inside
	record(r, "@alice", KindPrompt, 25*60, testChatID) // outside

	got, err := r.PromptsSince("@alice", time.Now().Add(-QuotaWindow))
	if err != nil {
		t.Fatalf("PromptsSince: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected only the message inside the window, got %d", got)
	}
}

// Confirming attendance is the bot's core job. Rationing it would be
// self-defeating, so it is logged but never counted.
func TestConfirmationsDoNotSpendQuota(t *testing.T) {
	r := setupRecorder(t)

	for range 60 {
		record(r, "@alice", KindConfirmation, 5, testChatID)
	}
	record(r, "@alice", KindPrompt, 5, testChatID)

	remaining, used := r.Allowance("@alice")
	if used != 1 {
		t.Fatalf("only the prompt should count, got %d used", used)
	}
	if remaining != DailyPromptLimit-1 {
		t.Fatalf("expected %d remaining, got %d", DailyPromptLimit-1, remaining)
	}
}

// A blocked message must not count against the quota that blocked it, or the
// refusal would deepen itself every time someone retried.
func TestBlockedMessagesDoNotSpendQuota(t *testing.T) {
	r := setupRecorder(t)

	for range 10 {
		record(r, "@alice", KindBlocked, 5, testChatID)
	}

	_, used := r.Allowance("@alice")
	if used != 0 {
		t.Fatalf("blocked messages should not count, got %d used", used)
	}
}

func TestAllowanceRunsOutAtTheLimit(t *testing.T) {
	r := setupRecorder(t)

	for range DailyPromptLimit - 1 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}
	if remaining, _ := r.Allowance("@alice"); remaining != 1 {
		t.Fatalf("expected 1 left, got %d", remaining)
	}

	record(r, "@alice", KindPrompt, 5, testChatID)
	remaining, used := r.Allowance("@alice")
	if remaining != 0 {
		t.Fatalf("expected the allowance spent, got %d", remaining)
	}
	if used != DailyPromptLimit {
		t.Fatalf("expected %d used, got %d", DailyPromptLimit, used)
	}
}

// Going past the limit must not report a negative allowance -- that would read
// as a debt and render as nonsense.
func TestAllowanceNeverGoesNegative(t *testing.T) {
	r := setupRecorder(t)

	for range DailyPromptLimit + 20 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}
	if remaining, _ := r.Allowance("@alice"); remaining != 0 {
		t.Fatalf("expected 0, got %d", remaining)
	}
}

// Telegram handles are case-insensitive, so @Alice must not be a second
// allowance for the same person.
func TestQuotaIsCaseInsensitive(t *testing.T) {
	r := setupRecorder(t)

	record(r, "@Alice", KindPrompt, 5, testChatID)
	record(r, "@alice", KindPrompt, 5, testChatID)
	record(r, "@ALICE", KindPrompt, 5, testChatID)

	if _, used := r.Allowance("@aLiCe"); used != 3 {
		t.Fatalf("expected all three to be the same person, got %d", used)
	}
}

func TestOnePersonsUseDoesNotAffectAnother(t *testing.T) {
	r := setupRecorder(t)

	for range DailyPromptLimit {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}

	if remaining, _ := r.Allowance("@bmaraujo"); remaining != DailyPromptLimit {
		t.Fatalf("expected an untouched allowance, got %d", remaining)
	}
}

// The quota is a guard against runaway use, not a second way for a broken disk
// to take the bot down. An unreadable log fails open.
func TestAnUnreadableLogFailsOpen(t *testing.T) {
	r := setupRecorder(t)
	record(r, "@alice", KindPrompt, 5, testChatID)

	path := filepath.Join(storage.BasePath, storage.DBPath, LogFileName)
	if err := os.WriteFile(path, []byte("{not json at all\n"), 0o600); err != nil {
		t.Fatalf("could not corrupt the log: %v", err)
	}

	remaining, _ := r.Allowance("@alice")
	if remaining != DailyPromptLimit {
		t.Fatalf("expected the full allowance when the log cannot be read, got %d", remaining)
	}
}

// A process killed mid-append leaves a truncated final line. Losing every
// earlier record over it would be the wrong trade.
func TestATruncatedFinalLineDoesNotDiscardTheRest(t *testing.T) {
	r := setupRecorder(t)
	for range 3 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}

	path := filepath.Join(storage.BasePath, storage.DBPath, LogFileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"ts":123,"user":"@bob","ki`); err != nil {
		t.Fatal(err)
	}
	file.Close()

	if _, used := r.Allowance("@alice"); used != 3 {
		t.Fatalf("expected the three intact records, got %d", used)
	}
}

func TestNoLogFileYetIsNotAnError(t *testing.T) {
	r := setupRecorder(t)

	remaining, used := r.Allowance("@alice")
	if remaining != DailyPromptLimit || used != 0 {
		t.Fatalf("a first run should have a full allowance, got %d/%d", remaining, used)
	}
}

// Scope is decided by where the question was asked. In a group, another
// group's traffic must not leak in.
func TestAGroupReportCoversOnlyThatGroup(t *testing.T) {
	r := setupRecorder(t)
	other := int64(-100999)

	record(r, "@alice", KindPrompt, 5, testChatID)
	record(r, "@alice", KindPrompt, 5, testChatID)
	record(r, "@bmaraujo", KindPrompt, 5, other)

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), testChatID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].User != "@alice" {
		t.Fatalf("expected only this chat's users, got %+v", rep.Rows)
	}
	if prompts, _, _ := rep.Totals(); prompts != 2 {
		t.Fatalf("expected 2 prompts, got %d", prompts)
	}
}

func TestAGlobalReportCoversEveryChat(t *testing.T) {
	r := setupRecorder(t)
	other := int64(-100999)

	record(r, "@alice", KindPrompt, 5, testChatID)
	record(r, "@bmaraujo", KindPrompt, 5, other)

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Rows) != 2 {
		t.Fatalf("expected both users, got %+v", rep.Rows)
	}
	if rep.Chats != 2 {
		t.Fatalf("expected 2 chats, got %d", rep.Chats)
	}
}

func TestReportRespectsItsWindow(t *testing.T) {
	r := setupRecorder(t)

	record(r, "@alice", KindPrompt, 30, testChatID)     // in
	record(r, "@alice", KindPrompt, 200, testChatID)    // out
	record(r, "@bmaraujo", KindPrompt, 400, testChatID) // out

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), testChatID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Prompts != 1 {
		t.Fatalf("expected one user with one prompt, got %+v", rep.Rows)
	}
}

// Two reports over the same data must not shuffle rows around.
func TestReportIsSortedBusiestFirstAndStable(t *testing.T) {
	r := setupRecorder(t)

	for range 5 {
		record(r, "@zeca", KindPrompt, 5, testChatID)
	}
	for range 5 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}
	for range 9 {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), testChatID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{"@bmaraujo", "@alice", "@zeca"}
	for i, w := range want {
		if rep.Rows[i].User != w {
			t.Fatalf("row %d: expected %s, got %s (all: %+v)", i, w, rep.Rows[i].User, rep.Rows)
		}
	}
}

func TestRenderShowsEachKindAndTheTotals(t *testing.T) {
	r := setupRecorder(t)

	for range 3 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}
	record(r, "@alice", KindConfirmation, 5, testChatID)
	record(r, "@bmaraujo", KindBlocked, 5, testChatID)

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), testChatID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := rep.Render()

	for _, want := range []string{"@alice", "3 prompts", "1 confirmação", "@bmaraujo", "1 bloqueada"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

func TestRenderSaysSoWhenNobodyTalked(t *testing.T) {
	r := setupRecorder(t)

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), testChatID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := rep.Render(); !strings.Contains(got, "Ninguém") {
		t.Errorf("expected an explicit empty answer, got:\n%s", got)
	}
}

// "restam N" is only true over the quota window itself. Printed next to a
// week's or a month's totals it would be arithmetic dressed up as a fact.
func TestRemainingIsOnlyShownForTheQuotaWindow(t *testing.T) {
	r := setupRecorder(t)
	for range DailyPromptLimit - 2 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}

	daily, err := r.Build(time.Now().Add(-QuotaWindow), time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(daily.Render(), "restam") {
		t.Errorf("expected a remaining count over the quota window:\n%s", daily.Render())
	}

	weekly, err := r.Build(time.Now().Add(-7*24*time.Hour), time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(weekly.Render(), "restam") {
		t.Errorf("a weekly report must not claim a daily remainder:\n%s", weekly.Render())
	}
}

// The quota is per person across every chat, but a group-scoped row only
// counts that group. Printing "restam N" there would promise headroom the
// person may already have spent elsewhere -- wrong, and wrong in the
// direction that misleads.
func TestRemainingIsNeverShownOnAGroupScopedReport(t *testing.T) {
	r := setupRecorder(t)
	other := int64(-100999)

	for range DailyPromptLimit - 2 {
		record(r, "@alice", KindPrompt, 5, testChatID)
	}
	// Same person, same 24h, different chat: their real remainder is lower
	// than this group's rows can possibly show.
	for range 2 {
		record(r, "@alice", KindPrompt, 5, other)
	}

	scoped, err := r.Build(time.Now().Add(-QuotaWindow), time.Now(), testChatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scoped.Render(), "restam") {
		t.Errorf("a group report must not state a global remainder:\n%s", scoped.Render())
	}

	// The allowance itself still counts every chat.
	if remaining, _ := r.Allowance("@alice"); remaining != 0 {
		t.Errorf("expected the quota to span chats, got %d remaining", remaining)
	}
}

func TestManageDefaultsToTheLast24Hours(t *testing.T) {
	r := setupRecorder(t)
	record(r, "@alice", KindPrompt, 60, testChatID)    // in
	record(r, "@alice", KindPrompt, 30*60, testChatID) // out

	got, err := r.Manage(reportArgs(nil))
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if !strings.Contains(got, "1 prompt") {
		t.Errorf("expected only the last 24h, got:\n%s", got)
	}
}

func TestManageHonoursAnHoursArgument(t *testing.T) {
	r := setupRecorder(t)
	record(r, "@alice", KindPrompt, 60, testChatID)
	record(r, "@alice", KindPrompt, 30*60, testChatID)

	got, err := r.Manage(reportArgs(map[string]any{"hours": float64(48)}))
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if !strings.Contains(got, "2 prompts") {
		t.Errorf("expected both messages inside 48h, got:\n%s", got)
	}
}

// The model sends JSON numbers as float64, but not always -- a string is a
// shape it reaches for often enough to handle rather than reject.
func TestManageAcceptsHoursAsAString(t *testing.T) {
	r := setupRecorder(t)
	record(r, "@alice", KindPrompt, 30*60, testChatID)

	got, err := r.Manage(reportArgs(map[string]any{"hours": "48"}))
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if !strings.Contains(got, "1 prompt") {
		t.Errorf("expected the string to be read as a number, got:\n%s", got)
	}
}

func TestManageAcceptsAnExplicitWindow(t *testing.T) {
	r := setupRecorder(t)
	now := time.Now()
	record(r, "@alice", KindPrompt, 5, testChatID)

	got, err := r.Manage(reportArgs(map[string]any{
		"since": now.Add(-time.Hour).Format(time.RFC3339),
		"until": now.Add(time.Hour).Format(time.RFC3339),
	}))
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if !strings.Contains(got, "1 prompt") {
		t.Errorf("expected the explicit window to be honoured, got:\n%s", got)
	}
}

func TestManageAcceptsABareDate(t *testing.T) {
	r := setupRecorder(t)
	record(r, "@alice", KindPrompt, 5, testChatID)

	got, err := r.Manage(reportArgs(map[string]any{
		"since": time.Now().Add(-24 * time.Hour).Format("2006-01-02"),
	}))
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if !strings.Contains(got, "1 prompt") {
		t.Errorf("expected a bare date to work, got:\n%s", got)
	}
}

// Every one of these is something a model will send, and each has to come back
// as an error it can act on rather than a silently wrong report.
func TestManageRejectsNonsenseWindows(t *testing.T) {
	r := setupRecorder(t)
	now := time.Now()

	cases := []struct {
		name string
		args map[string]any
	}{
		{"negative hours", map[string]any{"hours": float64(-5)}},
		{"zero hours", map[string]any{"hours": float64(0)}},
		{"unparseable since", map[string]any{"since": "semana passada"}},
		{"backwards window", map[string]any{
			"since": now.Format(time.RFC3339),
			"until": now.Add(-time.Hour).Format(time.RFC3339),
		}},
		{"longer than a year", map[string]any{"hours": float64(400 * 24)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Manage(reportArgs(tc.args)); err == nil {
				t.Fatalf("expected %v to be rejected", tc.args)
			}
		})
	}
}

// Scope is not the model's to choose: it comes from where the message was
// sent, exactly like every other tool's chat context.
func TestManageScopesByWhereItWasAsked(t *testing.T) {
	r := setupRecorder(t)
	other := int64(-100999)
	record(r, "@alice", KindPrompt, 5, testChatID)
	record(r, "@bmaraujo", KindPrompt, 5, other)

	inGroup, err := r.Manage(reportArgs(nil))
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if strings.Contains(inGroup, "@bmaraujo") {
		t.Errorf("a group report leaked another chat:\n%s", inGroup)
	}

	dmArgs := reportArgs(map[string]any{})
	dmArgs[googlegenai.ArgIsPrivate] = true
	dmArgs[googlegenai.ArgCallerChatID] = int64(55847128)

	inDM, err := r.Manage(dmArgs)
	if err != nil {
		t.Fatalf("Manage: %v", err)
	}
	if !strings.Contains(inDM, "@alice") || !strings.Contains(inDM, "@bmaraujo") {
		t.Errorf("a DM report should cover every chat:\n%s", inDM)
	}
}

func TestManageNeedsTheCallerChatContext(t *testing.T) {
	r := setupRecorder(t)

	if _, err := r.Manage(map[string]any{"hours": float64(24)}); err == nil {
		t.Fatal("expected a missing caller chat ID to be rejected")
	}
}

func TestQuotaMessageNamesTheNumberAndExemptsConfirmations(t *testing.T) {
	got := QuotaMessage(50)

	if !strings.Contains(got, "50") {
		t.Errorf("expected the count, got %q", got)
	}
	// Someone who hits the wall needs to know their invites still work, or
	// they will assume the bot is broken.
	if !strings.Contains(strings.ToLower(got), "confirmar") {
		t.Errorf("expected the confirmation exemption to be stated, got %q", got)
	}
}

func TestToolConfigMatchesTheDispatchName(t *testing.T) {
	tool := GetToolConfig()
	if len(tool.FunctionDeclarations) != 1 {
		t.Fatalf("expected one declaration, got %d", len(tool.FunctionDeclarations))
	}
	decl := tool.FunctionDeclarations[0]
	if decl.Name != UsageReportToolName {
		t.Errorf("declared name %q does not match the dispatch key %q", decl.Name, UsageReportToolName)
	}
	// Scope must not be an argument -- the code decides it.
	for _, forbidden := range []string{"chat_id", "chatID", "scope", "global"} {
		if _, exists := decl.Parameters.Properties[forbidden]; exists {
			t.Errorf("the model must not be able to choose scope, but %q is a parameter", forbidden)
		}
	}
}
