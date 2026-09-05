// Command meetspike is a throwaway probe of the Google Meet API.
//
// It exists to answer three questions before any of the Meet work is designed
// around guesses:
//
//  1. Can this account create a space, and will the API accept an
//     auto-transcription config at creation time? (If yes, nobody has to
//     remember to press "Transcribe meeting" on session night.)
//  2. After a real meeting, does conferenceRecords.transcripts.entries actually
//     return text for this account?
//  3. Is Gemini's meeting summary reachable through any API, or is a Drive
//     document the only route to it?
//
// Nothing here is meant to survive into the bot. Every response is printed as
// raw JSON on purpose, because unknown fields are part of what we are looking
// for.
//
// Usage, from the repository root:
//
//	go run ./cmd/meetspike auth      # one-time consent
//	go run ./cmd/meetspike create    # make a space, print the join link
//	go run ./cmd/meetspike check     # after the meeting: records, transcripts
//	go run ./cmd/meetspike drive     # look for Gemini notes in Drive
//	go run ./cmd/meetspike           # guided run through all of the above
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/config"
)

// statePath remembers the space between runs, so 'create' and 'check' can be
// separated by an actual meeting instead of by a sleep.
var statePath = filepath.Join("data", "db", "meetspike.json")

type spikeState struct {
	SpaceName   string    `json:"space_name"`
	MeetingURI  string    `json:"meeting_uri"`
	MeetingCode string    `json:"meeting_code"`
	AcceptedAs  string    `json:"accepted_as"`
	CreatedAt   time.Time `json:"created_at"`
}

func loadState() *spikeState {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return &spikeState{}
	}
	var s spikeState
	if err := json.Unmarshal(data, &s); err != nil {
		return &spikeState{}
	}
	return &s
}

func saveState(s *spikeState) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		fmt.Printf("could not create state dir: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		fmt.Printf("could not save state: %v\n", err)
	}
}

func main() {
	ctx := context.Background()

	cfg, err := config.LoadFromFile()
	if err != nil {
		fmt.Printf("failed to load config.yaml: %v\n", err)
		os.Exit(1)
	}

	command := "guided"
	if len(os.Args) > 1 {
		command = strings.ToLower(os.Args[1])
	}

	// The Drive scope is restricted, so it is never requested unless this run
	// is actually going looking for the Gemini notes document.
	withDrive := command == "drive"
	for _, arg := range os.Args[1:] {
		if arg == "-drive" || arg == "--drive" {
			withDrive = true
		}
	}

	auth, err := newAuthenticator(cfg, requestedScopes(withDrive))
	if err != nil {
		fmt.Printf("\n%v\n", err)
		os.Exit(1)
	}

	client := &apiClient{auth: auth}
	state := loadState()

	switch command {
	case "auth":
		if _, err := auth.accessToken(ctx); err != nil {
			fail(err)
		}
		fmt.Println("\nAuthentication works.")

	case "create":
		createSpace(ctx, client, state)

	case "check":
		requireSpace(state)
		client.inspectSpace(ctx, state.SpaceName)

	case "drive":
		since := state.CreatedAt
		if since.IsZero() {
			since = time.Now().Add(-24 * time.Hour)
		}
		client.findGeminiNotes(ctx, since)

	case "guided":
		guided(ctx, client, state, withDrive)

	default:
		fmt.Printf("unknown command %q. Use one of: auth, create, check, drive\n", command)
		os.Exit(1)
	}
}

const accessDeniedHelp = `
"Access blocked / Error 403: access_denied" means the consent screen did not
recognise the account you signed in with as an approved tester. Check, in this
order:

  1. The test user is listed on the SAME project the OAuth client came from.
     Having several Cloud projects open makes this easy to get wrong: compare
     the project on the consent screen with the project_id in your downloaded
     credentials JSON.
  2. The test user list was actually saved (Google Auth Platform > Audience >
     Test users), with the exact address you sign in with.
  3. Give it about five minutes. Test user changes are not always immediate.

If it still fails, the fastest fix is to set the publishing status to
"Production". Unverified apps show a warning screen you can click through via
"Advanced", and for a single-user bot that is fine -- it also removes the
7-day refresh token expiry that Testing mode imposes, which would otherwise
break the bot roughly every week once deployed.`

func fail(err error) {
	fmt.Printf("\nFAILED: %v\n", err)
	if strings.Contains(err.Error(), "access_denied") {
		fmt.Println(accessDeniedHelp)
	}
	os.Exit(1)
}

func requireSpace(state *spikeState) {
	if state.SpaceName == "" {
		fmt.Println("No space on record. Run 'go run ./cmd/meetspike create' first.")
		os.Exit(1)
	}
}

func createSpace(ctx context.Context, client *apiClient, state *spikeState) {
	space, acceptedAs, err := client.createSpace(ctx)
	if err != nil {
		fail(err)
	}

	state.SpaceName = str(space, "name")
	state.MeetingURI = str(space, "meetingUri")
	state.MeetingCode = str(space, "meetingCode")
	state.AcceptedAs = acceptedAs
	state.CreatedAt = time.Now()
	saveState(state)

	fmt.Printf("\n==============================================\n")
	fmt.Printf("Space:   %s\n", state.SpaceName)
	fmt.Printf("Join at: %s\n", state.MeetingURI)
	fmt.Printf("Created with: %s\n", acceptedAs)
	fmt.Printf("==============================================\n")

	if !strings.Contains(acceptedAs, "auto-transcription") {
		fmt.Println("\nNOTE: the API would not take an auto-transcription config, so somebody")
		fmt.Println("has to switch on 'Transcribe meeting' by hand once inside the call.")
		fmt.Println("Do that, or the transcript check later will come back empty.")
	}
}

// guided walks the whole experiment in one sitting, since the interesting part
// requires a human to actually hold a meeting in the middle of it.
func guided(ctx context.Context, client *apiClient, state *spikeState, withDrive bool) {
	fmt.Println("Google Meet API spike")
	fmt.Println("=====================")
	fmt.Println("This will create a real meeting and ask you to join it briefly.")

	if _, err := auth(ctx, client); err != nil {
		fail(err)
	}

	createSpace(ctx, client, state)

	fmt.Println("\nNow, in the browser:")
	fmt.Println("  1. Join the meeting at the link above.")
	fmt.Println("  2. Switch on 'Transcribe meeting' (and 'Take notes for me', to test both).")
	fmt.Println("  3. Talk for a minute -- say something story-like and something off-topic,")
	fmt.Println("     so we can see whether a narrative-only summary is even separable.")
	fmt.Println("  4. Leave the meeting, so the conference record gets an end time.")
	fmt.Print("\nPress Enter here once you have left the meeting... ")
	bufio.NewReader(os.Stdin).ReadString('\n')

	// Artifacts are produced after the fact; asking immediately tends to show
	// an ended record with no transcript attached yet.
	fmt.Println("\nGiving Google a moment to finish producing artifacts...")
	time.Sleep(30 * time.Second)

	client.inspectSpace(ctx, state.SpaceName)

	if withDrive {
		client.findGeminiNotes(ctx, state.CreatedAt.Add(-time.Hour))
	} else {
		fmt.Println("\nSkipping the Drive search for Gemini notes: it needs a restricted")
		fmt.Println("scope that would complicate deploying this headless. To probe it anyway:")
		fmt.Println("    go run ./cmd/meetspike drive")
	}

	fmt.Println("\n=====================================================")
	fmt.Println("If transcripts came back empty, wait a few minutes and run:")
	fmt.Println("    go run ./cmd/meetspike check")
	fmt.Println("Artifacts can take a while to appear after a call ends.")
	fmt.Println("=====================================================")
}

func auth(ctx context.Context, client *apiClient) (string, error) {
	return client.auth.accessToken(ctx)
}
