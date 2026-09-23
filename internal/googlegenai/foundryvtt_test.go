package googlegenai

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gtrindade/ultra-kiew/internal/config"
)

func foundryClient(t *testing.T, dir string) *Client {
	t.Helper()
	return &Client{config: &config.Config{FoundryVTT: &config.FoundryConfig{Directory: dir}}}
}

func mkVersion(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, versionPrefix+version)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// linkOrSkip creates a symlink, skipping on systems that will not allow it.
// Windows needs developer mode or elevation; the server this runs on is Linux.
func linkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks need developer mode on Windows: %v", err)
		}
		t.Fatal(err)
	}
}

func TestListFoundryVersionsFindsPlainDirectories(t *testing.T) {
	dir := t.TempDir()
	mkVersion(t, dir, "12.331")
	mkVersion(t, dir, "13.331")

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	for _, want := range []string{"12.331", "13.331"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

// The strongest suspect for "it lists nothing while Foundry works fine".
//
// os.ReadDir reports the type of the directory entry itself, so a version that
// physically lives elsewhere and is symlinked into the install directory reads
// as "not a directory". The old code tested entry.IsDir() and skipped it in
// total silence.
func TestASymlinkedVersionDirectoryIsStillAVersion(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()

	real := filepath.Join(elsewhere, "foundry-13.331")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	linkOrSkip(t, real, filepath.Join(dir, versionPrefix+"13.331"))

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(got, "13.331") {
		t.Fatalf("a symlinked version must still be listed, got:\n%s", got)
	}
}

// The "(current)" marker never worked for a version this bot had switched to.
// switchFoundryVersion writes an ABSOLUTE symlink, and the old check compared
// the raw link text ("/home/x/foundry/FoundryVTT-13.331") against a bare
// directory name ("FoundryVTT-13.331"). Those are never equal.
func TestTheCurrentVersionIsMarkedForAnAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	mkVersion(t, dir, "12.331")
	target := mkVersion(t, dir, "13.331")
	linkOrSkip(t, target, filepath.Join(dir, symlink)) // absolute, as the bot writes it

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(got, "13.331 (current)") {
		t.Fatalf("expected 13.331 marked current, got:\n%s", got)
	}
	if strings.Contains(got, "12.331 (current)") {
		t.Errorf("only the linked version is current, got:\n%s", got)
	}
}

// And it must keep working for a link made by hand, which is relative.
func TestTheCurrentVersionIsMarkedForARelativeSymlink(t *testing.T) {
	dir := t.TempDir()
	mkVersion(t, dir, "13.331")
	linkOrSkip(t, versionPrefix+"13.331", filepath.Join(dir, symlink))

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(got, "13.331 (current)") {
		t.Fatalf("expected the relative link recognised, got:\n%s", got)
	}
}

// The listing has to survive a symlink pointing at a version that was deleted,
// rather than reporting nothing at all.
func TestADanglingCurrentSymlinkDoesNotBreakTheListing(t *testing.T) {
	dir := t.TempDir()
	mkVersion(t, dir, "13.331")
	linkOrSkip(t, filepath.Join(dir, versionPrefix+"99.999"), filepath.Join(dir, symlink))

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(got, "13.331") {
		t.Fatalf("expected the real version still listed, got:\n%s", got)
	}
}

// The whole point of the rewrite: a negative answer has to be diagnosable from
// the chat, without anyone opening an SSH session.
func TestAnEmptyListingSaysWhereItLookedAndWhatItSaw(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"foundry-13.331", "backups", "notes.txt"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatalf("an empty listing is not an error: %v", err)
	}

	if !strings.Contains(got, dir) {
		t.Errorf("expected the path it searched, got:\n%s", got)
	}
	if !strings.Contains(got, versionPrefix) {
		t.Errorf("expected the expected naming explained, got:\n%s", got)
	}
	for _, want := range []string{"foundry-13.331", "backups", "notes.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q reported as what was found, got:\n%s", want, got)
		}
	}
}

func TestAnEmptyDirectorySaysSo(t *testing.T) {
	got, err := foundryClient(t, t.TempDir()).listFoundryVersions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "completely empty") {
		t.Errorf("expected the empty directory called out, got:\n%s", got)
	}
}

// A long listing must not try to push an entire filesystem into a Telegram
// message.
func TestAnEmptyListingIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := range 40 {
		if err := os.MkdirAll(filepath.Join(dir, string(rune('a'+i%26))+strings.Repeat("x", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "more") {
		t.Errorf("expected the list truncated with a count, got:\n%s", got)
	}
}

// A missing foundry_vtt block used to dereference a nil pointer, and a panic
// inside a tool call is caught nowhere above it.
func TestNoFoundryConfigIsAnErrorRatherThanAPanic(t *testing.T) {
	c := &Client{config: &config.Config{}}

	if _, err := c.listFoundryVersions(); err == nil {
		t.Fatal("expected an error with no foundry_vtt block")
	} else if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected a clear explanation, got: %v", err)
	}

	if _, err := c.switchFoundryVersion("13.331"); err == nil {
		t.Fatal("switching must fail too")
	}
	if c.checkIfVersionExists("13.331") {
		t.Error("nothing exists when nothing is configured")
	}
}

func TestAnEmptyDirectorySettingIsReported(t *testing.T) {
	c := foundryClient(t, "   ")

	if _, err := c.listFoundryVersions(); err == nil {
		t.Fatal("expected an error for a blank directory setting")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected the blank setting named, got: %v", err)
	}
}

func TestAMissingDirectoryReportsThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nao-existe")

	_, err := foundryClient(t, missing).listFoundryVersions()
	if err == nil {
		t.Fatal("expected an error for a missing directory")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("expected the path in the error, got: %v", err)
	}
}

// A stray file named like a version is not a version, and neither is a
// dangling link.
func TestCheckIfVersionExistsRequiresARealDirectory(t *testing.T) {
	dir := t.TempDir()
	c := foundryClient(t, dir)

	if err := os.WriteFile(filepath.Join(dir, versionPrefix+"1.0"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c.checkIfVersionExists("1.0") {
		t.Error("a plain file is not a version")
	}

	mkVersion(t, dir, "13.331")
	if !c.checkIfVersionExists("13.331") {
		t.Error("a real directory is a version")
	}
}

// The install symlink itself must never show up as a version to switch to.
func TestTheSymlinkItselfIsNotListedAsAVersion(t *testing.T) {
	dir := t.TempDir()
	target := mkVersion(t, dir, "13.331")
	linkOrSkip(t, target, filepath.Join(dir, symlink))

	got, err := foundryClient(t, dir).listFoundryVersions()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "13.331") != 1 {
		t.Errorf("expected the version listed exactly once, got:\n%s", got)
	}
}
