package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
)

const (
	adminChatID = int64(111111)
	userChatID  = int64(222222)
)

// adminRecorder wires a recorder with one admin and a users.json that knows
// everyone these tests refer to, keyed by the private chat ID that identifies
// them.
func adminRecorder(t *testing.T) *Recorder {
	t.Helper()
	r := setupRecorder(t)
	r.SetAdmins([]string{"@guilhermetmg"})
	if err := r.storage.SaveToDB(usersFileName, map[string]int64{
		"@guilhermetmg": adminChatID,
		"@bmaraujo":     userChatID,
	}); err != nil {
		t.Fatalf("could not seed users: %v", err)
	}
	return r
}

func grantArgs(callerChatID int64, extra map[string]any) map[string]any {
	args := map[string]any{
		googlegenai.ArgCallerChatID: callerChatID,
		googlegenai.ArgIsPrivate:    true,
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestAnAdminCanGrantQuota(t *testing.T) {
	r := adminRecorder(t)
	for range DailyPromptLimit {
		record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	}
	if r.Standing("@bmaraujo").Remaining != 0 {
		t.Fatal("setup: expected the allowance spent")
	}

	got, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(20),
	}))
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !strings.Contains(got, "@bmaraujo") || !strings.Contains(got, "20") {
		t.Errorf("expected the grant described, got %q", got)
	}

	st := r.Standing("@bmaraujo")
	if st.Remaining != 20 || st.Limit != DailyPromptLimit+20 || st.Granted != 20 {
		t.Fatalf("unexpected standing after the grant: %+v", st)
	}
}

// The whole point of the feature. Authorisation is checked in code from the
// caller's Telegram chat ID, so there is nothing the model can be talked into
// that would let a non-admin through.
func TestANonAdminCannotGrantQuota(t *testing.T) {
	r := adminRecorder(t)

	_, err := r.Grant(grantArgs(userChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(20),
	}))
	if err == nil {
		t.Fatal("a non-admin must not be able to grant quota")
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("expected the refusal to say why, got %v", err)
	}
	if r.Standing("@bmaraujo").Granted != 0 {
		t.Error("a refused grant must not have taken effect")
	}
}

// Someone the bot has never seen cannot be an admin either, however
// convincingly the request is phrased.
func TestAnUnknownCallerCannotGrantQuota(t *testing.T) {
	r := adminRecorder(t)

	if _, err := r.Grant(grantArgs(999999, map[string]any{
		"user": "@bmaraujo", "amount": float64(20),
	})); err == nil {
		t.Fatal("an unrecognised caller must not be able to grant quota")
	}
}

// In a group the chat ID says nothing about who is speaking, so admin rights
// cannot be checked -- hence group calls are refused outright rather than
// guessed at.
func TestGrantingIsRefusedInAGroup(t *testing.T) {
	r := adminRecorder(t)

	args := grantArgs(testChatID, map[string]any{"user": "@bmaraujo", "amount": float64(20)})
	args[googlegenai.ArgIsPrivate] = false

	if _, err := r.Grant(args); err == nil {
		t.Fatal("granting in a group must be refused")
	}
}

// With no admins configured, nobody can grant. An unconfigured deployment must
// not hand the power to whoever asks first.
func TestWithNoAdminsNobodyCanGrant(t *testing.T) {
	r := adminRecorder(t)
	r.SetAdmins(nil)

	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(20),
	})); err == nil {
		t.Fatal("expected no grants to be possible with no admins")
	}
}

func TestAdminMatchingIsCaseInsensitive(t *testing.T) {
	r := adminRecorder(t)
	r.SetAdmins([]string{"@GuilhermeTMG"})

	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(5),
	})); err != nil {
		t.Fatalf("handles are case-insensitive on Telegram: %v", err)
	}
}

// A grant aimed at a handle nobody owns is a silent no-op that looks like
// success -- the worst outcome here, since the admin believes they helped and
// the person keeps hitting the wall.
func TestGrantingToAnUnknownHandleIsRefused(t *testing.T) {
	r := adminRecorder(t)

	_, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@ninguem", "amount": float64(20),
	}))
	if err == nil {
		t.Fatal("expected a grant to an unknown handle to be refused")
	}
	if !strings.Contains(err.Error(), "@ninguem") {
		t.Errorf("expected the error to name the handle, got %v", err)
	}
}

func TestGrantAcceptsAHandleWithoutTheAt(t *testing.T) {
	r := adminRecorder(t)

	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "bmaraujo", "amount": float64(10),
	})); err != nil {
		t.Fatalf("a bare handle should still resolve: %v", err)
	}
	if r.Standing("@bmaraujo").Granted != 10 {
		t.Error("expected the grant to land on @bmaraujo")
	}
}

func TestGrantRejectsBadAmounts(t *testing.T) {
	r := adminRecorder(t)

	cases := []struct {
		name   string
		amount any
	}{
		{"zero", float64(0)},
		{"negative", float64(-10)},
		{"fractional", float64(1.5)},
		{"missing", nil},
		{"not a number", "muitas"},
		{"over the cap", float64(maxGrant + 1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := grantArgs(adminChatID, map[string]any{"user": "@bmaraujo"})
			if tc.amount != nil {
				args["amount"] = tc.amount
			}
			if _, err := r.Grant(args); err == nil {
				t.Fatalf("expected amount %v to be rejected", tc.amount)
			}
		})
	}

	if r.Standing("@bmaraujo").Granted != 0 {
		t.Error("no rejected grant may have taken effect")
	}
}

func TestGrantNeedsATargetUser(t *testing.T) {
	r := adminRecorder(t)

	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{"amount": float64(10)})); err == nil {
		t.Fatal("expected a missing user to be rejected")
	}
}

// Grants roll off on the same window as the usage they offset, so nobody keeps
// a raised limit forever without it being renewed.
func TestAGrantExpiresWithTheWindow(t *testing.T) {
	r := adminRecorder(t)

	r.Record(Entry{
		Timestamp: time.Now().Add(-25 * time.Hour).Unix(),
		User:      "@bmaraujo", Kind: KindGrant, Amount: 30, By: "@guilhermetmg",
	})

	st := r.Standing("@bmaraujo")
	if st.Granted != 0 || st.Limit != DailyPromptLimit {
		t.Fatalf("a grant older than the window must not still count: %+v", st)
	}
}

func TestGrantsAccumulateInsideTheWindow(t *testing.T) {
	r := adminRecorder(t)

	for range 3 {
		if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
			"user": "@bmaraujo", "amount": float64(10),
		})); err != nil {
			t.Fatalf("Grant: %v", err)
		}
	}

	if st := r.Standing("@bmaraujo"); st.Limit != DailyPromptLimit+30 {
		t.Fatalf("expected the grants to add up, got %+v", st)
	}
}

// Extra quota showing up as an unexplained 80 in a 50-limit bot would be
// confusing, so the report says where it came from.
func TestTheReportShowsGrantedQuota(t *testing.T) {
	r := adminRecorder(t)
	record(r, "@bmaraujo", KindPrompt, 5, testChatID)
	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(25),
	})); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	rep, err := r.Build(time.Now().Add(-time.Hour), time.Now(), 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := rep.Render(); !strings.Contains(got, "+25 de cota extra") {
		t.Errorf("expected the grant in the report:\n%s", got)
	}
}

// A grant is not a message and must never spend the quota it just raised.
func TestAGrantIsNotItselfAMessage(t *testing.T) {
	r := adminRecorder(t)

	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(10),
	})); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if used := r.Standing("@bmaraujo").Used; used != 0 {
		t.Fatalf("a grant must not count as usage, got %d", used)
	}
}

func TestGrantToolConfigMatchesTheDispatchName(t *testing.T) {
	tool := GetGrantToolConfig()
	decl := tool.FunctionDeclarations[0]

	if decl.Name != GrantQuotaToolName {
		t.Errorf("declared name %q does not match the dispatch key %q", decl.Name, GrantQuotaToolName)
	}
	// Who is calling is never the model's to state.
	for _, forbidden := range []string{"admin", "caller", "as_user", "granted_by"} {
		if _, exists := decl.Parameters.Properties[forbidden]; exists {
			t.Errorf("the model must not be able to claim an identity, but %q is a parameter", forbidden)
		}
	}
}

// There is no built-in admin. Authorisation comes from config.yaml alone, so
// a fallback creeping back in would silently widen who can lift a limit.
func TestThereIsNoBuiltInAdmin(t *testing.T) {
	r := setupRecorder(t)
	if err := r.storage.SaveToDB(usersFileName, map[string]int64{
		"@guilhermetmg": adminChatID,
		"@bmaraujo":     userChatID,
	}); err != nil {
		t.Fatalf("could not seed users: %v", err)
	}
	// SetAdmins deliberately never called: a fresh recorder trusts nobody.

	if len(r.Admins()) != 0 {
		t.Fatalf("a recorder with no configured admins must trust nobody, got %v", r.Admins())
	}
	if _, err := r.Grant(grantArgs(adminChatID, map[string]any{
		"user": "@bmaraujo", "amount": float64(10),
	})); err == nil {
		t.Fatal("expected the grant to be refused with no admins configured")
	}
}
