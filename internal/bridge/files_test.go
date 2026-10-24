package bridge

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestStoreKeepsPastedFilePrivate checks that a pasted file lands in the
// clipboard directory under Aft's temp directory with its own name, that the
// directory is private, and that a name cannot escape it.
func TestStoreKeepsPastedFilePrivate(t *testing.T) {
	t.Setenv("AFT_HOME", t.TempDir())
	path, err := Store("Screen Recording.mov", strings.NewReader("video"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "video" || filepath.Base(path) != "Screen Recording.mov" {
		t.Fatalf("stored %q at %s", b, path)
	}
	tmp := filepath.Join(os.Getenv("AFT_HOME"), "tmp")
	if !strings.HasPrefix(path, filepath.Join(tmp, "clipboard")+"/") {
		t.Fatalf("stored outside the clipboard directory: %s", path)
	}
	if fi, _ := os.Stat(tmp); fi.Mode().Perm() != 0o700 {
		t.Fatalf("temp directory mode %v", fi.Mode().Perm())
	}
	if _, err := Store("../escape", strings.NewReader("x")); err == nil {
		t.Fatal("a name with a path must be refused")
	}
}

// TestStorePrunesOldFiles checks that pasted files older than a week go.
func TestStorePrunesOldFiles(t *testing.T) {
	t.Setenv("AFT_HOME", t.TempDir())
	old, _ := Store("old.png", strings.NewReader("x"))
	week := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(filepath.Dir(old), week, week)
	if _, err := Store("new.png", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old pasted file was kept")
	}
}

// TestPastedFilesReadsTerminalPaths pins the forms terminals paste for a
// dropped or copied file: backslash-escaped spaces and quoted paths, several
// separated by spaces.
func TestPastedFilesReadsTerminalPaths(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "Screen Recording 1.44.00 AM.mov")
	b := filepath.Join(dir, "shot.png")
	for _, p := range []string{a, b} {
		os.WriteFile(p, []byte("x"), 0o600)
	}
	got, ok := PastedFiles(Escape(a) + " '" + b + "' ")
	if !ok || !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

// TestPastedFilesLeavesTextAlone checks that a paste is only rewritten when it
// is nothing but existing local files.
func TestPastedFilesLeavesTextAlone(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "notes.txt")
	os.WriteFile(f, []byte("x"), 0o600)
	for _, body := range []string{
		"look at " + f,                // prose mentioning a path
		filepath.Join(dir, "missing"), // not on this machine
		dir,                           // a directory
		"notes.txt",                   // relative
		"'" + f,                       // unterminated quote
	} {
		if _, ok := PastedFiles(body); ok {
			t.Errorf("rewrote %q", body)
		}
	}
}

// TestPastedFilesKeepsMacNarrowSpace reads a dragged screen recording, whose
// macOS name has a narrow no-break space before PM that terminals do not
// escape, as one file.
func TestPastedFilesKeepsMacNarrowSpace(t *testing.T) {
	dir := t.TempDir()
	name := "Screen Recording 2026-10-24 at 12.00.12 PM.mov"
	os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	paste := strings.ReplaceAll(filepath.Join(dir, name), " ", `\ `)
	got, ok := PastedFiles(paste)
	if !ok || len(got) != 1 || filepath.Base(got[0]) != name {
		t.Fatalf("PastedFiles(%q) = %q, %v", paste, got, ok)
	}
	// Pasted back to the app, the name is escaped as a terminal would: only
	// the ordinary space gets a backslash.
	if e := Escape(name); strings.Contains(e, "\\\u202f") || !strings.Contains(e, `Screen\ Recording`) {
		t.Fatalf("Escape(%q) = %q", name, e)
	}
}

// TestDroppable copies files dropped from home or a temporary directory, and
// leaves system paths, which likely name files on the server, as text.
func TestDroppable(t *testing.T) {
	h, _ := os.UserHomeDir()
	for p, want := range map[string]bool{
		filepath.Join(h, "Desktop", "a.png"):   true,
		filepath.Join(os.TempDir(), "rec.mov"): true,
		"/etc/nginx/nginx.conf":                false,
		"/usr/local/bin/tool":                  false,
	} {
		if got := Droppable(p); got != want {
			t.Errorf("Droppable(%q) = %v, want %v", p, got, want)
		}
	}
}
