package meet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// staticToken is the whole of what this package needs from the outside.
type staticToken struct {
	value string
	err   error
}

func (s staticToken) AccessToken(context.Context) (string, error) {
	return s.value, s.err
}

// newTestClient wires a Client to a handler standing in for the Meet API.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		tokens:  staticToken{value: "test-token"},
		baseURL: srv.URL,
		http:    srv.Client(),
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("could not write the test response: %v", err)
	}
}

func TestCreateSpaceSendsOpenAccessAndAutoArtifacts(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected a POST, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("expected the bearer token to be attached, got %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body was not JSON: %v", err)
		}
		writeJSON(t, w, map[string]any{
			"name":       "spaces/abc",
			"meetingUri": "https://meet.google.com/abc-defg-hij",
		})
	})

	name, uri, err := c.CreateSpace(context.Background())
	if err != nil {
		t.Fatalf("CreateSpace failed: %v", err)
	}
	if name != "spaces/abc" || uri != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("unexpected space: name=%q uri=%q", name, uri)
	}

	// accessType OPEN is the point of this call, not a detail: without it
	// every player who clicks the link waits for the host to admit them one
	// by one, which is exactly what this was changed to avoid.
	config, ok := got["config"].(map[string]any)
	if !ok {
		t.Fatalf("no config in the request body: %v", got)
	}
	if config["accessType"] != "OPEN" {
		t.Errorf("expected accessType OPEN, got %v", config["accessType"])
	}
	artifacts, ok := config["artifactConfig"].(map[string]any)
	if !ok {
		t.Fatalf("no artifactConfig in the request body: %v", config)
	}
	transcription, _ := artifacts["transcriptionConfig"].(map[string]any)
	if transcription["autoTranscriptionGeneration"] != "ON" {
		t.Errorf("expected auto transcription ON, got %v", transcription)
	}
	recording, _ := artifacts["recordingConfig"].(map[string]any)
	if recording["autoRecordingGeneration"] != "OFF" {
		t.Errorf("expected auto recording OFF, got %v", recording)
	}
}

// The fallback exists so that losing auto-transcription does not also lose the
// join link. If the second attempt were ever dropped, a Meet-capable account
// whose plan stopped accepting artifactConfig would go from "link without a
// transcript" to "no Meet integration at all".
func TestCreateSpaceFallsBackToPlainOpenAccess(t *testing.T) {
	var bodies []map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		bodies = append(bodies, parsed)

		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"artifactConfig not supported"}`))
			return
		}
		writeJSON(t, w, map[string]any{"name": "spaces/xyz", "meetingUri": "https://meet.google.com/x"})
	})

	name, uri, err := c.CreateSpace(context.Background())
	if err != nil {
		t.Fatalf("CreateSpace should have fallen back, got: %v", err)
	}
	if name != "spaces/xyz" || uri != "https://meet.google.com/x" {
		t.Fatalf("unexpected space from the fallback: name=%q uri=%q", name, uri)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected two attempts, got %d", len(bodies))
	}

	// The retry must still ask for open access -- that is the half worth
	// keeping when artifacts are refused.
	config, _ := bodies[1]["config"].(map[string]any)
	if config["accessType"] != "OPEN" {
		t.Errorf("fallback lost accessType OPEN: %v", config)
	}
	if _, hasArtifacts := config["artifactConfig"]; hasArtifacts {
		t.Errorf("fallback should not resend artifactConfig: %v", config)
	}
}

func TestCreateSpaceReportsTheErrorWhenBothAttemptsFail(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"insufficient scope"}`))
	})

	if _, _, err := c.CreateSpace(context.Background()); err == nil {
		t.Fatal("expected an error when both attempts fail")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected the status code in the error, got %v", err)
	}
}

func TestTokenFailureIsReportedRatherThanCallingTheAPI(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		tokens:  staticToken{err: errors.New("no token file")},
		baseURL: srv.URL,
		http:    srv.Client(),
	}

	_, err := c.ListConferenceRecords(context.Background(), "spaces/abc")
	if err == nil {
		t.Fatal("expected an error when no token is available")
	}
	if !strings.Contains(err.Error(), "access token") {
		t.Errorf("expected the error to name the token, got %v", err)
	}
	if called {
		t.Error("the API should not be called without a token")
	}
}

func TestListConferenceRecordsFiltersBySpaceAndKeepsOpenRecords(t *testing.T) {
	var gotFilter string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotFilter = r.URL.Query().Get("filter")
		writeJSON(t, w, map[string]any{
			"conferenceRecords": []any{
				map[string]any{"name": "conferenceRecords/1", "startTime": "2026-04-10T21:00:00Z", "endTime": "2026-04-10T23:30:00Z"},
				map[string]any{"name": "conferenceRecords/2", "startTime": "2026-04-10T23:40:00Z"},
			},
		})
	})

	records, err := c.ListConferenceRecords(context.Background(), "spaces/abc")
	if err != nil {
		t.Fatalf("ListConferenceRecords failed: %v", err)
	}
	if gotFilter != `space.name="spaces/abc"` {
		t.Errorf("unexpected filter: %q", gotFilter)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	// An empty EndTime is the "still running" signal the whole Meet lifecycle
	// is built on, so it must survive decoding rather than being dropped.
	if records[1].EndTime != "" {
		t.Errorf("expected the second record to still be open, got EndTime %q", records[1].EndTime)
	}
	if records[0].EndTime == "" {
		t.Errorf("expected the first record to be closed, got an empty EndTime")
	}
}

func TestListConferenceRecordsFollowsPagination(t *testing.T) {
	var pageTokens []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("pageToken")
		pageTokens = append(pageTokens, token)

		// The filter must be carried onto every page, not just the first --
		// dropping it would silently start returning other spaces' records.
		if got := r.URL.Query().Get("filter"); got != `space.name="spaces/abc"` {
			t.Errorf("page %d lost the filter: %q", len(pageTokens), got)
		}

		switch token {
		case "":
			writeJSON(t, w, map[string]any{
				"conferenceRecords": []any{map[string]any{"name": "conferenceRecords/1"}},
				"nextPageToken":     "page-2",
			})
		case "page-2":
			writeJSON(t, w, map[string]any{
				"conferenceRecords": []any{map[string]any{"name": "conferenceRecords/2"}},
			})
		default:
			t.Errorf("unexpected page token %q", token)
		}
	})

	records, err := c.ListConferenceRecords(context.Background(), "spaces/abc")
	if err != nil {
		t.Fatalf("ListConferenceRecords failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected both pages to be collected, got %d records", len(records))
	}
	if len(pageTokens) != 2 || pageTokens[1] != "page-2" {
		t.Errorf("unexpected page sequence: %v", pageTokens)
	}
}

func TestListConferenceRecordsHandlesAnEmptyResult(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{})
	})

	records, err := c.ListConferenceRecords(context.Background(), "spaces/abc")
	if err != nil {
		t.Fatalf("an empty list is not an error: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records, got %d", len(records))
	}
}

// A doc that has not finished generating comes back without docsDestination.
// Skipping it (rather than erroring, or emitting an empty link) is what lets
// the event lifecycle keep polling until the real link shows up.
func TestTranscriptLinksSkipsDocsThatAreNotReady(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/transcripts") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"transcripts": []any{
				map[string]any{"name": "t/1", "state": "STARTED"},
				map[string]any{"name": "t/2", "docsDestination": map[string]any{"exportUri": "https://docs.google.com/d/1"}},
				map[string]any{"name": "t/3", "docsDestination": map[string]any{"exportUri": ""}},
			},
		})
	})

	links, err := c.TranscriptLinks(context.Background(), "conferenceRecords/1")
	if err != nil {
		t.Fatalf("TranscriptLinks failed: %v", err)
	}
	if len(links) != 1 || links[0] != "https://docs.google.com/d/1" {
		t.Fatalf("expected only the ready doc, got %v", links)
	}
}

func TestSmartNotesLinksUsesTheSmartNotesCollection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/smartNotes") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"smartNotes": []any{
				map[string]any{"docsDestination": map[string]any{"exportUri": "https://docs.google.com/d/notes"}},
			},
		})
	})

	links, err := c.SmartNotesLinks(context.Background(), "conferenceRecords/1")
	if err != nil {
		t.Fatalf("SmartNotesLinks failed: %v", err)
	}
	if len(links) != 1 || links[0] != "https://docs.google.com/d/notes" {
		t.Fatalf("unexpected links: %v", links)
	}
}

func TestListParticipantsReadsPresenceFromLatestEndTime(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"participants": []any{
				map[string]any{
					"name":          "participants/1",
					"signedinUser":  map[string]any{"displayName": "Guilherme"},
					"latestEndTime": "2026-04-10T23:00:00Z",
				},
				map[string]any{
					"name":         "participants/2",
					"signedinUser": map[string]any{"displayName": "Bruno"},
				},
			},
		})
	})

	participants, err := c.ListParticipants(context.Background(), "conferenceRecords/1")
	if err != nil {
		t.Fatalf("ListParticipants failed: %v", err)
	}
	if len(participants) != 2 {
		t.Fatalf("expected 2 participants, got %d", len(participants))
	}
	// Someone who left stays in the list with Present false; that is what lets
	// the lifecycle report a leave rather than silently forgetting them.
	if participants[0].Present {
		t.Errorf("%q has an end time and should not be present", participants[0].DisplayName)
	}
	if !participants[1].Present {
		t.Errorf("%q has no end time and should still be present", participants[1].DisplayName)
	}
}

func TestListParticipantsLabelsEveryKindOfUser(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"participants": []any{
				map[string]any{"name": "p/1", "signedinUser": map[string]any{"displayName": "Guilherme"}},
				map[string]any{"name": "p/2", "anonymousUser": map[string]any{"displayName": "Convidado"}},
				map[string]any{"name": "p/3", "phoneUser": map[string]any{"displayName": "+55 11"}},
				map[string]any{"name": "p/4"},
			},
		})
	})

	participants, err := c.ListParticipants(context.Background(), "conferenceRecords/1")
	if err != nil {
		t.Fatalf("ListParticipants failed: %v", err)
	}

	want := []string{"Guilherme", "Convidado (anônimo)", "+55 11 (telefone)", "Desconhecido"}
	if len(participants) != len(want) {
		t.Fatalf("expected %d participants, got %d", len(want), len(participants))
	}
	for i, w := range want {
		if participants[i].DisplayName != w {
			t.Errorf("participant %d: expected %q, got %q", i, w, participants[i].DisplayName)
		}
	}
}

func TestAPIErrorTruncatesAVeryLongBody(t *testing.T) {
	err := &apiError{status: 500, body: strings.Repeat("x", 1000)}
	msg := err.Error()
	if len(msg) > 400 {
		t.Errorf("expected a truncated message, got %d chars", len(msg))
	}
	if !strings.HasSuffix(msg, "...") {
		t.Errorf("expected the truncation marker, got %q", msg[len(msg)-10:])
	}
	if !strings.Contains(msg, "HTTP 500") {
		t.Errorf("expected the status in the message, got %q", msg)
	}
}

func TestNonJSONResponseIsReportedAsADecodeFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	})

	_, err := c.ListConferenceRecords(context.Background(), "spaces/abc")
	if err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected a decode error, got %v", err)
	}
}

func TestRequestsHonourContextCancellation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{})
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.ListConferenceRecords(ctx, "spaces/abc"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a cancelled context to abort the call, got %v", err)
	}
}
