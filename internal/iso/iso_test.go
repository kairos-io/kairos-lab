package iso

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SelectDownloaded takes no input when exactly one ISO is cached, so without
// an explicit line it attaches that ISO with no output at all. That is the
// branch a dropped `start <path>` argument fell through to in
// kairos-io/kairos#4432, which is why the wrong ISO booted unnoticed.
func TestSelectDownloadedNamesTheOnlyCachedISO(t *testing.T) {
	dir := t.TempDir()
	only := filepath.Join(dir, "kairos-ubuntu-24.04-core-arm64-generic-v1.0.0.iso")
	if err := os.WriteFile(only, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	got, err := SelectDownloaded(SelectConfig{
		DownloadsDir: dir,
		Stdin:        strings.NewReader(""),
		Stdout:       &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != only {
		t.Fatalf("got %q, want %q", got, only)
	}
	if !strings.Contains(stdout.String(), filepath.Base(only)) {
		t.Fatalf("the selected ISO was not named on stdout, got %q", stdout.String())
	}
}

// With more than one cached ISO the user picks, so the choice is already
// visible and the auto-select line must not appear.
func TestSelectDownloadedPromptsWhenSeveralAreCached(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"kairos-a.iso", "kairos-b.iso"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("iso"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout bytes.Buffer
	got, err := SelectDownloaded(SelectConfig{
		DownloadsDir: dir,
		Stdin:        strings.NewReader("2\n"),
		Stdout:       &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "kairos-b.iso" {
		t.Fatalf("got %q, want kairos-b.iso", got)
	}
	if strings.Contains(stdout.String(), "only downloaded ISO") {
		t.Fatalf("auto-select line printed on the prompting path, got %q", stdout.String())
	}
}
