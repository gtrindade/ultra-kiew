package event

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/meet"
)

// upcomingWithMeet seeds one event that has not started yet, with a Meet space
// already created, minutesAway minutes from now.
func upcomingWithMeet(t *testing.T, m *Manager, minutesAway int) Event {
	t.Helper()
	ev := Event{
		Date:          "Sexta-feira, 10/04/2026 às 21:00",
		Timestamp:     time.Now().Add(time.Duration(minutesAway) * time.Minute).Unix(),
		Summary:       "Sessão 12",
		MessageID:     4242,
		Confirmations: map[string]string{"@alice": "💪"},
		Meet:          &MeetInfo{SpaceName: "spaces/abc", JoinURI: "https://meet.google.com/abc"},
	}
	key := fmt.Sprintf("%d", testGroupChatID)
	if err := m.storage.SaveToDB(eventsFileName, map[string]Event{key: ev}); err != nil {
		t.Fatalf("could not seed the event: %v", err)
	}
	return ev
}

func loadEvent(t *testing.T, m *Manager) Event {
	t.Helper()
	events := make(map[string]Event)
	if err := m.storage.LoadFromDB(eventsFileName, &events); err != nil {
		t.Fatalf("could not load events: %v", err)
	}
	return events[fmt.Sprintf("%d", testGroupChatID)]
}

func TestEarlyWatchNoticesSomeoneWaitingBeforeTheEvent(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	fake := &fakeMeet{
		records: []meet.ConferenceRecord{{Name: "conferenceRecords/1", StartTime: time.Now().Format(time.RFC3339)}},
		participants: map[string][]meet.Participant{
			"conferenceRecords/1": {{Name: "participants/1", DisplayName: "Guilherme", Present: true}},
		},
	}
	m.SetMeet(fake)
	upcomingWithMeet(t, m, 20) // inside the 30-minute window

	m.runMonitorTick(context.Background())

	got := loadEvent(t, m)
	if got.Meet == nil || len(got.Meet.Participants) != 1 {
		t.Fatalf("expected the early arrival to be recorded, got %+v", got.Meet)
	}
	if !got.Meet.Participants[0].Present {
		t.Error("expected the participant to be marked present")
	}
	// Critically, the event has NOT started.
	if got.Meet.SessionEnded {
		t.Error("watching early must never end the session")
	}
}

// Outside the window nothing is polled at all, so an event scheduled for
// tomorrow does not spend an API call every minute for a day.
func TestEarlyWatchDoesNothingOutsideTheWindow(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	fake := &fakeMeet{
		records: []meet.ConferenceRecord{{Name: "conferenceRecords/1"}},
		participants: map[string][]meet.Participant{
			"conferenceRecords/1": {{Name: "participants/1", DisplayName: "Guilherme", Present: true}},
		},
	}
	m.SetMeet(fake)
	upcomingWithMeet(t, m, 90) // well outside

	m.runMonitorTick(context.Background())

	if fake.participantsCalls != 0 {
		t.Errorf("expected no participant polling 90 minutes out, got %d calls", fake.participantsCalls)
	}
	if got := loadEvent(t, m); got.Meet != nil && len(got.Meet.Participants) != 0 {
		t.Errorf("expected nobody tracked yet, got %+v", got.Meet.Participants)
	}
}

// The bug this design exists to avoid. Someone drops in early and leaves, so
// their conference record closes. The "all records ended, grace period
// elapsed" rule would call the session over -- before it had started -- and
// because RecapPosted latches, that verdict could never be corrected.
func TestAnEarlyJoinAndLeaveDoesNotEndTheSessionBeforeItStarts(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))

	// A record that opened and closed an hour ago: well past meetGracePeriod.
	closed := time.Now().Add(-time.Hour)
	m.SetMeet(&fakeMeet{
		records: []meet.ConferenceRecord{{
			Name:      "conferenceRecords/1",
			StartTime: closed.Format(time.RFC3339),
			EndTime:   closed.Add(5 * time.Minute).Format(time.RFC3339),
		}},
	})
	upcomingWithMeet(t, m, 10) // starts in 10 minutes

	m.runMonitorTick(context.Background())

	got := loadEvent(t, m)
	if got.Meet == nil {
		t.Fatal("the event should still be upcoming with its Meet info")
	}
	if got.Meet.SessionEnded {
		t.Fatal("a closed early record must not end a session that has not started")
	}
	if got.Meet.RecapPosted {
		t.Fatal("no recap may be posted before the event has even begun")
	}
}

// Segments found early carry into the live session, so the lifecycle does not
// have to rediscover them and the participant state is already warm.
func TestSegmentsFoundEarlyCarryIntoTheLiveSession(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	m.SetMeet(&fakeMeet{
		records: []meet.ConferenceRecord{{
			Name:      "conferenceRecords/1",
			StartTime: time.Now().Format(time.RFC3339),
		}},
		participants: map[string][]meet.Participant{
			"conferenceRecords/1": {{Name: "participants/1", DisplayName: "Guilherme", Present: true}},
		},
	})
	upcomingWithMeet(t, m, 5)

	m.runMonitorTick(context.Background())

	got := loadEvent(t, m)
	if len(got.Meet.Segments) != 1 || got.Meet.Segments[0].RecordName != "conferenceRecords/1" {
		t.Fatalf("expected the record discovered early, got %+v", got.Meet.Segments)
	}
	if len(got.Meet.Participants) != 1 {
		t.Fatalf("expected the participant carried, got %+v", got.Meet.Participants)
	}
}

// Nothing about early watching may move the event out of the upcoming slot:
// create() reads only that map, and an event parked in live-sessions early
// would block scheduling for a session that has not happened.
func TestEarlyWatchLeavesTheEventUpcoming(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	m.SetMeet(&fakeMeet{
		records: []meet.ConferenceRecord{{Name: "conferenceRecords/1"}},
		participants: map[string][]meet.Participant{
			"conferenceRecords/1": {{Name: "participants/1", DisplayName: "G", Present: true}},
		},
	})
	upcomingWithMeet(t, m, 15)

	m.runMonitorTick(context.Background())

	events := make(map[string]Event)
	if err := m.storage.LoadFromDB(eventsFileName, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("the event must stay in the upcoming slot, got %d", len(events))
	}

	live := make(map[string][]Event)
	if err := m.storage.LoadFromDB(liveSessionsFileName, &live); err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("nothing should be live yet, got %+v", live)
	}
}

// With no Meet client there is simply nothing to watch, and the tick must not
// trip over it.
func TestEarlyWatchIsSafeWithNoMeetConfigured(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	upcomingWithMeet(t, m, 10)

	m.runMonitorTick(context.Background()) // must not panic

	if got := loadEvent(t, m); got.Summary != "Sessão 12" {
		t.Fatalf("expected the event untouched, got %+v", got)
	}
}

func TestWatchEarlyArrivalsIgnoresAnEndedSession(t *testing.T) {
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	fake := &fakeMeet{
		records: []meet.ConferenceRecord{{Name: "conferenceRecords/1"}},
		participants: map[string][]meet.Participant{
			"conferenceRecords/1": {{Name: "participants/1", DisplayName: "G", Present: true}},
		},
	}
	m.SetMeet(fake)

	ev := Event{
		Timestamp: time.Now().Add(10 * time.Minute).Unix(),
		Meet:      &MeetInfo{SpaceName: "spaces/abc", SessionEnded: true},
	}
	if m.watchEarlyArrivals(context.Background(), "-100", ev) {
		t.Error("an ended session has nothing left to watch")
	}
	if fake.participantsCalls != 0 {
		t.Errorf("expected no polling for an ended session, got %d", fake.participantsCalls)
	}
}
