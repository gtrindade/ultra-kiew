// Package google handles OAuth 2.0 user consent for the Google Meet API.
//
// This is deliberately narrow: one scope, one token file, no Drive. See
// cmd/meetspike for the exploration that settled on this shape -- in short, the
// Meet API has no API-key or plain service-account path on a consumer account,
// so the bot has to act as a real Google user who granted consent once, and the
// refresh token that consent produces is what lets every later run skip the
// browser.
package google

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"

	defaultTokenFile = "db/google_token.json"
)

// scopes is fixed rather than configurable: meetings.space.created is
// create-and-read on spaces this OAuth client itself made, and nothing else.
// There is no Drive scope here on purpose -- posting a link to a transcript or
// notes doc only needs the Meet API, and pulling in a scope this bot does not
// use would reopen the Testing-mode-vs-verification trade-off for no benefit.
var scopes = []string{
	"https://www.googleapis.com/auth/meetings.space.created",
	"https://www.googleapis.com/auth/meetings.space.readonly",
}

type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope"`
}

func (t *token) valid() bool {
	return t != nil && t.AccessToken != "" && time.Now().Add(time.Minute).Before(t.Expiry)
}

// Authenticator turns config credentials into usable bearer tokens, caching
// the refresh token on disk so consent is a one-time act rather than a
// per-run one.
type Authenticator struct {
	clientID     string
	clientSecret string
	tokenPath    string

	// NonInteractive stops AccessToken from ever falling back to the browser
	// consent flow, returning a plain error instead. The bot sets this: it
	// calls AccessToken from the event monitor's ticker goroutine, and a
	// consent flow that can never complete on a headless box would otherwise
	// block that goroutine for up to 5 minutes per attempt -- stalling event
	// reminders for every chat, not just the one that wanted a Meet link,
	// once every tick, indefinitely. cmd/meetspike leaves this false, since it
	// is a one-shot interactive tool where blocking for consent is the point.
	NonInteractive bool

	lock sync.Mutex
	tok  *token
}

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
	return "", "", fmt.Errorf("credentials_file %q has no client_id under \"installed\" or \"web\"", path)
}

// NewAuthenticator builds an Authenticator from config, or returns an error
// naming exactly what is missing. A nil *config.GoogleConfig is reported the
// same as a config missing credentials, so callers can treat "not configured"
// and "misconfigured" the same way: log it and run without Meet.
func NewAuthenticator(cfg *config.Config) (*Authenticator, error) {
	if cfg.Google == nil {
		return nil, fmt.Errorf("no [google] block in config.yaml")
	}
	g := cfg.Google

	clientID, clientSecret := g.ClientID, g.ClientSecret
	if clientID == "" || clientSecret == "" {
		if g.CredentialsFile == "" {
			return nil, fmt.Errorf("google config has neither client_id/client_secret nor credentials_file")
		}
		var err error
		clientID, clientSecret, err = loadCredentialsFile(g.CredentialsFile)
		if err != nil {
			return nil, err
		}
	}

	tokenFile := g.TokenFile
	if tokenFile == "" {
		tokenFile = defaultTokenFile
	}

	a := &Authenticator{
		clientID:     clientID,
		clientSecret: clientSecret,
		tokenPath:    filepath.Join(storage.BasePath, tokenFile),
	}
	a.loadToken()
	return a, nil
}

func (a *Authenticator) loadToken() {
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

func (a *Authenticator) saveToken() error {
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

// AccessToken returns a usable bearer token, refreshing or re-consenting as
// needed.
//
// Refreshing is a plain HTTP call with no browser involved, so once a token
// file exists (copied over from wherever the first interactive consent
// happened, if this process itself is headless) every subsequent run of the
// bot needs no human at all -- that is the entire point of caching the refresh
// token rather than just the access token.
func (a *Authenticator) AccessToken(ctx context.Context) (string, error) {
	a.lock.Lock()
	defer a.lock.Unlock()

	if err := a.ensureFreshTokenLocked(ctx); err == nil {
		return a.tok.AccessToken, nil
	} else if a.NonInteractive {
		return "", err
	}

	if err := a.consent(ctx); err != nil {
		return "", err
	}
	return a.tok.AccessToken, nil
}

// CheckToken reports whether a usable token is available right now, without
// ever opening a browser -- refreshing if that is possible, but nothing more.
//
// This exists so a caller can find out at startup, in one clear log line,
// exactly why Meet integration will not work: no token file has ever been
// created, or the one on disk has no refresh token, or the refresh token has
// been revoked. All three currently surface identically -- as CreateSpace
// quietly failing once a minute inside the event monitor -- which is exactly
// the class of problem worth catching before the first event ever needs it.
func (a *Authenticator) CheckToken(ctx context.Context) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	return a.ensureFreshTokenLocked(ctx)
}

// ensureFreshTokenLocked makes a.tok valid without ever consenting, or
// explains why it could not. Callers must hold a.lock.
func (a *Authenticator) ensureFreshTokenLocked(ctx context.Context) error {
	if a.tok.valid() {
		return nil
	}

	if a.tok == nil {
		return fmt.Errorf("no cached token at %s -- run interactive consent once (e.g. with cmd/meetspike) and copy the resulting file here, or point token_file at one that already exists", a.tokenPath)
	}
	if a.tok.RefreshToken == "" {
		return fmt.Errorf("the token cached at %s has no refresh token (consent may have been granted without offline access) -- redo consent", a.tokenPath)
	}

	if err := a.refresh(ctx); err != nil {
		return fmt.Errorf("refresh token at %s is no longer valid (%w) -- it may have been revoked; redo consent", a.tokenPath, err)
	}
	return nil
}

func (a *Authenticator) refresh(ctx context.Context) error {
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
	if newTok.RefreshToken == "" {
		newTok.RefreshToken = a.tok.RefreshToken
	}
	a.tok = newTok
	return a.saveToken()
}

// consent runs the interactive browser flow against a loopback redirect. It
// only ever triggers when there is no usable refresh token yet, which in
// normal operation should be never after the first run -- but if it does
// trigger on a headless box, this process needs a browser that can reach
// 127.0.0.1 on it (an SSH port-forward works), or the token file should be
// generated elsewhere and copied in instead.
func (a *Authenticator) consent(ctx context.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("failed to open loopback listener for oauth consent: %w", err)
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
		"scope":         {strings.Join(scopes, " ")},
		"state":         {state},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
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
		fmt.Fprint(w, "ultra-kiew is authorised. You can close this tab.")
		results <- result{code: q.Get("code")}
	})

	srv := &http.Server{Handler: mux}
	go func() {
		// ErrServerClosed is the deferred srv.Close below doing its job.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("google: local consent listener stopped: %v\n", err)
		}
	}()
	defer srv.Close()

	fmt.Printf("\ngoogle: open this URL to authorise ultra-kiew for Google Meet:\n\n%s\n\n", authURL)
	openBrowser(authURL)

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
			fmt.Println("google: WARNING no refresh token returned, access will lapse in about an hour")
		}
		a.tok = tok
		if err := a.saveToken(); err != nil {
			return err
		}
		fmt.Printf("google: authorised, token cached at %s\n", a.tokenPath)
		return nil
	}
}

func (a *Authenticator) postToken(ctx context.Context, form url.Values) (*token, error) {
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
	_ = cmd.Start()
}
