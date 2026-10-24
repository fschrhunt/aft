package session

import (
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/arocomputer/aft/internal/home"
)

// names are the words sessions are called by: short, common animals, easy to
// say and type, none an aft command. A host's live sessions never share one.
var names = strings.Fields(`
	ant ape bat bear bee boar bug calf cat clam cod colt cow crab crow cub
	deer doe dog dove duck eel elk emu ewe fawn finch fish flea fly foal fox
	frog gnu goat goose hare hawk hen hog ibis jay kid kite kiwi koi lamb
	lark lion llama loon lynx mink mole moose moth mouse mule newt orca otter
	owl ox panda pig pony pug puma quail ram rat raven robin seal shark sheep
	shrew skunk sloth slug snail swan tern tick tiger toad trout tuna viper
	vole wasp whale wolf worm wren yak zebra
`)

// NewID picks a name no session on this host uses, live or ended with its
// remains still kept, adding a number only if every name is taken. It claims
// the name by creating its record file, empty, so two sessions starting at
// the same moment cannot pick the same one; Serve fills the record in, and
// Start removes an unfilled claim when the session fails to start.
func NewID() string {
	taken := map[string]bool{}
	list, _ := List()
	for _, r := range append(list, Remains()...) {
		taken[r.ID] = true
	}
	home.Ensure()
	for n := 1; ; n++ {
		for _, i := range rand.Perm(len(names)) {
			id := names[i]
			if n > 1 {
				id += strconv.Itoa(n)
			}
			if taken[id] {
				continue
			}
			f, err := os.OpenFile(RecordPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				continue // claimed by another session this instant
			}
			f.Close()
			return id
		}
	}
}

// ShellCommand is what aft HOST runs in a session: the user's login shell.
// It is kept here so the terminal, the server and the CLI agree on it.
var ShellCommand = []string{"sh", "-c", `exec "${SHELL:-/bin/sh}" -l`}

// IsShell reports whether a session runs ShellCommand.
func IsShell(command []string) bool { return slices.Equal(command, ShellCommand) }

// cleanTitle turns a window title into a session's description: apps such as
// Claude Code prefix it with a status glyph (✳, ⠐) that changes while they
// work. Only such glyphs, non-ASCII symbols and punctuation, are stripped from
// the front, so a title like ~/room/code or [1] build keeps its first
// character.
func cleanTitle(t string) string {
	t = strings.TrimLeftFunc(t, func(r rune) bool {
		return unicode.IsSpace(r) || (r > unicode.MaxASCII && (unicode.IsSymbol(r) || unicode.IsPunct(r)))
	})
	t = strings.TrimSpace(t)
	if r := []rune(t); len(r) > 60 {
		t = string(r[:59]) + "…"
	}
	return t
}
