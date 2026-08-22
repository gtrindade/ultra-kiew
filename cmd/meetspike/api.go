package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiClient is a deliberately dumb REST caller.
//
// The spike talks to Meet over plain HTTP rather than through the generated
// Google client library for two reasons: it adds no dependency to a throwaway
// binary, and it lets every response be printed as raw JSON. Raw JSON is the
// point -- one of the questions this spike exists to answer is whether some
// field we do not know about (a summary, a notes document) is present on these
// resources, and a typed client would silently drop exactly that.
type apiClient struct {
	auth *authenticator
}

type apiResult struct {
	status int
	raw    map[string]any
	body   string
}

func (r apiResult) ok() bool { return r.status >= 200 && r.status < 300 }

// errorMessage digs the human-readable part out of a Google API error body.
func (r apiResult) errorMessage() string {
	if r.raw != nil {
		if e, ok := r.raw["error"].(map[string]any); ok {
			msg, _ := e["message"].(string)
			status, _ := e["status"].(string)
			if msg != "" {
				return fmt.Sprintf("%s (%s)", msg, status)
			}
		}
	}
	body := strings.TrimSpace(r.body)
	if len(body) > 400 {
		body = body[:400] + "..."
	}
	return fmt.Sprintf("HTTP %d: %s", r.status, body)
}

func (c *apiClient) do(ctx context.Context, method, endpoint string, payload any) (apiResult, error) {
	accessToken, err := c.auth.accessToken(ctx)
	if err != nil {
		return apiResult{}, err
	}

	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return apiResult{}, err
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return apiResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return apiResult{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return apiResult{}, err
	}

	result := apiResult{status: resp.StatusCode, body: string(raw)}
	// A non-JSON body is not an error worth aborting on: the status code and
	// the text are still evidence, and printing them is the whole job here.
	_ = json.Unmarshal(raw, &result.raw)
	return result, nil
}

func (c *apiClient) get(ctx context.Context, endpoint string) (apiResult, error) {
	return c.do(ctx, http.MethodGet, endpoint, nil)
}

// dump pretty-prints a response so unknown fields are visible.
func dump(label string, result apiResult) {
	fmt.Printf("\n--- %s (HTTP %d) ---\n", label, result.status)
	if result.raw != nil {
		pretty, err := json.MarshalIndent(result.raw, "", "  ")
		if err == nil {
			fmt.Println(string(pretty))
			return
		}
	}
	fmt.Println(strings.TrimSpace(result.body))
}

// listOf pulls a named array out of a response, tolerating its absence.
func listOf(result apiResult, key string) []map[string]any {
	if result.raw == nil {
		return nil
	}
	items, ok := result.raw[key].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// spaceAttempt records one attempt at creating a space with a given request
// body, so the spike can report exactly which shape the API accepted.
type spaceAttempt struct {
	label   string
	baseURL string
	payload map[string]any
}

// createAttempts is the cascade, most-desirable first.
//
// Auto-transcription is the prize: if the API accepts it at creation time, no
// human has to remember to press "Transcribe meeting", and the whole feature
// stops depending on the most unreliable component in the system. If it is
// rejected, we fall back to a plain space and the transcript becomes opt-in per
// meeting -- worth knowing now rather than discovering on session night.
func createAttempts() []spaceAttempt {
	autoConfig := map[string]any{
		"config": map[string]any{
			"artifactConfig": map[string]any{
				"transcriptionConfig": map[string]any{
					"autoTranscriptionGeneration": "ON",
				},
				"recordingConfig": map[string]any{
					"autoRecordingGeneration": "OFF",
				},
			},
		},
	}

	return []spaceAttempt{
		{label: "v2beta + auto-transcription", baseURL: "https://meet.googleapis.com/v2beta", payload: autoConfig},
		{label: "v2 + auto-transcription", baseURL: "https://meet.googleapis.com/v2", payload: autoConfig},
		{label: "v2 plain space (no artifact config)", baseURL: "https://meet.googleapis.com/v2", payload: map[string]any{}},
	}
}

func (c *apiClient) createSpace(ctx context.Context) (map[string]any, string, error) {
	var lastErr string
	for _, attempt := range createAttempts() {
		fmt.Printf("\nTrying to create a space: %s\n", attempt.label)
		result, err := c.do(ctx, http.MethodPost, attempt.baseURL+"/spaces", attempt.payload)
		if err != nil {
			return nil, "", err
		}
		dump("spaces.create ["+attempt.label+"]", result)
		if result.ok() {
			fmt.Printf("\n>>> ACCEPTED: %s\n", attempt.label)
			return result.raw, attempt.label, nil
		}
		lastErr = result.errorMessage()
		fmt.Printf(">>> rejected: %s\n", lastErr)
	}
	return nil, "", fmt.Errorf("every space creation attempt failed, last error: %s", lastErr)
}

// inspectSpace walks everything reachable from a space after a meeting.
func (c *apiClient) inspectSpace(ctx context.Context, spaceName string) {
	const base = "https://meet.googleapis.com/v2"

	fmt.Printf("\n=== Inspecting %s ===\n", spaceName)

	current, err := c.get(ctx, base+"/"+spaceName)
	if err != nil {
		fmt.Printf("failed to read the space: %v\n", err)
	} else {
		dump("spaces.get (does the config still show transcription settings?)", current)
	}

	filter := url.QueryEscape(fmt.Sprintf("space.name=\"%s\"", spaceName))
	records, err := c.get(ctx, base+"/conferenceRecords?filter="+filter)
	if err != nil {
		fmt.Printf("failed to list conference records: %v\n", err)
		return
	}
	dump("conferenceRecords.list", records)

	items := listOf(records, "conferenceRecords")
	if len(items) == 0 {
		fmt.Println("\n>>> No conference records yet.")
		fmt.Println(">>> A record only appears once someone has actually joined the meeting,")
		fmt.Println(">>> and endTime is only set once everyone has left. If you just hung up,")
		fmt.Println(">>> give it a minute and run 'check' again.")
		return
	}

	for _, record := range items {
		name := str(record, "name")
		endTime := str(record, "endTime")
		state := "STILL RUNNING (nobody has left yet, or the record is open)"
		if endTime != "" {
			state = "ENDED at " + endTime
		}
		fmt.Printf("\n=== Conference record %s: %s ===\n", name, state)

		c.inspectRecordChild(ctx, base, name, "participants")
		c.inspectRecordChild(ctx, base, name, "recordings")
		c.inspectTranscripts(ctx, base, name)
	}
}

func (c *apiClient) inspectRecordChild(ctx context.Context, base, recordName, child string) {
	result, err := c.get(ctx, fmt.Sprintf("%s/%s/%s", base, recordName, child))
	if err != nil {
		fmt.Printf("failed to list %s: %v\n", child, err)
		return
	}
	dump(child+".list", result)
}

func (c *apiClient) inspectTranscripts(ctx context.Context, base, recordName string) {
	result, err := c.get(ctx, fmt.Sprintf("%s/%s/transcripts", base, recordName))
	if err != nil {
		fmt.Printf("failed to list transcripts: %v\n", err)
		return
	}
	dump("transcripts.list", result)

	transcripts := listOf(result, "transcripts")
	if len(transcripts) == 0 {
		fmt.Println("\n>>> NO TRANSCRIPT on this conference record.")
		fmt.Println(">>> Either transcription was never switched on for the meeting, or the")
		fmt.Println(">>> account has no transcription entitlement. Note that this is the exact")
		fmt.Println(">>> silent-failure case: an empty list, not an error.")
		return
	}

	for _, transcript := range transcripts {
		name := str(transcript, "name")
		fmt.Printf("\n>>> TRANSCRIPT FOUND: %s (state %s)\n", name, str(transcript, "state"))

		// docsDestination points at a Drive doc. Worth surfacing, but entries
		// below are the better source: structured, per-speaker, and reachable
		// without asking the user for any Drive scope at all.
		if dest, ok := transcript["docsDestination"].(map[string]any); ok {
			fmt.Printf(">>> Drive doc: %s (%s)\n", str(dest, "document"), str(dest, "exportUri"))
		}

		entries, err := c.get(ctx, fmt.Sprintf("%s/%s/entries?pageSize=200", base, name))
		if err != nil {
			fmt.Printf("failed to list entries: %v\n", err)
			continue
		}
		dump("transcripts.entries.list", entries)

		items := listOf(entries, "truncatedEntries")
		if len(items) == 0 {
			items = listOf(entries, "transcriptEntries")
		}
		fmt.Printf("\n>>> %d transcript entries. Reconstructed text:\n", len(items))
		for _, entry := range items {
			fmt.Printf("    [%s] %s\n", str(entry, "participant"), str(entry, "text"))
		}
	}
}

// findGeminiNotes answers question 3 the only way that seems to be available:
// by looking in Drive for the document Gemini leaves behind.
//
// If this is the only route, using it in production means asking every host for
// a Drive scope, which is a much bigger ask than reading transcript entries --
// and it still yields a generic meeting recap rather than the narrative-only
// summary we actually want. Worth measuring before deciding.
func (c *apiClient) findGeminiNotes(ctx context.Context, since time.Time) {
	fmt.Println("\n=== Looking in Drive for Gemini notes / transcript docs ===")

	query := fmt.Sprintf("(name contains 'Notes by Gemini' or name contains 'Gemini' or name contains 'Transcript' or name contains 'Anota') and modifiedTime > '%s'",
		since.UTC().Format(time.RFC3339))
	endpoint := "https://www.googleapis.com/drive/v3/files?" + url.Values{
		"q":        {query},
		"fields":   {"files(id,name,mimeType,createdTime,modifiedTime,webViewLink,owners(emailAddress))"},
		"pageSize": {"25"},
		"orderBy":  {"modifiedTime desc"},
	}.Encode()

	result, err := c.get(ctx, endpoint)
	if err != nil {
		fmt.Printf("drive search failed: %v\n", err)
		return
	}
	dump("drive.files.list", result)

	files := listOf(result, "files")
	if len(files) == 0 {
		fmt.Println("\n>>> Nothing matching in Drive. Either Gemini notes were not generated,")
		fmt.Println(">>> or they take a while to land, or they are named something else.")
		return
	}
	fmt.Printf("\n>>> %d candidate documents:\n", len(files))
	for _, file := range files {
		fmt.Printf("    %s  [%s]  %s\n", str(file, "name"), str(file, "modifiedTime"), str(file, "webViewLink"))
	}
}
