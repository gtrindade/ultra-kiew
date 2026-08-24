package event

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

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	return loc
}
