package ui

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestDecodeKey pins the keypress classification the prompts rely on.
func TestDecodeKey(t *testing.T) {
	cases := []struct {
		in   []byte
		want key
	}{
		{[]byte{'y'}, keyYes},
		{[]byte{'Y'}, keyYes},
		{[]byte{'n'}, keyNo},
		{[]byte{'\r'}, keyEnter},
		{[]byte{'\n'}, keyEnter},
		{[]byte{3}, keyCancel},
		{[]byte{'q'}, keyCancel},
		{[]byte{27}, keyCancel},
		{[]byte{27, '[', 'A'}, keyUp},
		{[]byte{27, '[', 'B'}, keyDown},
		{[]byte{27, '[', 'C'}, keyRight},
		{[]byte{27, '[', 'D'}, keyLeft},
		{[]byte{'k'}, keyUp},
		{[]byte{'j'}, keyDown},
		{[]byte{'\t'}, keyRight},
		{[]byte{'z'}, keyOther},
		{nil, keyCancel},
	}
	for _, tc := range cases {
		if got := decodeKey(tc.in); got != tc.want {
			t.Errorf("decodeKey(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestNonInteractiveFallback checks prompts return the default without a tty,
// so scripted runs never block.
func TestNonInteractiveFallback(t *testing.T) {
	if Interactive() {
		t.Skip("stdin is a terminal in this run")
	}
	if !Confirm("x", true) || Confirm("x", false) {
		t.Fatal("Confirm must return its default without a terminal")
	}
	if Choose("x", []Option{{Label: "a"}, {Label: "b"}}, 1) != 1 {
		t.Fatal("Choose must return its default without a terminal")
	}
}

// TestTableAlignsStyledCells pads columns by visible width, so styled cells
// line up with plain ones.
func TestTableAlignsStyledCells(t *testing.T) {
	r, w, _ := os.Pipe()
	saved := os.Stdout
	os.Stdout = w
	Table([]string{"ID", "HOST"}, [][]string{{"\x1b[1mab\x1b[0m", "x"}, {"abcd", "y"}})
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	lines := strings.Split(ansi.Strip(string(out)), "\n")
	if lines[1] != "ab    x" || lines[2] != "abcd  y" {
		t.Fatalf("misaligned:\n%s", ansi.Strip(string(out)))
	}
}

// TestTableToleratesExtraCells prints a row with more cells than the header
// instead of panicking.
func TestTableToleratesExtraCells(t *testing.T) {
	r, w, _ := os.Pipe()
	saved := os.Stdout
	os.Stdout = w
	Table([]string{"A"}, [][]string{{"x", "extra"}})
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	if !strings.Contains(ansi.Strip(string(out)), "x  extra") {
		t.Fatalf("got %q", out)
	}
}
