package google

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// writeTokenFile drops a token JSON at the default location under a fresh
// storage base path, and returns an Authenticator pointed at it.
func authWithToken(t *testing.T, body string) *Authenticator {
	t.Helper()
	base := t.TempDir()
	storage.BasePath = base

	if body != "" {
		path := filepath.Join(base, "db", "google_token.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	auth, err := NewAuthenticator(&config.Config{Google: &config.GoogleConfig{
		ClientID: "id", ClientSecret: "secret", TokenFile: "db/google_token.json",
	}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	auth.NonInteractive = true
	return auth
}

// A token is only usable if it will still be usable when the request lands, so
// validity carries a one-minute skew. Anything less and a token that expires
// mid-flight gets handed out as good.
func TestTokenValidity(t *testing.T) {
	cases := []struct {
		name  string
		tok   *token
		valid bool
	}{
		{"nil token", nil, false},
		{"no access token", &token{Expiry: time.Now().Add(time.Hour)}, false},
		{"already expired", &token{AccessToken: "x", Expiry: time.Now().Add(-time.Second)}, false},
		{"expiring inside the skew", &token{AccessToken: "x", Expiry: time.Now().Add(30 * time.Second)}, false},
		{"good for an hour", &token{AccessToken: "x", Expiry: time.Now().Add(time.Hour)}, true},
		{"zero expiry", &token{AccessToken: "x"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.valid(); got != tc.valid {
				t.Errorf("valid() = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestNewAuthenticatorRequiresCredentials(t *testing.T) {
	storage.BasePath = t.TempDir()

	cases := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{"no google block", &config.Config{}, "no [google] block"},
		{"nothing configured", &config.Config{Google: &config.GoogleConfig{}}, "neither client_id/client_secret nor credentials_file"},
		{"id without secret", &config.Config{Google: &config.GoogleConfig{ClientID: "x"}}, "neither client_id/client_secret nor credentials_file"},
		{"missing credentials file", &config.Config{Google: &config.GoogleConfig{CredentialsFile: "nope.json"}}, "could not read credentials_file"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAuthenticator(tc.cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error mentioning %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// The console hands out this file for a "Desktop app" client, and its two
// possible shapes ("installed" and "web") are why this reads both -- looking
// under only one key means telling the user their download is malformed when
// it is perfectly fine.
func TestLoadCredentialsFileReadsBothShapes(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	installed := write("installed.json", `{"installed":{"client_id":"id-a","client_secret":"secret-a"}}`)
	web := write("web.json", `{"web":{"client_id":"id-b","client_secret":"secret-b"}}`)
	junk := write("junk.json", `{"something_else":{}}`)
	notJSON := write("notjson.json", `hello`)

	if id, secret, err := loadCredentialsFile(installed); err != nil || id != "id-a" || secret != "secret-a" {
		t.Errorf("installed shape: got (%q, %q, %v)", id, secret, err)
	}
	if id, secret, err := loadCredentialsFile(web); err != nil || id != "id-b" || secret != "secret-b" {
		t.Errorf("web shape: got (%q, %q, %v)", id, secret, err)
	}
	if _, _, err := loadCredentialsFile(junk); err == nil || !strings.Contains(err.Error(), "no client_id") {
		t.Errorf("expected a 'no client_id' error for an unrecognised shape, got %v", err)
	}
	if _, _, err := loadCredentialsFile(notJSON); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected a JSON error, got %v", err)
	}
}

// Inline client_id/client_secret take priority over credentials_file. This
// pins that: with both set the file must not even be opened -- here it does
// not exist, so reading it would fail construction outright.
func TestInlineCredentialsWinOverTheFile(t *testing.T) {
	storage.BasePath = t.TempDir()

	auth, err := NewAuthenticator(&config.Config{Google: &config.GoogleConfig{
		ClientID:        "inline-id",
		ClientSecret:    "inline-secret",
		CredentialsFile: "does-not-exist.json",
	}})
	if err != nil {
		t.Fatalf("NewAuthenticator should not have read the file: %v", err)
	}
	if auth.clientID != "inline-id" || auth.clientSecret != "inline-secret" {
		t.Errorf("expected the inline credentials, got %q/%q", auth.clientID, auth.clientSecret)
	}
}

func TestTokenFileDefaultsWhenUnset(t *testing.T) {
	base := t.TempDir()
	storage.BasePath = base

	auth, err := NewAuthenticator(&config.Config{Google: &config.GoogleConfig{
		ClientID: "id", ClientSecret: "secret",
	}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	// Resolved against the storage base path, not the working directory, so
	// the token lives beside the rest of the bot's state.
	if want := filepath.Join(base, defaultTokenFile); auth.tokenPath != want {
		t.Errorf("token path = %q, want %q", auth.tokenPath, want)
	}
}

// A token file granted without offline access has no refresh token, and that
// needs its own message: redoing consent fixes it, whereas "no cached token"
// would send someone hunting for a file that is sitting right there.
func TestCheckTokenDistinguishesAMissingRefreshToken(t *testing.T) {
	auth := authWithToken(t, `{"access_token":"expired","refresh_token":"","expiry":"2020-01-01T00:00:00Z"}`)

	err := auth.CheckToken(context.Background())
	if err == nil {
		t.Fatal("expected an error for a token with no refresh token")
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("expected the missing-refresh-token explanation, got: %v", err)
	}
}

// A cached token still in date is handed straight back with no network call at
// all. That is what makes AccessToken cheap enough to call from the
// once-a-minute event monitor.
func TestAccessTokenReturnsAValidCachedTokenWithoutRefreshing(t *testing.T) {
	body := fmt.Sprintf(`{"access_token":"still-good","refresh_token":"r","expiry":%q}`,
		time.Now().Add(time.Hour).Format(time.RFC3339))
	auth := authWithToken(t, body)

	got, err := auth.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "still-good" {
		t.Errorf("expected the cached token, got %q", got)
	}
}

// A token file that will not parse must not pass for a valid one. loadToken
// leaves a.tok nil, so the caller is told there is no token rather than being
// handed an empty access token that every API call rejects with a confusing
// 401.
func TestACorruptTokenFileReadsAsNoToken(t *testing.T) {
	auth := authWithToken(t, `{"access_token":`)

	err := auth.CheckToken(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unparseable token file")
	}
	if !strings.Contains(err.Error(), "no cached token") {
		t.Errorf("expected it to read as no token at all, got: %v", err)
	}
}

// The scope list is the security boundary this package was designed around:
// no Drive scope, so the bot can create and read its own Meet spaces and
// nothing else. Adding one is a deliberate decision with consent-screen
// consequences, not something to slip in.
func TestScopesStayNarrow(t *testing.T) {
	for _, scope := range scopes {
		if strings.Contains(scope, "drive") {
			t.Errorf("a Drive scope was added (%q) -- see the package comment before keeping this", scope)
		}
	}
	if len(scopes) != 2 {
		t.Errorf("expected exactly the two Meet scopes, got %v", scopes)
	}
}
