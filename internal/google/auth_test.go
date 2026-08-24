package google

import (
	"context"
	"strings"
	"testing"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// The whole point of CheckToken is that each of these failure shapes produces
// a distinct, actionable message instead of the same silent "Meet just
// doesn't work" that shipped to production once already.
func TestCheckTokenExplainsWhyThereIsNoToken(t *testing.T) {
	storage.BasePath = t.TempDir()

	cfg := &config.Config{Google: &config.GoogleConfig{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		TokenFile:    "db/google_token.json", // deliberately never created
	}}

	auth, err := NewAuthenticator(cfg)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	auth.NonInteractive = true

	err = auth.CheckToken(context.Background())
	if err == nil {
		t.Fatal("expected an error when no token file has ever been created")
	}
	if !strings.Contains(err.Error(), "no cached token") {
		t.Errorf("expected a 'no cached token' explanation, got: %v", err)
	}
}

// AccessToken must never attempt interactive consent when NonInteractive is
// set -- that guard is what stops the event monitor's ticker goroutine from
// blocking for up to 5 minutes on a headless box with no way to complete the
// browser flow.
func TestAccessTokenNeverConsentsWhenNonInteractive(t *testing.T) {
	storage.BasePath = t.TempDir()

	cfg := &config.Config{Google: &config.GoogleConfig{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		TokenFile:    "db/google_token.json",
	}}

	auth, err := NewAuthenticator(cfg)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	auth.NonInteractive = true

	// This must return quickly with an error, not hang waiting on a loopback
	// listener nothing will ever call.
	_, err = auth.AccessToken(context.Background())
	if err == nil {
		t.Fatal("expected an error rather than an attempt to consent")
	}
}
