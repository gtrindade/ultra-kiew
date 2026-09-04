package config

import (
	"os"
	"path/filepath"
	"testing"
)

// inTempDir runs the test with the working directory pointed at a scratch
// directory, since LoadFromFile reads a fixed relative path.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("could not restore the working directory: %v", err)
		}
	})
	return dir
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configFilePath), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFromFileReadsEverySection(t *testing.T) {
	dir := inTempDir(t)
	writeConfig(t, dir, `
telegram_bot_token: tg-token
gemini_api_key: gemini-key
bot_name: kiew
dnd_tools:
  host: localhost
  port: "3306"
  user: root
  password: secret
  name: dnd
google:
  credentials_file: creds.json
  token_file: db/google_token.json
`)

	cfg, err := LoadFromFile()
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.TelegramBotToken != "tg-token" || cfg.GeminiAPIKey != "gemini-key" || cfg.BotName != "kiew" {
		t.Errorf("top-level fields not read: %+v", cfg)
	}
	if cfg.DNDTools == nil || cfg.DNDTools.Host != "localhost" || cfg.DNDTools.Name != "dnd" {
		t.Errorf("dnd_tools not read: %+v", cfg.DNDTools)
	}
	if cfg.Google == nil || cfg.Google.CredentialsFile != "creds.json" {
		t.Errorf("google block not read: %+v", cfg.Google)
	}
}

// Every optional block is a pointer precisely so "absent" is distinguishable
// from "present but empty" -- setUpMeet logs a different line for each, and
// NewAuthenticator reports them differently too.
func TestOmittedSectionsStayNil(t *testing.T) {
	dir := inTempDir(t)
	writeConfig(t, dir, "telegram_bot_token: only-this\n")

	cfg, err := LoadFromFile()
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.Google != nil {
		t.Errorf("expected a nil google block, got %+v", cfg.Google)
	}
	if cfg.DNDTools != nil || cfg.SRD != nil || cfg.FoundryVTT != nil {
		t.Error("expected every omitted section to stay nil")
	}
}

func TestAPresentButEmptyGoogleBlockIsNotNil(t *testing.T) {
	dir := inTempDir(t)
	writeConfig(t, dir, "google:\n  client_id: \"\"\n")

	cfg, err := LoadFromFile()
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	// This is the "misconfigured" case rather than the "not configured" one,
	// and the two produce different startup messages.
	if cfg.Google == nil {
		t.Fatal("an explicitly written google block should not decode as nil")
	}
}

func TestMissingFileIsReportedRatherThanIgnored(t *testing.T) {
	inTempDir(t)

	if _, err := LoadFromFile(); err == nil {
		t.Fatal("expected an error when config.yaml does not exist")
	}
}

func TestMalformedYAMLIsReported(t *testing.T) {
	dir := inTempDir(t)
	writeConfig(t, dir, "telegram_bot_token: [unclosed\n")

	if _, err := LoadFromFile(); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}
