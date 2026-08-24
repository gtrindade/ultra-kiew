package event

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/gtrindade/ultra-kiew/internal/meet"
)

// Meet session timing. These are constants rather than config precisely
// because getting them wrong is cheap: every one of them only ever delays a
// recap by a bounded amount, never loses data, so there is no need to make
// them tunable before there is a reason to.
const (
	// meetGracePeriod is how long to wait, after every known conference
	// record has an end time, before believing the session is actually over.
	// Real sessions break for pizza: someone leaving closes a conference
	// record and rejoining opens a new one on the same space, and without this
	// window the first bathroom break would end the session.
	meetGracePeriod = 20 * time.Minute

	// meetNoShowGrace is how long to wait for a first conference record to
	// appear at all before concluding nobody joined this Meet space.
	meetNoShowGrace = 30 * time.Minute

	// meetMaxSessionLength is a hard cap so a conference record that somehow
	// never gets an end time (a crash on Google's side, not ours) cannot pin
	// an event in the "live" state forever.
	meetMaxSessionLength = 12 * time.Hour

	// meetArtifactMaxWait is how long, after the session is deemed over, to
	// keep retrying for transcript/notes links before giving up and posting
	// the recap saying nothing was found. Google can take a long while to
	// finish generating a transcript for a long session -- these run up to
	// several hours -- so this is deliberately generous rather than assuming
	// "a few minutes" covers every case.
	meetArtifactMaxWait = 24 * time.Hour

	// meetArtifactPollInterval paces how often the Meet API is actually
	// checked during that wait. Checking on every monitor tick (once a
	// minute) for up to 24 hours would be thousands of calls per session for
	// no benefit -- a transcript does not appear gradually, it either exists
	// on a given check or it does not.
	meetArtifactPollInterval = 10 * time.Minute
)

// MeetSegment is one conference record's worth of a session: when it ran, and
// whatever transcript/notes links were found for it.
//
// A session is not always one continuous record. Besides an actual reconnect,
// Google Meet's automatic language-detection nudge restarts artifact
// generation when accepted mid-meeting -- so a two-second "wrong language"
// opening and the real 4-hour session in the right language show up as two
// separate records, with no way to tell them apart from a flat list of links.
// Keeping links grouped by the record (and its duration) they came from is
// what lets the recap actually say which is which.
type MeetSegment struct {
	RecordName string `json:"record_name"`
	StartTime  string `json:"start_time,omitempty"`
	EndTime    string `json:"end_time,omitempty"`

	NotesLinks      []string `json:"notes_links,omitempty"`
	TranscriptLinks []string `json:"transcript_links,omitempty"`
}

// MeetInfo holds everything about the Google Meet space tied to one event.
// This is the state machine: every field is either something learned once
// (SpaceName, JoinURI), a flag/slice that only ever grows or gets filled in
// (Segments, and the fields within each one), or latches once true
// (SessionEnded, RecapPosted) -- so replaying the same tick twice against the
// same persisted state is always safe.
type MeetInfo struct {
	SpaceName string `json:"space_name,omitempty"`
	JoinURI   string `json:"join_uri,omitempty"`

	// Segments accumulates one entry per conference record seen for this
	// space, in the order first seen.
	Segments []MeetSegment `json:"segments,omitempty"`

	SessionEnded bool `json:"session_ended,omitempty"`
	// SessionEndedAt is when SessionEnded first latched true (unix seconds),
	// which anchors the meetArtifactMaxWait clock independently of whichever
	// tick happens to be running.
	SessionEndedAt int64 `json:"session_ended_at,omitempty"`

	RecapPosted bool `json:"recap_posted,omitempty"`

	// LastArtifactPollAt is when transcript/notes links were last actually
	// checked (unix seconds), so that check can be paced to
	// meetArtifactPollInterval instead of running on every monitor tick for
	// the entire meetArtifactMaxWait window.
	LastArtifactPollAt int64 `json:"last_artifact_poll_at,omitempty"`
}

// segmentIndex finds a segment by record name, or -1.
func (info *MeetInfo) segmentIndex(recordName string) int {
	for i := range info.Segments {
		if info.Segments[i].RecordName == recordName {
			return i
		}
	}
	return -1
}

// MeetClient is everything the event lifecycle needs from Google Meet.
// *meet.Client satisfies it; tests can supply a fake.
type MeetClient interface {
	CreateSpace(ctx context.Context) (spaceName, joinURI string, err error)
	ListConferenceRecords(ctx context.Context, spaceName string) ([]meet.ConferenceRecord, error)
	TranscriptLinks(ctx context.Context, conferenceRecordName string) ([]string, error)
	SmartNotesLinks(ctx context.Context, conferenceRecordName string) ([]string, error)
}

// advanceMeetSession drives one event through everything that happens after
// its start time: watching for the session to end, fetching transcript/notes
// links once it has, and posting the recap. It returns the updated event, and
// whether that event is now finished (finalized) and should be archived.
//
// This is called once per event per tick -- from runMonitorTick, only once
// ev.Timestamp has passed -- with no separate goroutine or timer per event.
// The lifecycle lives entirely in the persisted fields on ev.Meet, so it is
// exactly as safe to call twice on the same state as once.
func (m *Manager) advanceMeetSession(ctx context.Context, chatIDStr string, ev Event, now int64) (updated Event, finalized bool, changed bool) {
	if ev.Meet == nil {
		// Meet was never available for this event (disabled, or space
		// creation never succeeded before start time): nothing to wait for.
		return ev, true, false
	}

	meetInfo := *ev.Meet
	hardCapped := false

	if !meetInfo.SessionEnded {
		sessionChanged := m.pollConferenceRecords(ctx, chatIDStr, &meetInfo, ev.Timestamp, now)
		changed = changed || sessionChanged
	}

	if !meetInfo.SessionEnded && now-ev.Timestamp >= int64(meetMaxSessionLength.Seconds()) {
		log.Printf("Alert: Meet session for chat %s hit the %s hard cap, finalizing without a confirmed end", chatIDStr, meetMaxSessionLength)
		meetInfo.SessionEnded = true
		meetInfo.SessionEndedAt = now
		hardCapped = true
		changed = true
	}

	if !meetInfo.SessionEnded {
		ev.Meet = &meetInfo
		return ev, false, changed
	}

	if !meetInfo.RecapPosted {
		pollDue := meetInfo.LastArtifactPollAt == 0 || now-meetInfo.LastArtifactPollAt >= int64(meetArtifactPollInterval.Seconds())
		if pollDue && len(meetInfo.Segments) > 0 {
			if m.pollMeetLinks(ctx, chatIDStr, &meetInfo) {
				changed = true
			}
			meetInfo.LastArtifactPollAt = now
			changed = true
		}

		haveLinks := false
		for _, seg := range meetInfo.Segments {
			if len(seg.NotesLinks) > 0 || len(seg.TranscriptLinks) > 0 {
				haveLinks = true
				break
			}
		}
		waitedLongEnough := now-meetInfo.SessionEndedAt >= int64(meetArtifactMaxWait.Seconds())
		nothingToWaitFor := len(meetInfo.Segments) == 0

		// hardCapped skips the artifact wait entirely: reaching the hard cap
		// already means something took far longer than any real session
		// should, so waiting another meetArtifactMaxWait on top of that for
		// artifacts that are equally unlikely to ever arrive would just be
		// compounding the same failure.
		if haveLinks || waitedLongEnough || nothingToWaitFor || hardCapped {
			m.postMeetRecap(chatIDStr, ev, meetInfo)
			meetInfo.RecapPosted = true
			changed = true
		}
	}

	ev.Meet = &meetInfo
	return ev, meetInfo.RecapPosted, changed
}

// pollConferenceRecords lists conference records for the space, records any
// new ones (and refreshes end times on ones already known -- a record open on
// one poll can have ended by the next), and decides whether the session
// counts as over yet.
func (m *Manager) pollConferenceRecords(ctx context.Context, chatIDStr string, meetInfo *MeetInfo, eventTimestamp, now int64) (changed bool) {
	records, err := m.meet.ListConferenceRecords(ctx, meetInfo.SpaceName)
	if err != nil {
		log.Printf("could not list conference records for chat %s (%s): %v", chatIDStr, meetInfo.SpaceName, err)
		return false
	}

	for _, r := range records {
		if i := meetInfo.segmentIndex(r.Name); i == -1 {
			meetInfo.Segments = append(meetInfo.Segments, MeetSegment{RecordName: r.Name, StartTime: r.StartTime, EndTime: r.EndTime})
			changed = true
		} else if meetInfo.Segments[i].EndTime != r.EndTime {
			meetInfo.Segments[i].EndTime = r.EndTime
			changed = true
		}
	}

	if len(records) == 0 {
		if now-eventTimestamp >= int64(meetNoShowGrace.Seconds()) {
			log.Printf("no one joined the Meet space for chat %s within %s, treating the session as over", chatIDStr, meetNoShowGrace)
			meetInfo.SessionEnded = true
			meetInfo.SessionEndedAt = now
			changed = true
		}
		return changed
	}

	var latestEnd int64
	allEnded := true
	for _, r := range records {
		if r.EndTime == "" {
			allEnded = false
			continue
		}
		if t, err := time.Parse(time.RFC3339, r.EndTime); err == nil && t.Unix() > latestEnd {
			latestEnd = t.Unix()
		}
	}

	if allEnded && now-latestEnd >= int64(meetGracePeriod.Seconds()) {
		meetInfo.SessionEnded = true
		meetInfo.SessionEndedAt = now
		changed = true
	}

	return changed
}

// pollMeetLinks fetches transcript and smart-notes links for every known
// conference record, filing each under its own segment. It is safe to call
// repeatedly: links already present are left as-is (dedup by content), and a
// record whose docs have not landed yet just contributes nothing this time,
// to be retried on the next tick.
func (m *Manager) pollMeetLinks(ctx context.Context, chatIDStr string, meetInfo *MeetInfo) (changed bool) {
	for i := range meetInfo.Segments {
		recordName := meetInfo.Segments[i].RecordName

		if links, err := m.meet.TranscriptLinks(ctx, recordName); err != nil {
			log.Printf("could not fetch transcript links for chat %s record %s: %v", chatIDStr, recordName, err)
		} else {
			for _, link := range links {
				if !contains(meetInfo.Segments[i].TranscriptLinks, link) {
					meetInfo.Segments[i].TranscriptLinks = append(meetInfo.Segments[i].TranscriptLinks, link)
					changed = true
				}
			}
		}

		if links, err := m.meet.SmartNotesLinks(ctx, recordName); err != nil {
			log.Printf("could not fetch smart notes links for chat %s record %s: %v", chatIDStr, recordName, err)
		} else {
			for _, link := range links {
				if !contains(meetInfo.Segments[i].NotesLinks, link) {
					meetInfo.Segments[i].NotesLinks = append(meetInfo.Segments[i].NotesLinks, link)
					changed = true
				}
			}
		}
	}
	return changed
}

// dedupeMeetLinks merges a segment's notes and transcript link lists into
// one, dropping repeats by URL.
//
// When a meeting has both transcription and "Take notes for me" turned on,
// Google merges them into a single Drive doc rather than producing two -- so
// the exact same docsDestination.document shows up under both transcripts and
// smartNotes for that record. Labeling that one link twice ("Notas: X" /
// "Transcrição: X") reads as two artifacts when it is actually one.
func dedupeMeetLinks(notesLinks, transcriptLinks []string) []string {
	seen := make(map[string]bool)
	var links []string
	for _, link := range notesLinks {
		if !seen[link] {
			seen[link] = true
			links = append(links, link)
		}
	}
	for _, link := range transcriptLinks {
		if !seen[link] {
			seen[link] = true
			links = append(links, link)
		}
	}
	return links
}

// formatSegmentDuration renders how long a segment ran, in whatever
// resolution actually matters (seconds for a blink-and-you-missed-it
// fragment, hours+minutes for the real session) -- this is the label that
// lets a reader tell a Meet language-switch artifact (a few seconds, in the
// wrong language) apart from the actual multi-hour session at a glance,
// without opening either link. Returns "" if the times cannot be parsed
// (should not happen for a segment that reached the recap, but degrading to
// no label is better than a wrong one).
func formatSegmentDuration(startTime, endTime string) string {
	start, err := time.Parse(time.RFC3339, startTime)
	if err != nil {
		return ""
	}
	end, err := time.Parse(time.RFC3339, endTime)
	if err != nil {
		return ""
	}
	d := end.Sub(start)
	if d < 0 {
		return ""
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dmin", h, m)
	}
}

// postMeetRecap posts whatever transcript/notes links were found, one line
// per segment labeled with how long that segment ran, as a reply to the
// original event card. It is plain text, not model-generated: these are
// Drive links, and nothing about summarizing or phrasing this belongs to the
// model -- see the narrative-summary experiment this replaced, which turned
// out to invent details no matter which Gemini model wrote it.
func (m *Manager) postMeetRecap(chatIDStr string, ev Event, meetInfo MeetInfo) {
	if m.bot == nil {
		return
	}
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return
	}

	var lines []string
	totalLinks := 0
	for _, seg := range meetInfo.Segments {
		links := dedupeMeetLinks(seg.NotesLinks, seg.TranscriptLinks)
		if len(links) == 0 {
			continue
		}
		totalLinks += len(links)

		label := "🕐 Trecho:"
		if duration := formatSegmentDuration(seg.StartTime, seg.EndTime); duration != "" {
			label = fmt.Sprintf("🕐 Trecho de %s:", duration)
		}

		lines = append(lines, label)
		for _, link := range links {
			lines = append(lines, "📄 "+link)
		}
	}

	var text string
	if totalLinks == 0 {
		text = fmt.Sprintf("A sessão %q terminou, mas não encontrei transcrição ou anotações geradas para ela.", ev.Summary)
	} else {
		text = fmt.Sprintf("A sessão %q terminou! Aqui está o material da call:\n\n", ev.Summary)
		text += strings.Join(lines, "\n")
		text += "\n\n(Esses links só abrem para quem já tem acesso no Drive -- compartilhe manualmente se precisar.)"
	}

	params := &bot.SendMessageParams{ChatID: chatID, Text: text}
	if totalLinks > 0 {
		// Same reasoning as the Meet join link in reminders: Telegram's
		// preview for a Drive doc link is generic and adds nothing here.
		params.LinkPreviewOptions = &models.LinkPreviewOptions{IsDisabled: bot.True()}
	}
	if ev.MessageID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: ev.MessageID}
	}
	m.bot.SendMessage(context.Background(), params)

	log.Printf("Alert: Meet recap posted for event '%s' in chat %s (%d segment(s), %d link(s))",
		ev.Summary, chatIDStr, len(meetInfo.Segments), totalLinks)
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
