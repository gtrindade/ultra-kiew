// Package meet is a thin wrapper over the Google Meet REST API v2: create a
// space, list conference records, and pull whatever transcript / smart-notes
// links exist for one. See cmd/meetspike for the exploration that established
// this shape and confirmed it works with no Drive scope.
package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// defaultBaseURL is the real API root. It is held on the Client rather than
// referenced as a package constant so a test can point one client at an
// httptest server without mutating global state that other tests share.
const defaultBaseURL = "https://meet.googleapis.com/v2"

// TokenSource is the one thing this package needs from the outside: a bearer
// token. internal/google.Authenticator satisfies it.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

type Client struct {
	tokens  TokenSource
	baseURL string

	// http is the transport used for every call. Left nil it falls back to
	// http.DefaultClient, so the zero Client still works.
	http *http.Client
}

func NewClient(tokens TokenSource) *Client {
	return &Client{tokens: tokens, baseURL: defaultBaseURL}
}

func (c *Client) root() string {
	if c.baseURL == "" {
		return defaultBaseURL
	}
	return c.baseURL
}

func (c *Client) doer() *http.Client {
	if c.http == nil {
		return http.DefaultClient
	}
	return c.http
}

// ConferenceRecord is the subset of the API resource the event lifecycle
// actually needs: enough to tell whether a session is still running.
type ConferenceRecord struct {
	Name      string
	StartTime string
	EndTime   string // empty means the record is still open
}

type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	body := strings.TrimSpace(e.body)
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("meet api: HTTP %d: %s", e.status, body)
}

func (c *Client) do(ctx context.Context, method, endpoint string, payload any) (map[string]any, error) {
	accessToken, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("meet api: could not get an access token: %w", err)
	}

	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.doer().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{status: resp.StatusCode, body: string(raw)}
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("meet api: could not decode response: %w", err)
	}
	return parsed, nil
}

func (c *Client) getAllPages(ctx context.Context, baseEndpoint, itemsKey string) ([]map[string]any, error) {
	var all []map[string]any
	endpoint := baseEndpoint
	for {
		result, err := c.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return all, err
		}
		if items, ok := result[itemsKey].([]any); ok {
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					all = append(all, m)
				}
			}
		}
		token, _ := result["nextPageToken"].(string)
		if token == "" {
			return all, nil
		}
		sep := "&"
		if !strings.Contains(baseEndpoint, "?") {
			sep = "?"
		}
		endpoint = baseEndpoint + sep + "pageToken=" + url.QueryEscape(token)
	}
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// CreateSpace makes a new Meet space configured to auto-transcribe and
// auto-generate "Notes by Gemini". Both were confirmed accepted by this
// account's API during exploration; if the account or plan ever stops
// supporting one of them, the space is still created, just without that
// artifact -- ListConferenceRecords/*Links simply come back empty later
// rather than erroring.
func (c *Client) CreateSpace(ctx context.Context) (spaceName, joinURI string, err error) {
	payload := map[string]any{
		"config": map[string]any{
			// OPEN means anyone with the join link gets in directly -- no
			// knocking, no host admitting them one by one. The players
			// joining a session are not in the host's Workspace org (there
			// isn't one, this is a consumer account), so the other access
			// types would otherwise gate every single join on someone
			// manually letting them in.
			"accessType": "OPEN",
			"artifactConfig": map[string]any{
				"transcriptionConfig": map[string]any{
					"autoTranscriptionGeneration": "ON",
				},
				"smartNotesConfig": map[string]any{
					"autoSmartNotesGeneration": "ON",
				},
				"recordingConfig": map[string]any{
					"autoRecordingGeneration": "OFF",
				},
			},
		},
	}

	result, err := c.do(ctx, http.MethodPost, c.root()+"/spaces", payload)
	if err != nil {
		// Fall back to open access with no auto-transcription rather than
		// failing outright: a session with a join link that at least doesn't
		// need someone to admit every player is still strictly better than
		// no Meet integration at all, and manually pressing "Transcribe
		// meeting" still produces a transcript later.
		result, err = c.do(ctx, http.MethodPost, c.root()+"/spaces", map[string]any{
			"config": map[string]any{"accessType": "OPEN"},
		})
		if err != nil {
			return "", "", err
		}
	}

	return str(result, "name"), str(result, "meetingUri"), nil
}

// ListConferenceRecords returns every conference record for a space, oldest
// first is not guaranteed -- callers should not assume order.
func (c *Client) ListConferenceRecords(ctx context.Context, spaceName string) ([]ConferenceRecord, error) {
	filter := url.QueryEscape(fmt.Sprintf("space.name=\"%s\"", spaceName))
	items, err := c.getAllPages(ctx, c.root()+"/conferenceRecords?filter="+filter, "conferenceRecords")
	if err != nil {
		return nil, err
	}

	records := make([]ConferenceRecord, 0, len(items))
	for _, item := range items {
		records = append(records, ConferenceRecord{
			Name:      str(item, "name"),
			StartTime: str(item, "startTime"),
			EndTime:   str(item, "endTime"),
		})
	}
	return records, nil
}

// docsLinks pulls every populated docsDestination.exportUri out of a list
// response shaped like transcripts.list or smartNotes.list. An entry whose doc
// has not finished generating yet has no docsDestination at all, so it is
// silently skipped rather than erroring -- the caller is expected to retry on
// a later tick until the doc appears.
func (c *Client) docsLinks(ctx context.Context, endpoint, itemsKey string) ([]string, error) {
	items, err := c.getAllPages(ctx, endpoint, itemsKey)
	if err != nil {
		return nil, err
	}

	var links []string
	for _, item := range items {
		dest, ok := item["docsDestination"].(map[string]any)
		if !ok {
			continue
		}
		if link := str(dest, "exportUri"); link != "" {
			links = append(links, link)
		}
	}
	return links, nil
}

// TranscriptLinks returns a viewable Drive link for every transcript doc
// generated so far on this conference record. It can be empty if
// transcription was never enabled for the meeting, or if the doc has not
// landed yet.
func (c *Client) TranscriptLinks(ctx context.Context, conferenceRecordName string) ([]string, error) {
	return c.docsLinks(ctx, fmt.Sprintf("%s/%s/transcripts", c.root(), conferenceRecordName), "transcripts")
}

// SmartNotesLinks returns a viewable Drive link for every "Notes by Gemini"
// doc generated so far on this conference record.
func (c *Client) SmartNotesLinks(ctx context.Context, conferenceRecordName string) ([]string, error) {
	return c.docsLinks(ctx, fmt.Sprintf("%s/%s/smartNotes", c.root(), conferenceRecordName), "smartNotes")
}

// Participant is one attendee of a conference record, as of the moment this
// was fetched.
type Participant struct {
	// Name is the participant's resource name (unique per conference record),
	// used as a stable key -- not for display.
	Name string
	// DisplayName is whatever Meet reports: the person's Google account name,
	// not their Telegram handle. There is no reliable way to map one to the
	// other, so callers can only report this name, not @-mention anyone.
	DisplayName string
	// Present is true if this participant's most recent session has not
	// ended yet -- the same "no end time means still going" signal used for
	// conference records themselves, just one level down.
	Present bool
}

// ListParticipants returns every participant Meet has ever recorded for this
// conference record, each with whether their latest session is still open.
// Someone who left and never came back keeps appearing here with Present
// false, not removed from the list.
func (c *Client) ListParticipants(ctx context.Context, conferenceRecordName string) ([]Participant, error) {
	items, err := c.getAllPages(ctx, fmt.Sprintf("%s/%s/participants?pageSize=100", c.root(), conferenceRecordName), "participants")
	if err != nil {
		return nil, err
	}

	participants := make([]Participant, 0, len(items))
	for _, item := range items {
		display := "Desconhecido"
		if u, ok := item["signedinUser"].(map[string]any); ok && str(u, "displayName") != "" {
			display = str(u, "displayName")
		} else if u, ok := item["anonymousUser"].(map[string]any); ok && str(u, "displayName") != "" {
			display = str(u, "displayName") + " (anônimo)"
		} else if u, ok := item["phoneUser"].(map[string]any); ok && str(u, "displayName") != "" {
			display = str(u, "displayName") + " (telefone)"
		}
		participants = append(participants, Participant{
			Name:        str(item, "name"),
			DisplayName: display,
			Present:     str(item, "latestEndTime") == "",
		})
	}
	return participants, nil
}
