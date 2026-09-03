package event

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/meet"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

const (
	testGroupChatID = int64(-1001234567890)
	testUserChatID  = int64(55847128)
)

// setupTestStorage gives each test its own storage dir, pre-seeded with a group
// so event creation is allowed. Pass a timezone to simulate a chat that has
// already had one recorded.
func setupTestStorage(t *testing.T, timezone string) *storage.Client {
	t.Helper()

	tempDir := t.TempDir()
	storage.BasePath = tempDir
	storage.DBPath = "db"
	os.MkdirAll(filepath.Join(tempDir, "db"), 0755)

	client := storage.NewClient()

	key := fmt.Sprintf("%d", testGroupChatID)
	if err := client.Save(filepath.Join("db", "groups.json"), map[string]Group{
		key: {Users: []string{"@alice", "@bob"}, Timezone: timezone},
	}); err != nil {
		t.Fatalf("failed to seed groups: %v", err)
	}
	if err := client.Save(filepath.Join("db", "users.json"), map[string]int64{
		"@alice": testUserChatID,
	}); err != nil {
		t.Fatalf("failed to seed users: %v", err)
	}

	return client
}

// groupArgs builds the args a tool call arrives with from a group chat. The
// underscore-prefixed keys are the ones the code injects; a test that sets them
// by hand is standing in for googlegenai.runToolCall.
func groupArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		googlegenai.ArgCallerChatID: testGroupChatID,
		googlegenai.ArgChatTitle:    "Teste Ultra Kiew",
		googlegenai.ArgIsPrivate:    false,
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func dmArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		googlegenai.ArgCallerChatID: testUserChatID,
		googlegenai.ArgChatTitle:    "",
		googlegenai.ArgIsPrivate:    true,
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func tomorrowAt(hour int) string {
	return time.Now().Add(24 * time.Hour).Format("2006-01-02") + fmt.Sprintf("T%02d:00", hour)
}

func TestCreateRejectsPastTime(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	past := time.Now().In(mustLoad(t, "America/Sao_Paulo")).Add(-2 * time.Hour)
	_, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": past.Format("2006-01-02T15:04"),
	}))

	if err == nil || !strings.Contains(err.Error(), "in the past") {
		t.Fatalf("expected a past-time refusal, got err=%v", err)
	}
}

// The model must not be able to invent a timezone. Before this, it was asked to
// quote the user's words as proof they had named one, and it produced an event
// stamped EDT for a group that had said BRT.
func TestCreateRefusesWithoutAKnownTimezone(t *testing.T) {
	m := NewManager(setupTestStorage(t, "")) // no timezone on record

	_, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	}))

	if err == nil || !strings.Contains(err.Error(), "no timezone on record") {
		t.Fatalf("expected a refusal asking for the timezone, got err=%v", err)
	}
}

// Having answered "BRT" once, nobody should be asked again.
func TestCreateRemembersTheTimezone(t *testing.T) {
	storageClient := setupTestStorage(t, "")
	m := NewManager(storageClient)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
		"timezone":       "BRT",
	})); err != nil {
		t.Fatalf("create with an explicit timezone failed: %v", err)
	}

	groups := make(map[string]Group)
	storageClient.LoadFromDB(groupsFileName, &groups)
	if got := groups[fmt.Sprintf("%d", testGroupChatID)].Timezone; got != "America/Sao_Paulo" {
		t.Fatalf("expected the group to remember America/Sao_Paulo, got %q", got)
	}
}

// The stored timestamp must be the offset for that date in that zone, not the
// server's offset and not whatever the model felt like sending.
func TestCreateResolvesTheOffsetItself(t *testing.T) {
	storageClient := setupTestStorage(t, "")
	m := NewManager(storageClient)

	local := tomorrowAt(21)
	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": local,
		"timezone":       "BRT",
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	want, err := time.ParseInLocation("2006-01-02T15:04", local, mustLoad(t, "America/Sao_Paulo"))
	if err != nil {
		t.Fatal(err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	got := events[fmt.Sprintf("%d", testGroupChatID)]
	if got.Timestamp != want.Unix() {
		t.Fatalf("timestamp %d does not match São Paulo wall clock %d", got.Timestamp, want.Unix())
	}
	if !strings.Contains(got.Date, "21:00") {
		t.Fatalf("expected the card to read 21:00, got %q", got.Date)
	}
}

// One event per chat, enforced in code rather than by asking the model nicely.
func TestCreateRefusesASecondEvent(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	reply, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(22),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(reply, "NO NEW EVENT WAS CREATED") {
		t.Fatalf("expected a refusal naming the existing event, got %q", reply)
	}
}

// A chat ID the model supplies must have no effect at all. This is the failure
// that dominated testing: the model carried an ID around, got it wrong, and
// asked users to type it in.
func TestModelSuppliedChatIDIsIgnored(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
		"chatID":         "-999999999",
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if _, ok := events[fmt.Sprintf("%d", testGroupChatID)]; !ok {
		t.Fatalf("event was not filed under the real caller chat: %v", events)
	}
	if _, ok := events["-999999999"]; ok {
		t.Fatal("event was filed under the chat ID the model made up")
	}
}

func TestEventActionsAreRefusedInDMs(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	for _, action := range []string{"create", "remove", "get"} {
		_, err := m.Manage(dmArgs(map[string]any{
			"action":         action,
			"local_datetime": tomorrowAt(21),
		}))
		if err == nil || !strings.Contains(err.Error(), "private DM") {
			t.Errorf("action %q should be refused in a DM, got err=%v", action, err)
		}
	}
}

// Who is answering comes from the Telegram user, so a user cannot answer for
// someone else however they phrase it.
func TestUpdateStatusIdentifiesTheCallerItself(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// @alice is the one on this DM; the model tries to answer for @bob.
	if _, err := m.Manage(dmArgs(map[string]any{
		"action":   "update_status",
		"status":   "yes",
		"username": "@bob",
	})); err != nil {
		t.Fatalf("update_status failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	confs := events[fmt.Sprintf("%d", testGroupChatID)].Confirmations
	if confs["@alice"] != "💪" {
		t.Errorf("expected @alice to be confirmed, got %q", confs["@alice"])
	}
	if confs["@bob"] != "❔" {
		t.Errorf("@bob was answered for by someone else: %q", confs["@bob"])
	}
}

func TestUpdateStatusIsRefusedInAGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	_, err := m.Manage(groupArgs(map[string]any{"action": "update_status", "status": "yes"}))
	if err == nil || !strings.Contains(err.Error(), "private DM") {
		t.Fatalf("expected a refusal outside a DM, got err=%v", err)
	}
}

// End-to-end: once both invitees have confirmed (roster fully answered),
// one of them flip-flopping must be recorded and must NOT be treated as the
// first-time-completing-the-roster case again (AllRespondedSent already
// latched). m.bot is nil in tests, so sendStatusChangeUpdate's actual send is
// a no-op -- what's covered here is that the state transition happens
// without error and the stored confirmation is the new answer, not silently
// dropped or reverted.
func TestStatusChangeAfterFullConfirmationIsRecorded(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	chatIDStr := fmt.Sprintf("%d", testGroupChatID)

	const bobChatID = int64(918273645)
	users := make(map[string]int64)
	storageClient.LoadFromDB(usersFileName, &users)
	users["@bob"] = bobChatID
	if err := storageClient.SaveToDB(usersFileName, users); err != nil {
		t.Fatalf("failed to seed @bob's DM chat: %v", err)
	}

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	bobArgs := func(extra map[string]any) map[string]any {
		args := map[string]any{
			googlegenai.ArgCallerChatID: bobChatID,
			googlegenai.ArgChatTitle:    "",
			googlegenai.ArgIsPrivate:    true,
		}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}

	if _, err := m.Manage(dmArgs(map[string]any{"action": "update_status", "status": "yes"})); err != nil {
		t.Fatalf("@alice's update_status failed: %v", err)
	}
	if _, err := m.Manage(bobArgs(map[string]any{"action": "update_status", "status": "yes"})); err != nil {
		t.Fatalf("@bob's update_status failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if !events[chatIDStr].AllRespondedSent {
		t.Fatal("expected the roster to be fully confirmed after both answered")
	}

	// Now @bob flip-flops to "no" -- after the roster was already complete.
	if _, err := m.Manage(bobArgs(map[string]any{"action": "update_status", "status": "no"})); err != nil {
		t.Fatalf("@bob's status change failed: %v", err)
	}

	storageClient.LoadFromDB(eventsFileName, &events)
	if got := events[chatIDStr].Confirmations["@bob"]; got != "🐔" {
		t.Fatalf("expected @bob's answer to be updated to 'no' (🐔), got %q", got)
	}
}

// 'unsure' walks an answer back to ❔ -- the actual "remove my answer"
// mechanism, since there is no separate delete-the-confirmation action.
func TestUpdateStatusUnsureResetsToUnanswered(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	chatIDStr := fmt.Sprintf("%d", testGroupChatID)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if _, err := m.Manage(dmArgs(map[string]any{"action": "update_status", "status": "yes"})); err != nil {
		t.Fatalf("@alice's update_status failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if events[chatIDStr].Confirmations["@alice"] != "💪" {
		t.Fatalf("expected @alice confirmed before the reset, got %q", events[chatIDStr].Confirmations["@alice"])
	}

	if _, err := m.Manage(dmArgs(map[string]any{"action": "update_status", "status": "unsure"})); err != nil {
		t.Fatalf("@alice's reset to unsure failed: %v", err)
	}

	storageClient.LoadFromDB(eventsFileName, &events)
	if got := events[chatIDStr].Confirmations["@alice"]; got != "❔" {
		t.Fatalf("expected @alice's answer to reset to ❔, got %q", got)
	}
}

// request_responses exists so a stale invite (someone who hadn't started the
// bot yet, or just hasn't answered) can be re-pinged without removing and
// recreating the whole event.
func TestRequestResponsesRefusesWithNoEvent(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	reply, err := m.Manage(groupArgs(map[string]any{"action": "request_responses"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(reply, "No event exists") {
		t.Fatalf("expected a no-event message, got %q", reply)
	}
}

func TestRequestResponsesNoOpsWhenEveryoneAlreadyAnswered(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	chatIDStr := fmt.Sprintf("%d", testGroupChatID)
	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	ev := events[chatIDStr]
	ev.Confirmations["@alice"] = "💪"
	ev.Confirmations["@bob"] = "🐔"
	events[chatIDStr] = ev
	if err := storageClient.SaveToDB(eventsFileName, events); err != nil {
		t.Fatalf("failed to seed confirmations: %v", err)
	}

	reply, err := m.Manage(groupArgs(map[string]any{"action": "request_responses"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(reply, "already responded") {
		t.Fatalf("expected an already-responded message, got %q", reply)
	}
}

func TestRequestResponsesIsRefusedInAGroupOnlyDM(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	_, err := m.Manage(dmArgs(map[string]any{"action": "request_responses"}))
	if err == nil || !strings.Contains(err.Error(), "private DM") {
		t.Fatalf("expected a refusal outside the group chat, got err=%v", err)
	}
}

// SyncGroupMembers is what the group manager calls when its roster changes,
// instead of writing events.json itself.
func TestSyncGroupMembersAddsNewcomersAsUnanswered(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	chatIDStr := fmt.Sprintf("%d", testGroupChatID)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	note, err := m.SyncGroupMembers(chatIDStr, []string{"@alice", "@bob", "@carol"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if !strings.Contains(note, "@carol") {
		t.Fatalf("expected the note to mention the added user, got %q", note)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if got := events[chatIDStr].Confirmations["@carol"]; got != "❔" {
		t.Fatalf("expected @carol added to the card as unanswered, got %q", got)
	}
}

func TestSyncGroupMembersDropsRemovedMembers(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	chatIDStr := fmt.Sprintf("%d", testGroupChatID)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if _, err := m.SyncGroupMembers(chatIDStr, []string{"@alice"}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	confs := events[chatIDStr].Confirmations
	if _, still := confs["@bob"]; still {
		t.Fatalf("expected @bob dropped from the card, got %v", confs)
	}
	if _, kept := confs["@alice"]; !kept {
		t.Fatalf("expected @alice kept on the card, got %v", confs)
	}
}

// Adding someone who has not answered means the roster is no longer complete,
// so the "everyone's in" announcement must become available again rather than
// staying latched shut from the previous roster.
func TestSyncGroupMembersReopensTheAllRespondedAnnouncement(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	chatIDStr := fmt.Sprintf("%d", testGroupChatID)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	ev := events[chatIDStr]
	ev.AllRespondedSent = true
	events[chatIDStr] = ev
	if err := storageClient.SaveToDB(eventsFileName, events); err != nil {
		t.Fatalf("failed to seed: %v", err)
	}

	if _, err := m.SyncGroupMembers(chatIDStr, []string{"@alice", "@bob", "@carol"}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	storageClient.LoadFromDB(eventsFileName, &events)
	if events[chatIDStr].AllRespondedSent {
		t.Fatal("expected the all-responded latch to reopen after someone new was added")
	}
}

func TestSyncGroupMembersIsANoOpWithoutAnEvent(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	note, err := m.SyncGroupMembers(fmt.Sprintf("%d", testGroupChatID), []string{"@alice"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if note != "" {
		t.Fatalf("expected no note when there is no event to sync, got %q", note)
	}
}

// A roster change that adds and removes nobody must not redraw the card or
// claim anything happened.
func TestSyncGroupMembersIsANoOpWhenTheRosterMatches(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	note, err := m.SyncGroupMembers(fmt.Sprintf("%d", testGroupChatID), []string{"@alice", "@bob"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if note != "" {
		t.Fatalf("expected no note when nothing changed, got %q", note)
	}
}

func TestResolveTimezone(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"BRT", "America/Sao_Paulo"},
		{"brt", "America/Sao_Paulo"},
		{"horário de brasília", ""}, // free text is not a zone; must fail loudly
		{"America/Sao_Paulo", "America/Sao_Paulo"},
		{"UTC", "UTC"},
	} {
		loc, err := resolveTimezone(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%q should not resolve, got %v", tc.in, loc)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q failed to resolve: %v", tc.in, err)
			continue
		}
		if loc.String() != tc.want {
			t.Errorf("%q resolved to %q, want %q", tc.in, loc, tc.want)
		}
	}
}

// This exact behavior went missing once already -- written, described to the
// user as done, and silently dropped in a later rewrite -- so it gets its own
// direct test rather than relying only on the end-to-end coverage in
// meet_lifecycle_test.go.
func TestAppendMeetLinkAddsTheJoinURI(t *testing.T) {
	got := appendMeetLink("Prepare-se!", &MeetInfo{JoinURI: "https://meet.google.com/abc-defg-hij"})
	if !strings.Contains(got, "https://meet.google.com/abc-defg-hij") {
		t.Fatalf("expected the join link to be appended, got %q", got)
	}
	if !strings.HasPrefix(got, "Prepare-se!") {
		t.Fatalf("expected the original text to be preserved, got %q", got)
	}
}

func TestAppendMeetLinkLeavesTextUnchangedWithNoMeet(t *testing.T) {
	for _, meetInfo := range []*MeetInfo{nil, {JoinURI: ""}} {
		if got := appendMeetLink("Prepare-se!", meetInfo); got != "Prepare-se!" {
			t.Errorf("expected no change with meet=%+v, got %q", meetInfo, got)
		}
	}
}

// The exact bug reported in production: one player confirmed, one running 10
// minutes late, nobody absent. This used to get the same "sessão está
// comprometida" roast as an outright no-show, because the only signal
// available was "is everyone exactly 💪". It must now pick the light,
// proportionate tone, and its (deterministic, network-free) fallback text
// must actually name the late player rather than reading as generic doom.
func TestBuildAnnouncementIsProportionateForALateArrivalWithNoAbsences(t *testing.T) {
	groupUsers := []string{"@guilhermetmg", "@guikiew"}
	confirmations := map[string]string{
		"@guilhermetmg": "🐢 (10 min)",
		"@guikiew":      "💪",
	}

	a := buildAnnouncement("Test Ultra-Kiew", groupUsers, confirmations)

	if !strings.Contains(a.fallback, "@guilhermetmg") {
		t.Errorf("expected the fallback to name the late player, got %q", a.fallback)
	}
	if !strings.Contains(a.fallback, "(10 min)") {
		t.Errorf("expected the fallback to include the late duration, got %q", a.fallback)
	}
	for _, doomWord := range []string{"comprometida", "falha crítica", "morto"} {
		if strings.Contains(strings.ToLower(a.fallback), strings.ToLower(doomWord)) {
			t.Errorf("fallback reads like the old no-show panic (contains %q): %q", doomWord, a.fallback)
		}
	}
	if len(a.namesToVerify) != 1 || a.namesToVerify[0] != "@guilhermetmg" {
		t.Errorf("expected exactly the late player to require verification, got %v", a.namesToVerify)
	}
}

// A real no-show must still get the sharper tone, and must name specifically
// who bailed, not the group as a whole.
func TestBuildAnnouncementRoastsOnlyTheNoShow(t *testing.T) {
	groupUsers := []string{"@alice", "@bob", "@carol"}
	confirmations := map[string]string{
		"@alice": "💪",
		"@bob":   "🐔",
		"@carol": "🐢 (5 min)",
	}

	a := buildAnnouncement("Test Ultra-Kiew", groupUsers, confirmations)

	if !strings.Contains(a.fallback, "@bob") {
		t.Errorf("expected the fallback to name the no-show, got %q", a.fallback)
	}
	if len(a.namesToVerify) != 1 || a.namesToVerify[0] != "@bob" {
		t.Errorf("expected only the no-show to require verification, not the late player, got %v", a.namesToVerify)
	}
}

// Everyone confirmed on time: the hype path, unchanged, with nothing to
// specifically verify.
func TestBuildAnnouncementCelebratesWhenEveryoneIsOnTime(t *testing.T) {
	groupUsers := []string{"@alice", "@bob"}
	confirmations := map[string]string{"@alice": "💪", "@bob": "💪"}

	a := buildAnnouncement("Test Ultra-Kiew", groupUsers, confirmations)

	if len(a.namesToVerify) != 0 {
		t.Errorf("expected nothing to verify when everyone is on time, got %v", a.namesToVerify)
	}
	if !strings.Contains(a.fallback, "Todos confirmados") {
		t.Errorf("expected the hype fallback, got %q", a.fallback)
	}
}

// The exact bug reported in production: an event whose scheduled time has
// passed used to sit in events.json until its whole Meet lifecycle finished
// (which can take a long while -- grace periods, artifact waits), and
// create() refuses whenever anything is already filed under this chat. That
// meant nobody could schedule the next session without first manually
// removing the one that had already happened. A started event must free up
// the chat immediately, not once its Meet tracking is done.
//
// With no Meet integration configured, there is nothing to wait for, so the
// started event is fully archived in the same tick it starts.
func TestPastEventNoLongerBlocksSchedulingANewOne(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	chatIDStr := fmt.Sprintf("%d", testGroupChatID)
	pastEvent := Event{
		Date:          "Domingo, 23/08/2026 às 22:00",
		Timestamp:     time.Now().Add(-1 * time.Minute).Unix(),
		Summary:       "Sessão que já aconteceu",
		Confirmations: map[string]string{"@alice": "💪"},
	}
	if err := storageClient.SaveToDB(eventsFileName, map[string]Event{chatIDStr: pastEvent}); err != nil {
		t.Fatalf("failed to seed a past event: %v", err)
	}

	m.runMonitorTick(context.Background())

	// The chat must now be free: no event blocking a new create().
	reply, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if strings.Contains(reply, "NO NEW EVENT WAS CREATED") {
		t.Fatalf("new event was blocked by the one that had already started: %q", reply)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if got := events[chatIDStr].Summary; got != "Teste Ultra Kiew" {
		t.Fatalf("expected the new event to take the chat's slot, got %+v", events[chatIDStr])
	}

	archivedEvents := make(map[string][]Event)
	storageClient.LoadFromDB("archived_events.json", &archivedEvents)
	archived := archivedEvents[chatIDStr]
	if len(archived) != 1 || archived[0].Summary != "Sessão que já aconteceu" {
		t.Fatalf("expected the started event to be archived, got %+v", archivedEvents)
	}
}

// Same scenario, but with a Meet session still actually running: the started
// event must move into live_sessions.json (not be dropped, not be archived
// early) while still freeing up the chat for a new create() right away.
func TestPastEventWithRunningMeetSessionStillUnblocksScheduling(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	fm := &fakeMeet{records: []meet.ConferenceRecord{{
		Name:      "conferenceRecords/abc",
		StartTime: time.Now().Format(time.RFC3339),
		// No EndTime: the call is still going.
	}}}
	m.meet = fm

	chatIDStr := fmt.Sprintf("%d", testGroupChatID)
	pastEvent := Event{
		Date:          "Domingo, 23/08/2026 às 22:00",
		Timestamp:     time.Now().Add(-1 * time.Minute).Unix(),
		Summary:       "Sessão em andamento",
		Confirmations: map[string]string{"@alice": "💪"},
		Meet:          &MeetInfo{SpaceName: "spaces/xyz", JoinURI: "https://meet.google.com/abc-defg-hij"},
	}
	if err := storageClient.SaveToDB(eventsFileName, map[string]Event{chatIDStr: pastEvent}); err != nil {
		t.Fatalf("failed to seed a past event: %v", err)
	}

	m.runMonitorTick(context.Background())

	reply, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if strings.Contains(reply, "NO NEW EVENT WAS CREATED") {
		t.Fatalf("new event was blocked by the one still in progress: %q", reply)
	}

	liveSessions := make(map[string][]Event)
	storageClient.LoadFromDB(liveSessionsFileName, &liveSessions)
	sessions := liveSessions[chatIDStr]
	if len(sessions) != 1 || sessions[0].Summary != "Sessão em andamento" {
		t.Fatalf("expected the still-running session to be tracked in live_sessions.json, got %+v", liveSessions)
	}

	archivedEvents := make(map[string][]Event)
	storageClient.LoadFromDB("archived_events.json", &archivedEvents)
	if len(archivedEvents[chatIDStr]) != 0 {
		t.Fatalf("a still-running session must not be archived yet, got %+v", archivedEvents)
	}
}

// The exact case reported in production: one player never answered the
// invite. The reminder must still fire for whoever did confirm -- the
// session happens either way -- but the model must be told someone never
// responded, rather than the prompt looking like everyone is accounted for.
func TestBuildReminderMessageFlagsSomeoneWhoNeverResponded(t *testing.T) {
	confirmations := map[string]string{
		"@alice": "💪",
		"@bob":   "❔", // never answered
	}

	rm := buildReminderMessage("Test Ultra-Kiew", confirmations, "AGORA")

	if len(rm.confirmedTags) != 1 || rm.confirmedTags[0] != "@alice" {
		t.Fatalf("expected only @alice to be tagged/counted as coming, got %v", rm.confirmedTags)
	}
	if !strings.Contains(rm.prompt, "@bob") {
		t.Errorf("expected the prompt to flag @bob as never having responded, got %q", rm.prompt)
	}
	if !strings.Contains(rm.fallback, "@bob") {
		t.Errorf("expected the fallback to mention @bob too, got %q", rm.fallback)
	}
}

// When everyone has answered and confirmed, no extra roster context is
// needed -- the reminder should read exactly as it did before this feature.
func TestBuildReminderMessageAddsNoExtraContextWhenEveryoneConfirmed(t *testing.T) {
	confirmations := map[string]string{"@alice": "💪", "@bob": "💪"}

	rm := buildReminderMessage("Test Ultra-Kiew", confirmations, "AGORA")

	if strings.Contains(rm.prompt, "Nem todo mundo respondeu") {
		t.Errorf("expected no roster-context note when everyone confirmed, got %q", rm.prompt)
	}
	if len(rm.confirmedTags) != 2 {
		t.Errorf("expected both users tagged, got %v", rm.confirmedTags)
	}
}

// No confirmed attendees at all: the reminder must not fire (nobody to remind
// or tag), same as before this feature.
func TestBuildReminderMessageIsEmptyWithNoConfirmedUsers(t *testing.T) {
	confirmations := map[string]string{"@alice": "❔", "@bob": "🐔"}

	rm := buildReminderMessage("Test Ultra-Kiew", confirmations, "AGORA")

	if len(rm.confirmedTags) != 0 || rm.prompt != "" {
		t.Fatalf("expected an empty reminderMessage with no one confirmed, got %+v", rm)
	}
}

// The initial invite DM is sent at creation time, so the daily no-response
// nudge must not repeat that same ask hours later on the same day -- it
// should wait until the following day.
func TestCreateSeedsTheNudgeDateSoDailyNudgeWaitsUntilTomorrow(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	loc := mustLoad(t, "America/Sao_Paulo")
	today := time.Now().In(loc).Format("2006-01-02")

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	ev := events[fmt.Sprintf("%d", testGroupChatID)]
	if ev.LastNoResponseNudgeDate != today {
		t.Fatalf("expected the nudge date to be seeded to today (%s), got %q", today, ev.LastNoResponseNudgeDate)
	}

	// Later the same day, at or after 9am: must not nudge again.
	laterToday := time.Date(time.Now().In(loc).Year(), time.Now().In(loc).Month(), time.Now().In(loc).Day(), 15, 0, 0, 0, loc).Unix()
	if m.maybeSendDailyNudges(&ev, Group{Timezone: "America/Sao_Paulo"}, map[string]int64{}, laterToday) {
		t.Fatal("should not nudge on the day the event was created")
	}

	// The next day at 9am: should nudge normally.
	tomorrow9am := time.Now().In(loc).AddDate(0, 0, 1)
	tomorrow9am = time.Date(tomorrow9am.Year(), tomorrow9am.Month(), tomorrow9am.Day(), 9, 0, 0, 0, loc)
	if !m.maybeSendDailyNudges(&ev, Group{Timezone: "America/Sao_Paulo"}, map[string]int64{}, tomorrow9am.Unix()) {
		t.Fatal("expected a nudge the day after creation")
	}
}

func TestConfirmationSeverityRanksCommitmentLevels(t *testing.T) {
	if confirmationSeverity("💪") <= confirmationSeverity("🐢") {
		t.Error("confirmed should rank above late")
	}
	if confirmationSeverity("🐢") <= confirmationSeverity("🐔") {
		t.Error("late should rank above absent")
	}
	if confirmationSeverity("🐢 (10 min)") != confirmationSeverity("🐢 (20 min)") {
		t.Error("a changed late-time estimate should not change severity")
	}
	if confirmationSeverity("❔") != confirmationSeverity("") {
		t.Error("never-answered and missing should rank the same")
	}
	// "unsure" (which resets an answer back to ❔) must rank as equally
	// negative as an outright "no" -- withdrawing to uncertain after having
	// confirmed is not a neutral change.
	if confirmationSeverity("❔") != confirmationSeverity("🐔") {
		t.Error("unsure (❔) should rank the same as an outright no (🐔)")
	}
}

func TestBuildStatusChangeMessageJudgesToneByDirection(t *testing.T) {
	worse := buildStatusChangeMessage("@bob", "Test Ultra-Kiew", "💪", "🐔")
	if !worse.worse {
		t.Error("confirmed -> absent should be judged as a change for the worse")
	}
	if !strings.Contains(worse.fallback, "@bob") {
		t.Errorf("expected the fallback to name @bob, got %q", worse.fallback)
	}

	better := buildStatusChangeMessage("@bob", "Test Ultra-Kiew", "🐔", "💪")
	if better.worse {
		t.Error("absent -> confirmed should not be judged as a change for the worse")
	}

	lateToWorseLate := buildStatusChangeMessage("@bob", "Test Ultra-Kiew", "🐢 (10 min)", "🐔")
	if !lateToWorseLate.worse {
		t.Error("late -> absent should be judged as a change for the worse")
	}

	// "unsure" resets to ❔, which must be judged exactly as negative as an
	// outright "no" -- withdrawing a confirmation is not neutral.
	confirmedToUnsure := buildStatusChangeMessage("@bob", "Test Ultra-Kiew", "💪", "❔")
	if !confirmedToUnsure.worse {
		t.Error("confirmed -> unsure should be judged as a change for the worse")
	}
	if !strings.Contains(confirmedToUnsure.fallback, "incerto") {
		t.Errorf("expected the fallback to describe the new state as 'incerto', got %q", confirmedToUnsure.fallback)
	}

	lateToUnsure := buildStatusChangeMessage("@bob", "Test Ultra-Kiew", "🐢", "❔")
	if !lateToUnsure.worse {
		t.Error("late -> unsure should be judged as a change for the worse")
	}
}

func TestNoResponseUsersTreatsMissingAndQuestionMarkTheSame(t *testing.T) {
	confirmations := map[string]string{"@alice": "❔", "@bob": "💪", "@carol": ""}
	got := noResponseUsers(confirmations)
	if len(got) != 2 {
		t.Fatalf("expected 2 pending users (missing counts the same as ❔), got %v", got)
	}
}

// The daily nudge is gated on the GROUP's timezone, not the server's -- same
// reasoning as event scheduling itself.
func TestMaybeSendDailyNudgesWaitsForTheDailyHourThenFiresOncePerDay(t *testing.T) {
	m := NewManager(setupTestStorage(t, ""))
	loc := mustLoad(t, "America/Sao_Paulo")
	group := Group{Timezone: "America/Sao_Paulo"}

	ev := &Event{Confirmations: map[string]string{"@alice": "❔"}}

	before9am := time.Date(2026, 8, 24, 8, 59, 0, 0, loc).Unix()
	if m.maybeSendDailyNudges(ev, group, map[string]int64{}, before9am) {
		t.Fatal("should not nudge before 9am in the group's timezone")
	}
	if ev.LastNoResponseNudgeDate != "" {
		t.Fatal("should not have recorded a nudge date yet")
	}

	at9am := time.Date(2026, 8, 24, 9, 0, 0, 0, loc).Unix()
	if !m.maybeSendDailyNudges(ev, group, map[string]int64{}, at9am) {
		t.Fatal("expected a nudge at 9am")
	}
	if ev.LastNoResponseNudgeDate != "2026-08-24" {
		t.Fatalf("expected the nudge date to be recorded, got %q", ev.LastNoResponseNudgeDate)
	}

	laterSameDay := time.Date(2026, 8, 24, 15, 0, 0, 0, loc).Unix()
	if m.maybeSendDailyNudges(ev, group, map[string]int64{}, laterSameDay) {
		t.Fatal("should not nudge a second time on the same day")
	}

	nextDay := time.Date(2026, 8, 25, 9, 0, 0, 0, loc).Unix()
	if !m.maybeSendDailyNudges(ev, group, map[string]int64{}, nextDay) {
		t.Fatal("expected a nudge again the next day")
	}
}

func TestMaybeSendDailyNudgesNoOpWhenEveryoneResponded(t *testing.T) {
	m := NewManager(setupTestStorage(t, ""))
	ev := &Event{Confirmations: map[string]string{"@alice": "💪"}}
	if m.maybeSendDailyNudges(ev, Group{}, map[string]int64{}, time.Now().Unix()) {
		t.Fatal("should not nudge when nobody is pending")
	}
}

func TestSend24hCalloutIsSafeWithNoBotConfigured(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	chatIDStr := fmt.Sprintf("%d", testGroupChatID)

	// Must not panic whether or not there is anyone to call out, even with
	// m.bot left nil (no SetBot call, as in every other test here).
	m.send24hCallout(chatIDStr, Event{Summary: "Test", Confirmations: map[string]string{"@alice": "💪"}})
	m.send24hCallout(chatIDStr, Event{Summary: "Test", Confirmations: map[string]string{"@alice": "❔"}})
}

// End-to-end through the real tick: an event 23h59m out with a pending
// responder must have the callout latch set after one tick, and it must stay
// set (not refire) on the next.
// An event scheduled with less than 24h notice has no "24 hours before" left
// to warn anyone at by the time it exists -- the callout must be pre-latched
// at creation so it never fires and calls people out for not yet answering an
// invite they may have received minutes ago.
func TestCreatePreLatchesThe24hCalloutForShortNoticeEvents(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	loc := mustLoad(t, "America/Sao_Paulo")

	soon := time.Now().Add(2 * time.Hour).In(loc).Format("2006-01-02T15:04")
	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": soon,
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if !events[fmt.Sprintf("%d", testGroupChatID)].Reminder24hCalloutSent {
		t.Fatal("expected the 24h callout to be pre-latched for a 2-hours-out event")
	}
}

// An event scheduled with a full day or more of notice must NOT be
// pre-latched -- the callout should still fire normally at its 24-hour mark.
func TestCreateDoesNotPreLatchThe24hCalloutForNormalNoticeEvents(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)
	loc := mustLoad(t, "America/Sao_Paulo")

	// Comfortably more than 24h out regardless of what time "now" is.
	wellAhead := time.Now().Add(48 * time.Hour).In(loc).Format("2006-01-02T15:04")
	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": wellAhead,
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if events[fmt.Sprintf("%d", testGroupChatID)].Reminder24hCalloutSent {
		t.Fatal("should not pre-latch the 24h callout for an event scheduled well over a day out")
	}
}

func TestRunMonitorTickLatches24hCalloutOnce(t *testing.T) {
	storageClient := setupTestStorage(t, "America/Sao_Paulo")
	m := NewManager(storageClient)

	chatIDStr := fmt.Sprintf("%d", testGroupChatID)
	ev := Event{
		Date:          "amanhã",
		Timestamp:     time.Now().Add(23*time.Hour + 59*time.Minute).Unix(),
		Summary:       "Sessão de teste",
		Confirmations: map[string]string{"@alice": "💪", "@bob": "❔"},
	}
	if err := storageClient.SaveToDB(eventsFileName, map[string]Event{chatIDStr: ev}); err != nil {
		t.Fatalf("failed to seed event: %v", err)
	}

	m.runMonitorTick(context.Background())

	events := make(map[string]Event)
	storageClient.LoadFromDB(eventsFileName, &events)
	if !events[chatIDStr].Reminder24hCalloutSent {
		t.Fatal("expected the 24h callout latch to be set after the first tick past the 24h mark")
	}

	m.runMonitorTick(context.Background())
	storageClient.LoadFromDB(eventsFileName, &events)
	if !events[chatIDStr].Reminder24hCalloutSent {
		t.Fatal("latch should remain set on a later tick")
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	return loc
}
