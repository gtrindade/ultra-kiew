package event

import (
	"strings"
	"testing"
	"time"
)

// The model is asked for a plain wall-clock time and a zone name, and the
// offset is computed here. A value that arrives carrying its own offset means
// the model misunderstood, and honouring it quietly is how an event asked for
// in BRT ended up booked in EDT -- so it is rejected loudly instead.
func TestParseLocalDateTimeRejectsAnythingCarryingAnOffset(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}

	for _, value := range []string{
		"2026-04-10T21:00:00Z",
		"2026-04-10T21:00-03:00",
		"2026-04-10T21:00:00+05:30",
		"2026-04-10T21:00Z",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := parseLocalDateTime(value, loc)
			if err == nil {
				t.Fatalf("expected %q to be rejected", value)
			}
			// The error is fed back to the model, so it has to say what to
			// send instead rather than just "invalid".
			if !strings.Contains(err.Error(), "timezone") {
				t.Errorf("expected the error to explain the rule, got: %v", err)
			}
		})
	}
}

func TestParseLocalDateTimeAcceptsTheSpellingsTheModelActuallySends(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}

	// One shape is asked for; these are the ones that turn up anyway.
	for _, value := range []string{
		"2026-04-10T21:00",
		"2026-04-10T21:00:00",
		"2026-04-10 21:00",
		"2026-04-10 21:00:00",
		"  2026-04-10T21:00  ",
	} {
		t.Run(value, func(t *testing.T) {
			got, err := parseLocalDateTime(value, loc)
			if err != nil {
				t.Fatalf("parseLocalDateTime(%q): %v", value, err)
			}
			if got.Hour() != 21 || got.Minute() != 0 {
				t.Errorf("expected 21:00, got %s", got)
			}
			if got.Location() != loc {
				t.Errorf("expected the time to be in %s, got %s", loc, got.Location())
			}
		})
	}
}

func TestParseLocalDateTimeRejectsGarbage(t *testing.T) {
	loc := time.UTC

	for _, value := range []string{"", "amanha", "2026-04-10", "10/04/2026 21:00", "21:00"} {
		if _, err := parseLocalDateTime(value, loc); err == nil {
			t.Errorf("expected %q to be rejected", value)
		}
	}
}

// The reason offsets are computed rather than accepted: Sao Paulo's offset has
// been both -03:00 and -02:00 historically, and only the date decides which.
// A model guessing an offset has no way to know.
func TestParseLocalDateTimeResolvesTheOffsetForTheGivenDate(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}

	// 2018-01-15 fell inside Brazilian DST (-02:00); 2018-07-15 did not (-03:00).
	summer, err := parseLocalDateTime("2018-01-15T21:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	winter, err := parseLocalDateTime("2018-07-15T21:00", loc)
	if err != nil {
		t.Fatal(err)
	}

	_, summerOffset := summer.Zone()
	_, winterOffset := winter.Zone()
	if summerOffset == winterOffset {
		t.Fatalf("expected the offset to differ across the DST boundary, both were %d", summerOffset)
	}
	// Both still read as 21:00 locally -- that is the point of storing a wall
	// clock plus a zone.
	if summer.Hour() != 21 || winter.Hour() != 21 {
		t.Errorf("expected both to be 21:00 locally, got %s and %s", summer, winter)
	}
}

func TestResolveTimezoneRejectsUnknownZones(t *testing.T) {
	for _, tz := range []string{"", "   ", "Marte/Olympus", "XYZ"} {
		if _, err := resolveTimezone(tz); err == nil {
			t.Errorf("expected %q to be rejected", tz)
		}
	}
}

func TestFormatPTBRDateNamesTheWeekdayInPortuguese(t *testing.T) {
	cases := []struct {
		date time.Time
		want string
	}{
		{time.Date(2026, 4, 10, 21, 0, 0, 0, time.UTC), "Sexta-feira, 10/04/2026 às 21:00"},
		{time.Date(2026, 4, 11, 9, 30, 0, 0, time.UTC), "Sábado, 11/04/2026 às 09:30"},
		{time.Date(2026, 4, 12, 0, 5, 0, 0, time.UTC), "Domingo, 12/04/2026 às 00:05"},
	}

	for _, tc := range cases {
		if got := formatPTBRDate(tc.date); got != tc.want {
			t.Errorf("formatPTBRDate(%s) = %q, want %q", tc.date, got, tc.want)
		}
	}
}

// The card is the group's shared source of truth about who is coming, so
// somebody in the roster with no recorded answer has to show as unanswered
// rather than disappearing from the list.
func TestRenderEventTextShowsEveryInvitedUser(t *testing.T) {
	got := renderEventText("Sessão 12", "Sexta-feira, 10/04/2026 às 21:00",
		[]string{"@alice", "@bob", "@carol"},
		map[string]string{"@alice": "💪", "@bob": ""},
	)

	if !strings.HasPrefix(got, "Sessão 12 - Sexta-feira, 10/04/2026 às 21:00") {
		t.Errorf("unexpected header: %q", got)
	}
	for _, want := range []string{"@alice 💪", "@bob ❔", "@carol ❔"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

// Someone removed from the group must not linger on the card just because an
// old answer for them is still in the map.
func TestRenderEventTextIgnoresAnswersFromNonMembers(t *testing.T) {
	got := renderEventText("Sessão", "hoje", []string{"@alice"},
		map[string]string{"@alice": "💪", "@expulso": "🐔"})

	if strings.Contains(got, "@expulso") {
		t.Errorf("a non-member appeared on the card:\n%s", got)
	}
}

func TestLateSuffixExtractsTheEstimateWhenThereIsOne(t *testing.T) {
	cases := map[string]string{
		"🐢":             "",
		"🐢 (10 min)":    "(10 min)",
		"🐢 (meia hora)": "(meia hora)",
		"💪":             "",
	}

	for conf, want := range cases {
		if got := lateSuffix(conf); got != want {
			t.Errorf("lateSuffix(%q) = %q, want %q", conf, got, want)
		}
	}
}

// An empty Go slice printed with %v reads as "[]", which a model with no other
// signal does not reliably understand as "nobody".
func TestJoinOrNenhum(t *testing.T) {
	if got := joinOrNenhum(nil); got != "ninguém" {
		t.Errorf("empty list = %q, want %q", got, "ninguém")
	}
	if got := joinOrNenhum([]string{}); got != "ninguém" {
		t.Errorf("empty slice = %q, want %q", got, "ninguém")
	}
	if got := joinOrNenhum([]string{"@alice", "@bob"}); got != "@alice, @bob" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeConfirmationRendersEachStateAsProse(t *testing.T) {
	cases := map[string]string{
		"💪":          "confirmado",
		"🐢":          "atrasado",
		"🐢 (15 min)": "atrasado (15 min)",
		"🐔":          "não vai",
		"❔":          "incerto",
		"":           "incerto",
		"🤷":          "sem resposta",
	}

	for conf, want := range cases {
		if got := describeConfirmation(conf); got != want {
			t.Errorf("describeConfirmation(%q) = %q, want %q", conf, got, want)
		}
	}
}

// severity and describeConfirmation have to agree about what counts as a
// commitment, or a status change gets a tone that contradicts its own words.
func TestSeverityAndDescriptionAgreeOnWithdrawal(t *testing.T) {
	// Going from "confirmado" to "incerto" reads as a step back in prose, and
	// must rank as one too -- that is what earns the light jab rather than the
	// neutral report.
	if confirmationSeverity("❔") >= confirmationSeverity("💪") {
		t.Error("withdrawing to unsure must rank below confirmed")
	}
	if confirmationSeverity("❔") != confirmationSeverity("🐔") {
		t.Error("unsure and not-coming both leave one fewer person actually coming")
	}
	if confirmationSeverity("🐢 (10 min)") != confirmationSeverity("🐢 (20 min)") {
		t.Error("changing a late estimate is the same commitment, not a flip-flop")
	}
}
