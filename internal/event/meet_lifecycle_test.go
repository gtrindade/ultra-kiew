package event

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/meet"
)

// fakeMeet is a scriptable MeetClient: each field is a canned answer, and
// CreateSpaceCalls/other counters let a test assert on retry behaviour without
// touching the network.
type fakeMeet struct {
	createErr        error
	spaceName        string
	joinURI          string
	createSpaceCalls int

	records []meet.ConferenceRecord
	listErr error

	transcriptLinks     map[string][]string // by conference record name
	notesLinks          map[string][]string
	transcriptLinkCalls int
	notesLinkCalls      int
}

func (f *fakeMeet) CreateSpace(ctx context.Context) (string, string, error) {
	f.createSpaceCalls++
	if f.createErr != nil {
		return "", "", f.createErr
	}
	return f.spaceName, f.joinURI, nil
}

func (f *fakeMeet) ListConferenceRecords(ctx context.Context, spaceName string) ([]meet.ConferenceRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.records, nil
}

func (f *fakeMeet) TranscriptLinks(ctx context.Context, recordName string) ([]string, error) {
	f.transcriptLinkCalls++
	return f.transcriptLinks[recordName], nil
}

func (f *fakeMeet) SmartNotesLinks(ctx context.Context, recordName string) ([]string, error) {
	f.notesLinkCalls++
	return f.notesLinks[recordName], nil
}

func newTestManager(t *testing.T, fm *fakeMeet) *Manager {
	t.Helper()
	m := NewManager(setupTestStorage(t, "America/Sao_Paulo"))
	m.meet = fm
	return m
}

// A session still running (no end time on its one record) must not finalize,
// no matter how many ticks pass -- otherwise a recap would post mid-session.
func TestAdvanceMeetSessionStaysLiveWhileRecordIsOpen(t *testing.T) {
	fm := &fakeMeet{records: []meet.ConferenceRecord{{Name: "conferenceRecords/abc", StartTime: time.Now().Format(time.RFC3339)}}}
	m := newTestManager(t, fm)

	ev := Event{
		Summary:   "Sessão de teste",
		Timestamp: time.Now().Add(-1 * time.Hour).Unix(), // well under the hard cap
		Meet:      &MeetInfo{SpaceName: "spaces/xyz"},
	}
	now := time.Now().Unix()

	updated, finalized, changed := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if finalized {
		t.Fatal("session should not finalize while a conference record is still open")
	}
	if !changed {
		t.Fatal("expected the new conference record to be persisted")
	}
	if len(updated.Meet.ConferenceRecords) != 1 {
		t.Fatalf("expected 1 conference record recorded, got %v", updated.Meet.ConferenceRecords)
	}
}

// The whole point of the grace period: a record ending must not immediately
// finalize the session, because a pizza break looks identical at that instant
// to the session actually being over.
func TestAdvanceMeetSessionWaitsOutGracePeriodAfterRecordEnds(t *testing.T) {
	recordName := "conferenceRecords/abc"
	endTime := time.Now().Add(-5 * time.Minute) // ended recently, inside the grace window
	fm := &fakeMeet{records: []meet.ConferenceRecord{{Name: recordName, EndTime: endTime.Format(time.RFC3339)}}}
	m := newTestManager(t, fm)

	ev := Event{
		Summary:   "Sessão de teste",
		Timestamp: time.Now().Add(-1 * time.Hour).Unix(), // well under the hard cap
		Meet:      &MeetInfo{SpaceName: "spaces/xyz"},
	}
	now := time.Now().Unix()

	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if finalized {
		t.Fatal("session should not finalize before the grace period has elapsed")
	}
	if updated.Meet.SessionEnded {
		t.Fatal("SessionEnded should not latch until the grace period has elapsed")
	}
}

// Past the grace period, with no reconnect, the session is over and the recap
// step should run (and, with no links available, finalize immediately rather
// than waiting the full artifact timeout for nothing).
func TestAdvanceMeetSessionFinalizesAfterGracePeriodWithNoRecap(t *testing.T) {
	recordName := "conferenceRecords/abc"
	endTime := time.Now().Add(-(meetGracePeriod + time.Minute))
	fm := &fakeMeet{
		records:         []meet.ConferenceRecord{{Name: recordName, EndTime: endTime.Format(time.RFC3339)}},
		transcriptLinks: map[string][]string{recordName: {"https://docs.google.com/transcript"}},
	}
	m := newTestManager(t, fm)

	ev := Event{Summary: "Sessão de teste", Meet: &MeetInfo{SpaceName: "spaces/xyz"}}
	now := time.Now().Unix()

	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if !finalized {
		t.Fatal("expected the session to finalize once the grace period has passed and a link was found")
	}
	if !updated.Meet.RecapPosted {
		t.Fatal("expected RecapPosted to be set")
	}
	if len(updated.Meet.TranscriptLinks) != 1 {
		t.Fatalf("expected the transcript link to be recorded, got %v", updated.Meet.TranscriptLinks)
	}
}

// If nobody ever joins, ConferenceRecords stays empty forever -- this must
// still finalize eventually rather than tracking the event indefinitely.
func TestAdvanceMeetSessionFinalizesOnNoShow(t *testing.T) {
	fm := &fakeMeet{records: nil}
	m := newTestManager(t, fm)

	ev := Event{
		Summary:   "Sessão de teste",
		Timestamp: time.Now().Add(-(meetNoShowGrace + time.Minute)).Unix(),
		Meet:      &MeetInfo{SpaceName: "spaces/xyz"},
	}
	now := time.Now().Unix()

	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if !finalized {
		t.Fatal("expected a no-show session to finalize after meetNoShowGrace")
	}
	if !updated.Meet.RecapPosted {
		t.Fatal("expected a recap (even an empty one) to be posted")
	}
}

// A record that never gets an end time must not pin the event forever.
func TestAdvanceMeetSessionHitsHardCap(t *testing.T) {
	recordName := "conferenceRecords/stuck"
	fm := &fakeMeet{records: []meet.ConferenceRecord{{Name: recordName}}} // no EndTime, ever
	m := newTestManager(t, fm)

	ev := Event{
		Summary:   "Sessão de teste",
		Timestamp: time.Now().Add(-(meetMaxSessionLength + time.Minute)).Unix(),
		Meet:      &MeetInfo{SpaceName: "spaces/xyz", ConferenceRecords: []string{recordName}},
	}
	now := time.Now().Unix()

	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if !finalized {
		t.Fatal("expected the hard cap to force finalization of a stuck session")
	}
	if !updated.Meet.SessionEnded {
		t.Fatal("expected SessionEnded to be set by the hard cap")
	}
}

// A long session (these run up to several hours) must not have its transcript
// check give up after 30 minutes -- it must keep trying for up to
// meetArtifactMaxWait (24h), and it must not do so by hammering the API once a
// minute for that whole window: the check should only actually run once every
// meetArtifactPollInterval.
func TestAdvanceMeetSessionPacesArtifactChecksRatherThanPollingEveryTick(t *testing.T) {
	fm := &fakeMeet{}
	m := newTestManager(t, fm)

	now := time.Now().Unix()
	ev := Event{
		Summary: "Sessão longa",
		Meet: &MeetInfo{
			SpaceName:         "spaces/xyz",
			ConferenceRecords: []string{"conferenceRecords/abc"},
			SessionEnded:      true,
			SessionEndedAt:    now - 60, // ended a minute ago
		},
	}

	// First tick after the session ends: no LastArtifactPollAt yet, so the
	// check must run immediately rather than waiting a full interval.
	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)
	if finalized {
		t.Fatal("should still be waiting, well within meetArtifactMaxWait")
	}
	if fm.transcriptLinkCalls != 1 || fm.notesLinkCalls != 1 {
		t.Fatalf("expected exactly one check on the first tick, got %d/%d", fm.transcriptLinkCalls, fm.notesLinkCalls)
	}
	if updated.Meet.LastArtifactPollAt != now {
		t.Fatalf("expected LastArtifactPollAt to be recorded, got %d", updated.Meet.LastArtifactPollAt)
	}

	// A tick 1 minute later (as the real ticker runs) must NOT check again --
	// meetArtifactPollInterval has not elapsed.
	soonAfter := now + 60
	updated, _, _ = m.advanceMeetSession(context.Background(), "-100", updated, soonAfter)
	if fm.transcriptLinkCalls != 1 || fm.notesLinkCalls != 1 {
		t.Fatalf("expected no additional check before the poll interval elapses, got %d/%d", fm.transcriptLinkCalls, fm.notesLinkCalls)
	}

	// Once meetArtifactPollInterval has actually elapsed, it should check again.
	afterInterval := now + int64(meetArtifactPollInterval.Seconds()) + 1
	updated, _, _ = m.advanceMeetSession(context.Background(), "-100", updated, afterInterval)
	if fm.transcriptLinkCalls != 2 || fm.notesLinkCalls != 2 {
		t.Fatalf("expected a second check after the poll interval elapsed, got %d/%d", fm.transcriptLinkCalls, fm.notesLinkCalls)
	}
	if updated.Meet.LastArtifactPollAt != afterInterval {
		t.Fatalf("expected LastArtifactPollAt to advance, got %d", updated.Meet.LastArtifactPollAt)
	}
}

// Once the session has ended but no links have shown up yet, the event must
// stay live (not finalize) until either a link appears or the artifact wait
// expires -- otherwise a recap could be posted with no chance for the
// transcript to land.
func TestAdvanceMeetSessionWaitsForArtifactsBeforeGivingUp(t *testing.T) {
	fm := &fakeMeet{}
	m := newTestManager(t, fm)

	now := time.Now().Unix()
	ev := Event{
		Summary: "Sessão de teste",
		Meet: &MeetInfo{
			SpaceName:         "spaces/xyz",
			ConferenceRecords: []string{"conferenceRecords/abc"},
			SessionEnded:      true,
			SessionEndedAt:    now - int64((meetArtifactMaxWait - time.Minute).Seconds()),
		},
	}

	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if finalized {
		t.Fatal("should still be waiting for artifacts within meetArtifactMaxWait")
	}
	if updated.Meet.RecapPosted {
		t.Fatal("recap should not post while still within the artifact wait window")
	}
}

// Past meetArtifactMaxWait with no links ever showing up, the recap must
// still post (saying nothing was found) rather than waiting forever.
func TestAdvanceMeetSessionGivesUpWaitingForArtifacts(t *testing.T) {
	fm := &fakeMeet{}
	m := newTestManager(t, fm)

	now := time.Now().Unix()
	ev := Event{
		Summary: "Sessão de teste",
		Meet: &MeetInfo{
			SpaceName:         "spaces/xyz",
			ConferenceRecords: []string{"conferenceRecords/abc"},
			SessionEnded:      true,
			SessionEndedAt:    now - int64((meetArtifactMaxWait + time.Minute).Seconds()),
		},
	}

	updated, finalized, _ := m.advanceMeetSession(context.Background(), "-100", ev, now)

	if !finalized {
		t.Fatal("expected finalization once meetArtifactMaxWait has passed")
	}
	if !updated.Meet.RecapPosted {
		t.Fatal("expected a recap to be posted even with no links found")
	}
}

// An event with Meet integration unavailable for its whole life (ev.Meet ==
// nil) must finalize immediately -- there is nothing to wait for.
func TestAdvanceMeetSessionFinalizesImmediatelyWithNoMeet(t *testing.T) {
	m := newTestManager(t, &fakeMeet{})
	ev := Event{Summary: "Sessão sem Meet"}

	updated, finalized, changed := m.advanceMeetSession(context.Background(), "-100", ev, time.Now().Unix())

	if !finalized {
		t.Fatal("an event with no Meet info should finalize on its first post-start tick")
	}
	if changed {
		t.Fatal("nothing should have been mutated")
	}
	if updated.Meet != nil {
		t.Fatal("Meet should remain nil")
	}
}

// runMonitorTick end-to-end: space creation is retried tick over tick until it
// succeeds, and once it does it is never attempted again.
func TestRunMonitorTickCreatesMeetSpaceOnceThenStops(t *testing.T) {
	fm := &fakeMeet{createErr: fmt.Errorf("temporary outage")}
	m := newTestManager(t, fm)

	if _, err := m.Manage(groupArgs(map[string]any{
		"action":         "create",
		"local_datetime": tomorrowAt(21),
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	m.runMonitorTick(context.Background())
	if fm.createSpaceCalls != 1 {
		t.Fatalf("expected 1 create attempt after first tick, got %d", fm.createSpaceCalls)
	}

	events := make(map[string]Event)
	m.storage.LoadFromDB(eventsFileName, &events)
	if events[fmt.Sprintf("%d", testGroupChatID)].Meet != nil {
		t.Fatal("Meet should still be nil after a failed create attempt")
	}

	// Second tick: creation now succeeds.
	fm.createErr = nil
	fm.spaceName, fm.joinURI = "spaces/xyz", "https://meet.google.com/abc-defg-hij"
	m.runMonitorTick(context.Background())
	if fm.createSpaceCalls != 2 {
		t.Fatalf("expected a second create attempt, got %d calls", fm.createSpaceCalls)
	}

	m.storage.LoadFromDB(eventsFileName, &events)
	ev := events[fmt.Sprintf("%d", testGroupChatID)]
	if ev.Meet == nil || ev.Meet.JoinURI != fm.joinURI {
		t.Fatalf("expected the event to record the created space, got %+v", ev.Meet)
	}

	// Third tick: must not create a second space now that one exists.
	m.runMonitorTick(context.Background())
	if fm.createSpaceCalls != 2 {
		t.Fatalf("expected no further create attempts once a space exists, got %d calls", fm.createSpaceCalls)
	}
}
