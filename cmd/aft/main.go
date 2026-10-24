// aft runs your agents on your server while the terminal stays on your machine.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arocomputer/aft/internal/bridge"
	"github.com/arocomputer/aft/internal/config"
	"github.com/arocomputer/aft/internal/home"
	"github.com/arocomputer/aft/internal/launcher"
	"github.com/arocomputer/aft/internal/session"
	"github.com/arocomputer/aft/internal/ui"
)

// commands is the help screen: groups of command, arguments and summary.
var commands = []struct {
	group string
	cmds  [][3]string
}{
	{"Start", [][3]string{
		{"setup", "", "add a host, create launchers, add the PATH block"},
		{"on", "[--host H] [--dir D]", "this folder runs on the host; creates it there if needed"},
		{"off", "", "run them locally again"},
	}},
	{"Sessions", [][3]string{
		{"HOST", "[CMD...]", "a terminal on that host that survives disconnects; CMD runs once"},
		{"ls", "", "sessions on every host"},
		{"attach", "[NAME]", "reattach to a session"},
		{"kill", "NAME", "stop a session"},
	}},
	{"Hosts", [][3]string{
		{"host", "[add|rm|use]", "list your hosts, add or remove one, pick the default"},
		{"mount", "list|add|remove", "map a network-mounted folder"},
	}},
	{"Launchers", [][3]string{
		{"agents add", "NAME...", "programs that run in sessions on the server"},
		{"tools add", "NAME...", "commands like git and gh that run there once"},
	}},
	{"Maintain", [][3]string{
		{"status", "", "where you are, hosts, sessions, ports, projects"},
		{"doctor", "[--fix]", "check SSH, server, agents, launchers, shell block"},
		{"uninstall", "", "remove everything Aft added, everywhere"},
		{"completion", "bash|zsh|fish", "shell completion"},
		{"version", "", "print the version"},
		{"help", "", "this list"},
	}},
}

// meant maps names people reach for, including commands aft no longer has, to
// what they are after. A command is a word, never a flag, so --help is
// answered with a suggestion rather than accepted.
var meant = map[string]string{
	"list": "ls", "sessions": "ls", "ps": "ls",
	"end": "kill", "stop": "kill", "rm": "kill",
	"ssh": "HOST", "shell": "HOST", "connect": "HOST",
	"use": "host use", "hosts": "host",
	"clone": "on", "ports": "status", "exec": "tools add", "run": "tools add",
	"check": "doctor", "init": "setup", "install": "setup", "remove": "uninstall",
}

// isCommand reports whether name is one of aft's commands.
func isCommand(name string) bool {
	switch name {
	case "selftest":
		return true
	}
	for _, g := range commands {
		for _, c := range g.cmds {
			if strings.Fields(c[0])[0] == name {
				return true
			}
		}
	}
	return false
}

// suggest returns the command someone probably meant by an unknown word, or
// empty: a known slip, the word without its dashes, or a near spelling.
func suggest(word string) string {
	w := strings.ToLower(strings.TrimLeft(word, "-"))
	switch w {
	case "h":
		w = "help"
	case "v":
		w = "version"
	}
	if m, ok := meant[w]; ok {
		return m
	}
	best, bestD := "", 3
	for _, g := range commands {
		for _, c := range g.cmds {
			name := strings.Fields(c[0])[0]
			if name == "HOST" {
				continue // a placeholder, not a command
			}
			if name == w && w != word {
				return name // --help, -version
			}
			if d := distance(w, name); d < bestD && d <= len(name)/2 {
				best, bestD = name, d
			}
		}
	}
	return best
}

// distance is the edit distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// printUsage prints the help screen: the commands by group.
func printUsage() {
	fmt.Println(ui.Bold("aft") + ui.Dim(" — your agents on your server, your terminal on your machine"))
	for _, g := range commands {
		fmt.Println("\n" + ui.Bold(g.group))
		for _, c := range g.cmds {
			name := fmt.Sprintf("%-11s", c[0])
			fmt.Printf("  %s %s%s\n", ui.Accent(name), fmt.Sprintf("%-22s", c[1]), ui.Dim(c[2]))
		}
	}
	fmt.Println("\n" + ui.Dim("In a project that is on, run claude, codex, opencode or pi exactly as before."))
}

// version is set by the build.
var version = "dev"

func main() {
	session.Version = version
	base := filepath.Base(os.Args[0])
	if bridge.IsShim(base) {
		os.Exit(bridge.RunShim(os.Args[0], os.Args[1:]))
	}
	if base == home.ServerName {
		os.Exit(serverMain(os.Args[1:]))
	}
	if home.Migrate() {
		moveShellBlocks()
	}
	// Invoked under an agent's name (via a launcher symlink), run that agent
	// on the server; invoked as aft (or a dev build whose name starts with
	// "aft"), act as the CLI.
	if !strings.HasPrefix(base, "aft") {
		os.Exit(launcher.Launch(base, os.Args[1:]))
	}
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}
	os.Exit(run(os.Args[1], os.Args[2:]))
}

// moveShellBlocks points the PATH line at the launchers' new home and tells
// you about any line it could not change. Remove it with home.Migrate.
func moveShellBlocks() {
	st, err := config.LoadState()
	if err != nil {
		return
	}
	kept := launcher.MoveShellBlocks(&st)
	config.SaveState(st)
	ui.Note("launchers moved to %s; open a new shell to use them", ui.Path(home.Launchers()))
	if len(st.ShellBlocks) == 0 || len(kept) > 0 {
		ui.Warn("put this first on your PATH instead of $HOME/.aft/launchers%s:", inFiles(kept))
		fmt.Fprintf(os.Stderr, "    export PATH=\"%s:$PATH\"\n", launcher.PathDir)
	}
}

// inFiles names the files a shell block was found edited in, for a message.
func inFiles(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return " in " + strings.Join(files, ", ")
}

func run(cmd string, args []string) int {
	switch cmd {
	case "help":
		printUsage()
		return 0
	case "version":
		fmt.Println("aft", version)
		return 0
	case "setup":
		return cmdSetup(args)
	case "host":
		return cmdHost(args)
	case "on":
		return cmdOn(args)
	case "off":
		return cmdOff(args)
	case "ls":
		return cmdLs(args)
	case "attach":
		return cmdAttach(args)
	case "kill":
		return cmdKill(args)
	case "status":
		return cmdStatus(args)
	case "doctor":
		return cmdDoctor(args)
	case "mount":
		return cmdMount(args)
	case "agents", "tools":
		return cmdLaunchers(cmd, args)
	case "uninstall":
		return cmdUninstall(args)
	case "selftest":
		return cmdSelftest(args)
	case "completion":
		return cmdCompletion(args)
	}
	// aft HOST: a host you have, or one SSH can reach, which becomes one.
	if cfg, err := config.Load(); err == nil {
		if _, ok := cfg.Hosts[cmd]; ok || cmd == "local" {
			h, _ := cfg.Host(cmd)
			return cmdHostShell(cfg, h, args)
		}
		plainly := strings.Contains(cmd, "@")
		if !strings.HasPrefix(cmd, "-") && (plainly || suggest(cmd) == "") {
			if h, ok := adoptHost(&cfg, cmd, !plainly); ok {
				return cmdHostShell(cfg, h, args)
			}
			if plainly {
				return 1
			}
		}
	}
	ui.Error("unknown command %q", cmd)
	if m := suggest(cmd); m != "" {
		if cfg, err := config.Load(); m == "HOST" && err == nil && len(cfg.Hosts) == 1 {
			m = cfg.HostNames()[0]
		}
		ui.Suggest("aft " + m)
	} else {
		ui.Hint("aft help lists the commands")
	}
	return 2
}

// serverMain runs the session server: aft started as aft-server is the server
// for sessions on this machine, the same program a host runs as aft-server.
func serverMain(args []string) int {
	return session.Main(nil, args, os.Stdin, os.Stdout, os.Stderr)
}

// fail reports err on stderr and returns the exit code for a failed command.
func fail(err error) int {
	ui.Error("%v", err)
	return 1
}
