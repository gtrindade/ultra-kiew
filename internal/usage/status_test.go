package usage

import (
	"strings"
	"testing"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
)

func statusArgs(callerChatID int64, extra map[string]any) map[string]any {
	args := map[string]any{
		googlegenai.ArgCallerChatID: callerChatID,
		googlegenai.ArgIsPrivate:    true,
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// Whoever can lift everyone else's limit gains nothing from having one, and
// being locked out by it would leave nobody able to unlock it.
func TestAnAdminIsNeverRationed(t *testing.T) {
	r := adminRecorder(t)
	for range DailyPromptLimit * 3 {
		record(r, "@guilhermetmg", KindPrompt, 5, testChatID)
	}

	st := r.Standing("@guilhermetmg")
	if !st.Unlimited {
		t.Fatal("an admin must be unlimited")
	}
	if !st.Allowed() {
		t.Fatal("an admin must always be allowed to send")
	}
	// The count is still tracked -- unlimited is not untracked.
	if st.Used != DailyPromptLimit*3 {
		t.Errorf("expected usage still counted, got %d", st.Used)
	}
}

func TestANonAdminIsStillRationed(t *testing.T) {
	r := adminRecorder(t)
	for range DailyPromptLimit {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}

	st := r.Standing("@bmaraujo")
	if st.Unlimited {
		t.Fatal("a normal user must not be unlimited")
	}
	if st.Allowed() {
		t.Fatal("a normal user at the limit must be refused")
	}
}

// Losing admin status must restore the limit rather than leaving someone
// grandfathered in.
func TestUnlimitedFollowsTheConfiguredAdminList(t *testing.T) {
	r := adminRecorder(t)
	for range DailyPromptLimit {
		record(r, "@guilhermetmg", KindPrompt, 5, testChatID)
	}
	if !r.Standing("@guilhermetmg").Allowed() {
		t.Fatal("setup: expected the admin to be unlimited")
	}

	r.SetAdmins(nil)
	if r.Standing("@guilhermetmg").Allowed() {
		t.Fatal("someone who is no longer an admin must be rationed again")
	}
}

func TestAnAdminSeesEveryonesQuota(t *testing.T) {
	r := adminRecorder(t)
	for range 12 {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}
	record(r, "@guilhermetmg", KindPrompt, 5, testChatID)

	got, err := r.QuotaStatus(statusArgs(adminChatID, nil))
	if err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	for _, want := range []string{"@bmaraujo", "12", "@guilhermetmg", "sem limite"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

func TestAnAdminCanCheckOnePerson(t *testing.T) {
	r := adminRecorder(t)
	for range 7 {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}

	got, err := r.QuotaStatus(statusArgs(adminChatID, map[string]any{"user": "@bmaraujo"}))
	if err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	if !strings.Contains(got, "@bmaraujo") || !strings.Contains(got, "7") {
		t.Errorf("expected one person's standing, got %q", got)
	}
	// One person asked for means one person answered about.
	if strings.Contains(got, "@guilhermetmg") {
		t.Errorf("expected only the named user, got %q", got)
	}
}

// Not a secrecy rule so much as a scope one: a tool that answered for any
// handle would let the model be talked into a roll call by whoever asked.
func TestANonAdminCannotLookUpSomebodyElse(t *testing.T) {
	r := adminRecorder(t)

	_, err := r.QuotaStatus(statusArgs(userChatID, map[string]any{"user": "@guilhermetmg"}))
	if err == nil {
		t.Fatal("a normal user must not be able to look up another person")
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("expected the refusal to say why, got %v", err)
	}
}

func TestANonAdminCanCheckTheirOwn(t *testing.T) {
	r := adminRecorder(t)
	for range 9 {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}

	got, err := r.QuotaStatus(statusArgs(userChatID, map[string]any{"user": "@bmaraujo"}))
	if err != nil {
		t.Fatalf("checking your own quota should work: %v", err)
	}
	if !strings.Contains(got, "9") {
		t.Errorf("expected their own usage, got %q", got)
	}
}

// Asking for "everyone" as a normal user answers about them and says why,
// rather than erroring -- the question is reasonable, only its scope is not.
func TestANonAdminAskingForEveryoneGetsThemselves(t *testing.T) {
	r := adminRecorder(t)
	for range 4 {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}
	record(r, "@guilhermetmg", KindPrompt, 5, testChatID)

	got, err := r.QuotaStatus(statusArgs(userChatID, nil))
	if err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	if !strings.Contains(got, "@bmaraujo") {
		t.Errorf("expected their own standing, got %q", got)
	}
	if strings.Contains(got, "@guilhermetmg") {
		t.Errorf("a normal user must not see anyone else, got %q", got)
	}
}

func TestQuotaStatusIsRefusedInAGroup(t *testing.T) {
	r := adminRecorder(t)

	args := statusArgs(adminChatID, nil)
	args[googlegenai.ArgIsPrivate] = false

	if _, err := r.QuotaStatus(args); err == nil {
		t.Fatal("expected a group call to be refused")
	}
}

func TestQuotaStatusNeedsAKnownCaller(t *testing.T) {
	r := adminRecorder(t)

	if _, err := r.QuotaStatus(statusArgs(999999, nil)); err == nil {
		t.Fatal("expected an unrecognised caller to be refused")
	}
}

func TestQuotaStatusAcceptsAHandleWithoutTheAt(t *testing.T) {
	r := adminRecorder(t)
	record(r, "@bmaraujo", KindPrompt, 5, testChatID)

	got, err := r.QuotaStatus(statusArgs(adminChatID, map[string]any{"user": "bmaraujo"}))
	if err != nil {
		t.Fatalf("a bare handle should resolve: %v", err)
	}
	if !strings.Contains(got, "@bmaraujo") {
		t.Errorf("expected the handle normalised, got %q", got)
	}
}

// Granted quota has to show up here, or "restam 30" would contradict a limit
// of 50 with no explanation.
func TestQuotaStatusShowsGrantedQuota(t *testing.T) {
	r := adminRecorder(t)
	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(25),
	})); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	got, err := r.QuotaStatus(statusArgs(adminChatID, map[string]any{"user": "@bmaraujo"}))
	if err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	if !strings.Contains(got, "75") || !strings.Contains(got, "+25") {
		t.Errorf("expected the raised limit and its source, got %q", got)
	}
}

// The roll call is built from users.json plus anyone active in the window, so
// somebody the bot has seen but who has not spoken today still appears.
func TestEveryoneIncludesQuietUsers(t *testing.T) {
	r := adminRecorder(t)
	record(r, "@bmaraujo", KindPrompt, 5, testChatID)

	got, err := r.QuotaStatus(statusArgs(adminChatID, nil))
	if err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	// @guilhermetmg is in users.json but has sent nothing in the window.
	if !strings.Contains(got, "@guilhermetmg") {
		t.Errorf("expected quiet users listed too:\n%s", got)
	}
}

func TestQuotaStatusToolConfigMatchesTheDispatchName(t *testing.T) {
	tool := GetQuotaStatusToolConfig()
	decl := tool.FunctionDeclarations[0]

	if decl.Name != QuotaStatusToolName {
		t.Errorf("declared name %q does not match the dispatch key %q", decl.Name, QuotaStatusToolName)
	}
	if _, exists := decl.Parameters.Properties["user"]; !exists {
		t.Error("expected a user parameter")
	}
	// Identity and scope are the code's to decide, never the model's.
	for _, forbidden := range []string{"admin", "caller", "all", "everyone"} {
		if _, exists := decl.Parameters.Properties[forbidden]; exists {
			t.Errorf("the model must not be able to claim scope, but %q is a parameter", forbidden)
		}
	}
}
