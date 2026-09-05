package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/config"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
)

// baseScopes are the scopes every run asks for.
//
// meetings.space.created is the one production actually needs: create-and-read
// on spaces this OAuth client itself created, and nothing else. The readonly
// meetings scope is here so that an empty transcript list can never be blamed
// on a missing scope while we are still establishing what this account can do.
var baseScopes = []string{
	"https://www.googleapis.com/auth/meetings.space.created",
	"https://www.googleapis.com/auth/meetings.space.readonly",
}

// driveScope is opt-in, and deliberately so.
//
// It is the only way found so far to reach Gemini's "Notes by Gemini" document,
// but it is a *restricted* scope: Google blocks restricted scopes for
// unverified apps in Production. Since publishing to Production is exactly how
// this bot escapes the 7-day refresh-token expiry that Testing mode imposes,
// carrying this scope by default would trade a working headless deployment for
// a summary source we do not even want -- the plan builds the recap from
// transcript entries instead. Ask for it only when explicitly probing for the
// notes document.
const driveScope = "https://www.googleapis.com/auth/drive.readonly"

func requestedScopes(withDrive bool) []string {
	scopes := append([]string{}, baseScopes...)
	if withDrive {
		scopes = append(scopes, driveScope)
	}
	return scopes
}

type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope"`
}

func (t *token) valid() bool {
	// A minute of slack, so a token that would expire mid-request is refreshed
	// before it is used rather than after it fails.
	return t != nil && t.AccessToken != "" && time.Now().Add(time.Minute).Before(t.Expiry)
}

// authenticator turns config credentials into usable bearer tokens, caching the
// refresh token on disk so consent is a one-time act rather than a per-run one.
type authenticator struct {
	clientID     string
	clientSecret string
	tokenPath    string
	scopes       []string

	lock sync.Mutex
	tok  *token
}

// hasRequestedScopes reports whether the cached token actually carries
// everything this run needs.
//
// A cached token is not automatically good enough: consent granted for the base
// scopes says nothing about Drive. Without this check a Drive probe would fail
// as a puzzling 403 on the API call instead of simply asking for consent again.
func (a *authenticator) hasRequestedScopes() bool {
	if a.tok == nil {
		return false
	}
	granted := strings.Fields(a.tok.Scope)
	for _, want := range a.scopes {
		found := false
		for _, have := range granted {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

const missingCredentialsHelp = `missing google credentials in config.yaml.

Point at the JSON you downloaded when you created the "Desktop app" OAuth
client in the Google Cloud Console:

google:
  credentials_file: "client_secret_xxxxx.json"
  token_file: "db/google_token.json"

credentials_file is the file you DOWNLOAD (it contains client_id and
client_secret). token_file is written by this program after you sign in; it
does not exist yet and there is nothing to download for it.

An API key will NOT work: the Meet API only accepts OAuth user credentials.
Also make sure the Google Meet API is enabled on that project, and that your
own account is listed as a test user on the OAuth consent screen.`

// clientCredentials mirrors the JSON downloaded from the Cloud Console. Desktop
// clients nest under "installed", web clients under "web"; accept either, since
// telling them apart is not the user's job.
type clientCredentials struct {
	Installed struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"installed"`
	Web struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"web"`
}

func loadCredentialsFile(path string) (id string, secret string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("could not read credentials_file %q: %w", path, err)
	}
	var creds clientCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", "", fmt.Errorf("credentials_file %q is not valid JSON: %w", path, err)
	}
	if creds.Installed.ClientID != "" {
		return creds.Installed.ClientID, creds.Installed.ClientSecret, nil
	}
	if creds.Web.ClientID != "" {
		return creds.Web.ClientID, creds.Web.ClientSecret, nil
	}
	return "", "", fmt.Errorf("credentials_file %q has no client_id under \"installed\" or \"web\". Is it the OAuth client JSON from the Cloud Console?", path)
}

func newAuthenticator(cfg *config.Config, scopes []string) (*authenticator, error) {
	g := cfg.Google
	if g == nil {
		return nil, fmt.Errorf("%s", missingCredentialsHelp)
	}

	clientID, clientSecret := g.ClientID, g.ClientSecret
	if clientID == "" || clientSecret == "" {
		if g.CredentialsFile == "" {
			return nil, fmt.Errorf("%s", missingCredentialsHelp)
		}
		var err error
		clientID, clientSecret, err = loadCredentialsFile(g.CredentialsFile)
		if err != nil {
			return nil, err
		}
		fmt.Printf("Loaded OAuth client from %s\n", g.CredentialsFile)
	}

	tokenFile := g.TokenFile
	if tokenFile == "" {
		tokenFile = "db/google_token.json"
	}

	a := &authenticator{
		clientID:     clientID,
		clientSecret: clientSecret,
		tokenPath:    filepath.Join("data", tokenFile),
		scopes:       scopes,
	}
	a.loadToken()
	return a, nil
}

func (a *authenticator) loadToken() {
	data, err := os.ReadFile(a.tokenPath)
	if err != nil {
		return
	}
	var t token
	if err := json.Unmarshal(data, &t); err != nil {
		return
	}
	a.tok = &t
}

func (a *authenticator) saveToken() error {
	if err := os.MkdirAll(filepath.Dir(a.tokenPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.tok, "", "  ")
	if err != nil {
		return err
	}
	// 0600: this file is a standing grant on a real Google account.
	return os.WriteFile(a.tokenPath, data, 0o600)
}

// accessToken returns a usable bearer token, refreshing or re-consenting as
// needed.
func (a *authenticator) accessToken(ctx context.Context) (string, error) {
	a.lock.Lock()
	defer a.lock.Unlock()

	if a.tok.valid() && a.hasRequestedScopes() {
		return a.tok.AccessToken, nil
	}

	if a.tok != nil && a.tok.RefreshToken != "" && a.hasRequestedScopes() {
		if err := a.refresh(ctx); err != nil {
			fmt.Printf("token refresh failed (%v), falling back to a fresh consent\n", err)
		} else {
			return a.tok.AccessToken, nil
		}
	}

	if err := a.consent(ctx); err != nil {
		return "", err
	}
	return a.tok.AccessToken, nil
}

func (a *authenticator) refresh(ctx context.Context) error {
	form := url.Values{
		"client_id":     {a.clientID},
		"client_secret": {a.clientSecret},
		"refresh_token": {a.tok.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	newTok, err := a.postToken(ctx, form)
	if err != nil {
		return err
	}
	// A refresh response does not repeat the refresh token, so carry it over.
	if newTok.RefreshToken == "" {
		newTok.RefreshToken = a.tok.RefreshToken
	}
	a.tok = newTok
	return a.saveToken()
}

// consent runs the interactive browser flow against a loopback redirect, which
// is what "Desktop app" OAuth clients are built for: any 127.0.0.1 port is an
// acceptable redirect URI, so nothing has to be registered up front.
func (a *authenticator) consent(ctx context.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("failed to open loopback listener: %w", err)
	}
	defer listener.Close()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	authURL := fmt.Sprintf("%s?%s", authEndpoint, url.Values{
		"client_id":     {a.clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(a.scopes, " ")},
		"state":         {state},
		// offline + consent is what actually yields a refresh token. Without
		// them Google hands back an access token that dies in an hour, and the
		// bot would need a human to re-authorise it every morning.
		"access_type": {"offline"},
		"prompt":      {"consent"},
	}.Encode())

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			fmt.Fprintf(w, "Authorisation failed: %s. You can close this tab.", errStr)
			results <- result{err: fmt.Errorf("authorisation denied: %s", errStr)}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- result{err: fmt.Errorf("state mismatch on the oauth callback")}
			return
		}
		fmt.Fprint(w, "ultra-kiew is authorised. You can close this tab and go back to the terminal.")
		results <- result{code: q.Get("code")}
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	defer srv.Close()

	fmt.Println("\nOpen this URL in your browser to authorise ultra-kiew:")
	fmt.Printf("\n%s\n\n", authURL)
	openBrowser(authURL)
	fmt.Println("Waiting for the callback...")

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for the oauth callback")
	case res := <-results:
		if res.err != nil {
			return res.err
		}
		form := url.Values{
			"client_id":     {a.clientID},
			"client_secret": {a.clientSecret},
			"code":          {res.code},
			"grant_type":    {"authorization_code"},
			"redirect_uri":  {redirectURI},
		}
		tok, err := a.postToken(ctx, form)
		if err != nil {
			return err
		}
		if tok.RefreshToken == "" {
			fmt.Println("WARNING: Google returned no refresh token. Access will lapse in about an hour.")
		}
		a.tok = tok
		if err := a.saveToken(); err != nil {
			return err
		}
		fmt.Printf("Authorised. Token cached at %s\n", a.tokenPath)
		fmt.Printf("Granted scopes: %s\n", tok.Scope)
		return nil
	}
}

func (a *authenticator) postToken(ctx context.Context, form url.Values) (*token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("could not decode token response: %w", err)
	}
	if body.Error != "" {
		return nil, fmt.Errorf("token endpoint returned %s: %s", body.Error, body.ErrorDescription)
	}
	if body.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access token")
	}

	return &token{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
		Scope:        body.Scope,
	}, nil
}

func openBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("(could not open a browser automatically -- copy the URL above)")
	}
}
