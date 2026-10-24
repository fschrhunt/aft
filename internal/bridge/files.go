package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arocomputer/aft/internal/bridge/transport"
	"github.com/arocomputer/aft/internal/home"
)

// Pasted files. A terminal pastes a file dragged or copied into it as its
// local path, which means nothing on the server. PastedFiles recognizes such
// a paste on your machine, Upload sends each file over the existing SSH
// connection, and Store (the server command "clipboard-put") keeps it in the
// clipboard directory under home.Temp, so the terminal can paste the server
// path instead. Stored files are removed after a week.

// keepPasted is how long pasted files stay on the host.
const keepPasted = 7 * 24 * time.Hour

// PastedFiles reads a paste as the whitespace-separated paths terminals
// produce for files, honoring backslash escapes and quotes. It returns the
// paths only when every word is an absolute or ~ path to an existing regular
// file (or one this process may not read), so ordinary text that merely
// mentions a path, or a path that exists only on the server, is never touched.
func PastedFiles(paste string) ([]string, bool) {
	words, ok := splitWords(paste)
	if !ok || len(words) == 0 {
		return nil, false
	}
	var paths []string
	for _, w := range words {
		if strings.HasPrefix(w, "~/") {
			h, err := os.UserHomeDir()
			if err != nil {
				return nil, false
			}
			w = filepath.Join(h, w[2:])
		}
		if !filepath.IsAbs(w) {
			return nil, false
		}
		fi, err := os.Stat(w)
		switch {
		case os.IsPermission(err):
			// macOS privacy rules can hide a file from the terminal. Try the
			// upload anyway so the failure is reported instead of silently
			// pasting a path the server cannot open.
		case err != nil || !fi.Mode().IsRegular():
			return nil, false
		}
		paths = append(paths, w)
	}
	return paths, true
}

// Droppable reports whether a pasted path is a file someone would drop in to
// share with the server: one under their home, a temporary directory or a
// removable volume. A path elsewhere, such as /etc/nginx/nginx.conf, is more
// likely a path on the server they are naming, so it stays text.
func Droppable(path string) bool {
	roots := []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders", "/Volumes", "/media", "/run/media", "/mnt"}
	if h, err := os.UserHomeDir(); err == nil {
		roots = append(roots, h)
	}
	for _, r := range roots {
		if rel, err := filepath.Rel(r, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// Upload copies each local file to the host and returns the server paths,
// shell-escaped the way terminals escape a dropped file. Cancelling ctx stops
// it.
func Upload(ctx context.Context, h transport.Host, paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		dest, err := transport.OutputContext(ctx, h, f, "clipboard-put", filepath.Base(p))
		f.Close()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		out = append(out, Escape(strings.TrimSpace(string(dest))))
	}
	return out, nil
}

// Store keeps one pasted file, read from r, as <clipboard dir>/<unique>/<name>
// and returns its absolute path. It runs on the server. name must be a bare
// file name; it is kept so the application sees what the user pasted.
func Store(name string, r io.Reader) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	tmp, err := home.EnsureTemp()
	if err != nil {
		return "", err
	}
	root := filepath.Join(tmp, "clipboard")
	prune(root, time.Now().Add(-keepPasted))
	var id [4]byte
	rand.Read(id[:])
	dir := filepath.Join(root, time.Now().UTC().Format("20060102-150405")+"-"+hex.EncodeToString(id[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.RemoveAll(dir)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return filepath.Abs(path)
}

// prune removes pasted-file directories last changed before cutoff.
func prune(root string, cutoff time.Time) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && e.IsDir() && fi.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// splitWords splits s like a POSIX shell would split plain words: whitespace
// separates, a backslash escapes the next character, and single or double
// quotes group. ok is false for an unterminated quote.
func splitWords(s string) (words []string, ok bool) {
	var cur strings.Builder
	in := false // a word is open, possibly empty ('')
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				esc = true
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			// Only the whitespace a terminal escapes in a dropped path; macOS
			// names hold a narrow no-break space (before AM or PM) that it
			// does not escape and that belongs to the name.
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || esc {
		return nil, false
	}
	if in {
		words = append(words, cur.String())
	}
	return words, true
}

// Escape backslash-escapes every character a shell treats specially,
// which is how terminals paste a dropped file's path.
func Escape(p string) string {
	var b strings.Builder
	for _, r := range p {
		// The same whitespace splitWords splits on; a narrow no-break space in
		// a macOS name is left as terminals leave it.
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || strings.ContainsRune(`\'"$`+"`"+`!&|;<>()[]{}*?#~`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
