package event

import (
	"strings"
	"testing"
	"time"
)

// The production bug, reproduced exactly.
//
// The server runs in New York; the group schedules in Sao Paulo. At 18:02 ET
// the event card said 21:00, and the model answered "2 horas e 58 minutos" by
// subtracting one from the other. Both clocks were real, neither was in the
// other's zone, and the true answer was 1h58.
func TestTheCountdownIsNotFooledByTheServerTimezone(t *testing.T) {
	saoPaulo, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}

	// 19 Sep 2026, 21:00 in Sao Paulo.
	start := time.Date(2026, 9, 19, 21, 0, 0, 0, saoPaulo)
	// The same instant the group saw as 18:02 on the server's New York clock.
	now := time.Date(2026, 9, 19, 18, 2, 0, 0, newYork)

	ev := Event{
		Summary:   "Sessão de hoje",
		Date:      "Sábado, 19/09/2026 às 21:00",
		Timestamp: start.Unix(),
	}

	got := describeEventTiming(ev, Group{Timezone: "America/Sao_Paulo"}, now)

	if !strings.Contains(got, "1 hora e 58 minutos") {
		t.Fatalf("expected the real remaining time, got:\n%s", got)
	}
	// The wrong answer the model produced, and the one it produced after
	// being corrected. Neither may reappear.
	for _, wrong := range []string{"2 horas e 58", "2 horas e 57"} {
		if strings.Contains(got, wrong) {
			t.Errorf("the timezone bug is back: %q appeared in:\n%s", wrong, got)
		}
	}
}

// The absolute instant is stated alongside the human date, so anything reading
// this has an unambiguous anchor rather than a bare wall clock.
func TestTimingIncludesAnUnambiguousInstant(t *testing.T) {
	saoPaulo, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	start := time.Date(2026, 9, 19, 21, 0, 0, 0, saoPaulo)

	got := describeEventTiming(
		Event{Summary: "Sessão", Date: "Sábado, 19/09/2026 às 21:00", Timestamp: start.Unix()},
		Group{Timezone: "America/Sao_Paulo"},
		start.Add(-time.Hour),
	)

	if !strings.Contains(got, "2026-09-19T21:00:00-03:00") {
		t.Errorf("expected the instant with its offset, got:\n%s", got)
	}
	// And it must tell the model not to touch the number.
	if !strings.Contains(strings.ToLower(got), "never recompute") {
		t.Errorf("expected the figure marked authoritative, got:\n%s", got)
	}
}

func TestCountdownHandlesAnEventAlreadyUnderway(t *testing.T) {
	now := time.Now()
	ev := Event{Summary: "Sessão", Date: "hoje", Timestamp: now.Add(-90 * time.Minute).Unix()}

	got := describeEventTiming(ev, Group{}, now)
	if !strings.Contains(got, "already started") {
		t.Errorf("expected it reported as underway, got:\n%s", got)
	}
	if !strings.Contains(got, "1 hora e 30 minutos") {
		t.Errorf("expected how long ago, got:\n%s", got)
	}
}

// An event written by a build that predated timestamps has nothing to compute
// from. Saying so beats inventing a countdown, and beats letting the model
// invent one.
func TestAnEventWithNoTimestampRefusesToGuess(t *testing.T) {
	got := describeEventTiming(Event{Summary: "Antiga", Date: "Sexta às 21:00"}, Group{}, time.Now())

	if !strings.Contains(got, "no exact timestamp") {
		t.Errorf("expected the missing timestamp acknowledged, got:\n%s", got)
	}
	if !strings.Contains(got, "do NOT estimate") {
		t.Errorf("expected the model told not to estimate, got:\n%s", got)
	}
}

// A group with no timezone on file still gets a correct countdown -- the
// comparison is between instants, and the zone only decides how the start is
// rendered for a human.
func TestTheCountdownWorksWithNoTimezoneRecorded(t *testing.T) {
	// Truncated to the second: Timestamp has no sub-second part, so an
	// untruncated "now" would leave the span a fraction under three hours and
	// the countdown would honestly report 2h59.
	now := time.Now().Truncate(time.Second)
	ev := Event{Summary: "Sessão", Date: "hoje", Timestamp: now.Add(3 * time.Hour).Unix()}

	got := describeEventTiming(ev, Group{}, now)
	if !strings.Contains(got, "3 horas") {
		t.Errorf("expected a correct countdown regardless of zone, got:\n%s", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{90 * time.Minute, "1 hora e 30 minutos"},
		{118 * time.Minute, "1 hora e 58 minutos"},
		{2 * time.Hour, "2 horas"},
		{time.Hour, "1 hora"},
		{45 * time.Minute, "45 minutos"},
		{time.Minute, "1 minuto"},
		{25 * time.Hour, "1 dia e 1 hora"},
		{49 * time.Hour, "2 dias e 1 hora"},
		{48 * time.Hour, "2 dias"},
		{72*time.Hour + 30*time.Minute, "3 dias"},
	}

	for _, tc := range cases {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFormatCountdownEdges(t *testing.T) {
	if got := formatCountdown(30 * time.Second); !strings.Contains(got, "less than a minute") {
		t.Errorf("got %q", got)
	}
	if got := formatCountdown(-10 * time.Second); !strings.Contains(got, "just now") {
		t.Errorf("got %q", got)
	}
}

// The DST case that makes wall-clock arithmetic unsafe even within one zone:
// Sao Paulo's offset has been both -03:00 and -02:00, and only the date says
// which. Comparing instants sidesteps it entirely.
func TestTheCountdownIsCorrectAcrossADSTBoundary(t *testing.T) {
	saoPaulo, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}

	// Brazil moved its clocks forward on 2018-11-04.
	before := time.Date(2018, 11, 3, 21, 0, 0, 0, saoPaulo)
	after := time.Date(2018, 11, 5, 21, 0, 0, 0, saoPaulo)
	if _, o1 := before.Zone(); true {
		if _, o2 := after.Zone(); o1 == o2 {
			t.Skip("this tzdata has no DST transition here")
		}
	}

	ev := Event{Summary: "Sessão", Date: "depois", Timestamp: after.Unix()}
	got := describeEventTiming(ev, Group{Timezone: "America/Sao_Paulo"}, before)

	// 21:00 Saturday to 21:00 Monday across a spring-forward is 47 hours, not
	// 48 -- which is exactly the sort of thing nobody should be doing by hand.
	if !strings.Contains(got, "1 dia e 23 horas") {
		t.Errorf("expected the DST-aware span, got:\n%s", got)
	}
}
