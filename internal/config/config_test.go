package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindProjectLongestPrefix picks the most specific on project containing a
// directory, and nothing when none contains it.
func TestFindProjectLongestPrefix(t *testing.T) {
	s := State{Projects: map[string]Project{
		"/home/me/code":     {Host: "a", Dir: "/srv/code"},
		"/home/me/code/app": {Host: "b", Dir: "/srv/app"},
	}}
	root, p, ok := s.FindProject("/home/me/code/app/sub")
	if !ok || root != "/home/me/code/app" || p.Host != "b" {
		t.Fatalf("got %q %+v %v", root, p, ok)
	}
	if _, _, ok := s.FindProject("/home/me/other"); ok {
		t.Fatal("matched outside any project")
	}
	// A sibling whose name shares a prefix must not match.
	if _, _, ok := s.FindProject("/home/me/code-x"); ok {
		t.Fatal("prefix without a path separator matched")
	}
}

// TestRemoteDir maps a subdirectory to the server path.
func TestRemoteDir(t *testing.T) {
	p := Project{Dir: "/srv/app"}
	if got := RemoteDir("/home/me/app", p, "/home/me/app"); got != "/srv/app" {
		t.Fatalf("root: %q", got)
	}
	if got := RemoteDir("/home/me/app", p, "/home/me/app/pkg/x"); got != "/srv/app/pkg/x" {
		t.Fatalf("sub: %q", got)
	}
}

// TestDetachByte accepts control-key spellings and rejects the rest.
func TestDetachByte(t *testing.T) {
	cases := map[string]byte{"": 0, "off": 0, "ctrl-]": 29, "^]": 29, "ctrl-a": 1}
	for in, want := range cases {
		got, err := DetachByte(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d %v want %d", in, got, err, want)
		}
	}
	if _, err := DetachByte("ctrl-ab"); err == nil {
		t.Error("accepted a two-letter key")
	}
}

// TestDefaultRemoteDir mirrors the path relative to home.
func TestDefaultRemoteDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := DefaultRemoteDir(filepath.Join(home, "code", "app")); got != "~/code/app" {
		t.Fatalf("under home: %q", got)
	}
	if got := DefaultRemoteDir(home); got != "~" {
		t.Fatalf("home itself: %q", got)
	}
	if got := DefaultRemoteDir("/opt/x"); got != "/opt/x" {
		t.Fatalf("outside home: %q", got)
	}
}

// TestLoadRepo finds aft.toml walking up from a subdirectory.
func TestLoadRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aft.toml"), []byte("host = \"box\"\ndir = \"/srv/x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	r, root, ok := LoadRepo(sub)
	if !ok || root != dir || r.Host != "box" || r.Dir != "/srv/x" {
		t.Fatalf("got %+v %q %v", r, root, ok)
	}
	if _, _, ok := LoadRepo(t.TempDir()); ok {
		t.Fatal("found aft.toml where there is none")
	}
}

// TestFindProjectThroughSymlink finds a project, kept by its real path, from a
// current directory spelled through a symlink, and maps the subdirectory to
// the server.
func TestFindProjectThroughSymlink(t *testing.T) {
	real, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(real, "code", "app"), 0o755)
	link := filepath.Join(t.TempDir(), "room")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	st := State{Projects: map[string]Project{filepath.Join(real, "code"): {Host: "box", Dir: "/srv/code"}}}
	root, p, ok := st.FindProject(filepath.Join(link, "code", "app"))
	if !ok || p.Host != "box" {
		t.Fatalf("not found: %q %+v", root, p)
	}
	if got := RemoteDir(root, p, filepath.Join(link, "code", "app")); got != "/srv/code/app" {
		t.Fatalf("RemoteDir = %q", got)
	}
}
