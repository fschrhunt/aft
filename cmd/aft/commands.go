package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/arocomputer/aft/internal/bridge/proto"
	"github.com/arocomputer/aft/internal/bridge/transport"
	"github.com/arocomputer/aft/internal/config"
	"github.com/arocomputer/aft/internal/home"
	"github.com/arocomputer/aft/internal/launcher"
	"github.com/arocomputer/aft/internal/session"
	"github.com/arocomputer/aft/internal/session/install"
	"github.com/arocomputer/aft/internal/terminal"
	"github.com/arocomputer/aft/internal/ui"
)

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func ask(prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// connect opens the shared SSH connection to h behind a spinner. It tries
// without a terminal first and, if SSH needs one (a passphrase, a new host
// key), clears the spinner and asks in the terminal.
func connect(h transport.Host) error {
	if h.IsLocal() || transport.MasterAlive(h) {
		return nil
	}
	l := ui.Start("Connecting to "+h.Name, 0)
	defer l.Stop()
	err := transport.EnsureMasterBatch(h)
	if err == nil || !transport.NeedsTerminal(err) {
		return err
	}
	l.Pause()
	defer l.Resume()
	return transport.EnsureMaster(h)
}

// ensureServer installs or updates the server on h behind a spinner.
func ensureServer(h transport.Host) (session.Info, error) {
	var info session.Info
	err := ui.Spin("Checking the server on "+h.Name, func() (err error) {
		info, err = install.Ensure(h)
		return err
	})
	return info, err
}

// cmdSetup adds a server and prepares this machine.
func cmdSetup(args []string) int {
	fs := flags("setup")
	hostFlag := fs.String("host", "", "SSH destination, usually an alias from ~/.ssh/config")
	name := fs.String("name", "", "name for the host (default: the SSH destination)")
	yes := fs.Bool("yes", false, "do not ask; also skip the server if --host is empty")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	st, err := config.LoadState()
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	self, _ = filepath.EvalSymlinks(self)

	dest := *hostFlag
	if dest == "" && !*yes {
		dest = ask("SSH destination for your server (alias from ~/.ssh/config; empty to skip)", "")
	}
	if dest != "" {
		n := *name
		if n == "" {
			n = nameFor(dest)
		}
		if err := checkHostName(n); err != nil {
			return fail(err)
		}
		keepDefault(&cfg)
		cfg.Hosts[n] = config.HostConfig{SSH: dest}
		h, _ := cfg.Host(n)
		if err := connect(h); err != nil {
			return fail(err)
		}
		info, err := ensureServer(h)
		if err != nil {
			return fail(err)
		}
		ui.OK("Server ready on %s %s", ui.Bold(n), ui.Dim(info.OS+"/"+info.Arch+" · "+info.Version))
		if found := agentsIn(info.Agents); len(found) == 0 {
			ui.Warn("no agents on %s yet; install one there (claude, codex, opencode, pi)", n)
			ui.Hint("then run aft doctor")
		} else {
			ui.OK("Agents on %s: %s", n, strings.Join(found, ", "))
		}
	}
	if len(cfg.Agents) == 0 {
		cfg.Agents = config.DefaultAgents
	}
	if err := launcher.InstallLaunchers(self, cfg.Launched(), &st); err != nil {
		return fail(err)
	}
	ui.OK("Launchers for %s %s", strings.Join(cfg.Launched(), ", "), ui.Dim("in "+ui.Path(home.Launchers())))

	// Decide how the PATH line reaches the shell. People who manage their
	// dotfiles want to add it themselves, so ask rather than edit blindly.
	if cfg.EditDotfiles == nil && !*yes {
		b := chooseEditDotfiles()
		cfg.EditDotfiles = &b
	}
	if cfg.EditingDotfiles() {
		changed, err := launcher.InstallShellBlock(&st)
		if err != nil {
			return fail(err)
		}
		for _, f := range changed {
			ui.OK("Added the PATH line to %s %s", ui.Path(f), ui.Dim("· open a new terminal so it takes effect"))
		}
	} else {
		fmt.Println("Add this line wherever your shell sets PATH, then apply it with your dotfile manager:")
		fmt.Println("\n    " + ui.Command("export PATH=\""+launcher.PathDir+":$PATH\"") + "\n")
	}
	if err := config.Save(cfg); err != nil {
		return fail(err)
	}
	if err := config.SaveState(st); err != nil {
		return fail(err)
	}
	fmt.Println("\n" + ui.Bold("Next") + "  cd into a project, run " + ui.Command("aft on") + ", then start your agent as usual.")
	return 0
}

// chooseEditDotfiles asks whether aft should edit the shell config itself,
// defaulting to no when the shell config looks managed.
func chooseEditDotfiles() bool {
	def := 0
	if launcher.DotfilesManaged() {
		def = 1
	}
	i := ui.Choose("How should aft put its launchers on your PATH?", []ui.Option{
		{Label: "Edit my shell config for me", Hint: "~/.zshrc or ~/.bashrc"},
		{Label: "Show me the line, I manage my dotfiles", Hint: "chezmoi, stow, nix, a repo"},
	}, def)
	return i == 0
}

// cmdHost manages the [host.<name>] tables in config.toml. With no verb it
// lists them; add and rm edit the tables. Which host is used by default is the
// `use` key, which `aft host use` sets (see hostUse). These only touch local config — removing a
// host leaves whatever is installed on it.
func cmdHost(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	verb := ""
	if len(args) > 0 {
		verb = args[0]
		args = args[1:]
	}
	switch verb {
	case "", "ls", "list":
		if len(cfg.Hosts) == 0 {
			fmt.Println("No hosts yet.")
			ui.Hint("aft host add NAME SSH-DESTINATION")
			return 0
		}
		used := cfg.UsedHost()
		var rows [][]string
		for _, n := range cfg.HostNames() {
			mark := ""
			if n == used {
				mark = ui.Accent("default")
			}
			rows = append(rows, []string{ui.Bold(n), cfg.Hosts[n].SSH, mark})
		}
		ui.Table([]string{"HOST", "SSH", ""}, rows)
		return 0

	case "add":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: aft host add NAME SSH-DESTINATION")
			return 2
		}
		name, dest := args[0], args[1]
		if err := checkHostName(name); err != nil {
			return fail(err)
		}
		if _, ok := cfg.Hosts[name]; ok {
			return fail(fmt.Errorf("host %q already exists; remove it first or edit config.toml", name))
		}
		keepDefault(&cfg)
		cfg.Hosts[name] = config.HostConfig{SSH: dest}
		if err := config.Save(cfg); err != nil {
			return fail(err)
		}
		ui.OK("Added host %s %s", ui.Bold(name), ui.Dim(dest))
		h, _ := cfg.Host(name)
		if err := connect(h); err != nil {
			ui.Error("saved, but could not connect yet: %v", err)
			ui.Hint("fix the SSH destination, then run aft doctor --fix")
			return 1
		}
		info, err := ensureServer(h)
		if err != nil {
			return fail(err)
		}
		ui.OK("Server ready on %s %s", ui.Bold(name), ui.Dim(info.OS+"/"+info.Arch+" · "+info.Version))
		if cfg.UsedHost() == name {
			fmt.Println(ui.Dim(name + " is used by default."))
		} else {
			ui.Hint("aft host use %s makes it the default, or pass --host %s per command", name, name)
		}
		return 0

	case "use":
		return hostUse(cfg, args)

	case "rm", "remove":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: aft host rm NAME")
			return 2
		}
		name := args[0]
		if _, ok := cfg.Hosts[name]; !ok {
			return fail(fmt.Errorf("unknown host %q", name))
		}
		delete(cfg.Hosts, name)
		if cfg.Use == name {
			cfg.Use = "" // fall back to the only remaining host, or to aft host use
		}
		if err := config.Save(cfg); err != nil {
			return fail(err)
		}
		ui.OK("Removed host %s from your config", ui.Bold(name))
		fmt.Println(ui.Dim("Whatever Aft installed on it stays; run aft uninstall on that machine to clean it up."))
		switch used := cfg.UsedHost(); {
		case used != "":
			fmt.Println(ui.Dim(used + " is now used by default."))
		case len(cfg.Hosts) > 1:
			ui.Hint("aft host use NAME picks the default host")
		}
		return 0
	}
	ui.Error("aft host: unknown verb %q", verb)
	ui.Hint("aft host lists; add NAME DEST, rm NAME and use NAME change them")
	return 2
}

// keepDefault, called before a host is added, writes down the host that is
// the default only because it is the only one, so adding a second does not
// leave aft without a default.
func keepDefault(cfg *config.Config) {
	if cfg.Use == "" && len(cfg.Hosts) == 1 {
		cfg.Use = cfg.HostNames()[0]
	}
}

// hostUse sets the host used when none is given (config key `use`). With no
// name it prints the current one.
func hostUse(cfg config.Config, args []string) int {
	if len(args) == 0 {
		if used := cfg.UsedHost(); used != "" {
			fmt.Println(used)
			return 0
		}
		ui.Warn("no host is used by default")
		ui.Hint("aft host use NAME")
		return 1
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: aft host use NAME")
		return 2
	}
	name := args[0]
	if _, ok := cfg.Hosts[name]; !ok {
		return fail(fmt.Errorf("unknown host %q; add it with aft host add", name))
	}
	cfg.Use = name
	if err := config.Save(cfg); err != nil {
		return fail(err)
	}
	ui.OK("Using %s by default", ui.Bold(name))
	return 0
}

func agentsIn(m map[string]string) []string {
	var out []string
	for _, a := range config.DefaultAgents {
		if _, ok := m[a]; ok {
			out = append(out, a)
		}
	}
	return out
}

// cmdOn maps the current directory to a server directory.
func cmdOn(args []string) int {
	fs := flags("on")
	hostName := fs.String("host", "", "host name from aft setup")
	dir := fs.String("dir", "", "directory on the server, created if missing (default: same path relative to home)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	named := *dir != "" // a folder the user named is one they want
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	st, err := config.LoadState()
	if err != nil {
		return fail(err)
	}
	// The project is exactly this folder, kept by its real path so it is found
	// from either spelling (see config.FindProject). An aft.toml here can pin
	// its host and server directory for every machine.
	root := config.RealPath(cwd)
	if repo, repoRoot, ok := config.LoadRepo(cwd); ok && repoRoot == cwd {
		if *hostName == "" {
			*hostName = repo.Host
		}
		if *dir == "" {
			*dir = repo.Dir
		}
	}
	// A project on a network mount already lives on the server; map it in place
	// with no clone and no sync. This runs unless the user forced --dir.
	mounted := false
	if *dir == "" {
		if mh, mr, ok := resolveLocalDir(&cfg, root); ok {
			if *hostName == "" {
				*hostName = mh
			}
			*dir = mr
			mounted = true
		}
	}
	h, err := cfg.Host(*hostName)
	if err != nil {
		return fail(err)
	}
	if *dir == "" {
		*dir = config.DefaultRemoteDir(root)
	}
	if err := connect(h); err != nil {
		return fail(err)
	}
	if _, err := ensureServer(h); err != nil {
		return fail(err)
	}
	if !mounted {
		// Resolve a home-relative mapping to an absolute server path, since the
		// Mac and the server have different home directories and the session uses
		// the path as a real working directory, not a shell word.
		resolved, err := resolveRemoteDir(h, *dir)
		if err != nil {
			return fail(err)
		}
		*dir = resolved
	}
	exists, err := remoteDirExists(h, *dir)
	if err != nil {
		return fail(err)
	}
	if !exists {
		if mounted {
			return fail(fmt.Errorf("the mount maps to %s on %s, but that path is missing; check aft mount list", *dir, h.Name))
		}
		if !named {
			ui.Error("%s does not exist on %s", *dir, h.Name)
			ui.Hint("aft on --dir %s creates it, or name the folder this project lives in there", *dir)
			return 1
		}
		if _, err := transport.RunRaw(h, nil, "mkdir -p "+transport.Quote(*dir)); err != nil {
			return fail(fmt.Errorf("could not create %s on %s: %w", *dir, h.Name, err))
		}
		ui.OK("Created %s on %s", *dir, h.Name)
	}
	st.Projects[root] = config.Project{Host: h.Name, Dir: *dir}
	if err := config.SaveState(st); err != nil {
		return fail(err)
	}
	p := config.Project{Host: h.Name, Dir: *dir}
	ui.OK("On  %s %s %s", ui.Path(root), ui.Dim("→"), where(p))
	if h.IsLocal() {
		fmt.Println(ui.Dim("Agents started here now run in sessions on this machine."))
	} else {
		fmt.Println(ui.Dim("Agents started here now run on " + h.Name + "."))
	}
	if !launcher.OnPath() {
		ui.Warn("aft's launchers are not first on PATH in this shell")
		ui.Hint("open a new terminal, or run aft doctor")
	}
	return 0
}

// cmdOff removes the mapping for the current directory's project.
func cmdOff(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	st, err := config.LoadState()
	if err != nil {
		return fail(err)
	}
	root, _, ok := st.FindProject(cwd)
	if !ok {
		fmt.Println(ui.Dim("Already off here."))
		return 0
	}
	delete(st.Projects, root)
	if err := config.SaveState(st); err != nil {
		return fail(err)
	}
	ui.OK("Off  %s %s", ui.Path(root), ui.Dim("· agents started here run locally"))
	return 0
}

// cmdHostShell is aft HOST: a login shell on h in a session, or, given a
// command, that command run there once, as ssh HOST CMD does. The shell is
// like ssh but survives disconnects, roams over UDP and predicts typing. It
// starts in the project's directory when this project is on for h, and comes
// back to the shell already open there.
func cmdHostShell(cfg config.Config, h transport.Host, args []string) int {
	st, _ := config.LoadState()
	cwd, _ := os.Getwd()
	root, p, on := st.FindProject(cwd)
	on = on && p.Host == h.Name
	dir := ""
	if on {
		dir = config.RemoteDir(root, p, cwd)
	}
	if len(args) > 0 {
		if h.IsLocal() && dir == "" {
			dir = cwd
		}
		return launcher.RunTool(h, dir, nil, args)
	}
	if !on {
		root, p = "", config.Project{}
	}
	opts, err := launcher.SessionOptions(cfg, h.Name, root, p)
	if err != nil {
		return fail(err)
	}
	opts.Host = h
	opts.Command = session.ShellCommand
	opts.Dir = dir
	if dir == "" && h.IsLocal() {
		opts.Dir, _ = os.UserHomeDir()
	} else if dir == "" {
		if err := connect(h); err != nil {
			return fail(err)
		}
		if opts.Dir, err = resolveRemoteDir(h, "~"); err != nil {
			return fail(err)
		}
	}
	// Come back to the shell already open in this directory, if there is one.
	isShell := func(r session.Record) bool { return session.IsShell(r.Command) }
	if id := launcher.ExistingSession(h, opts.Dir, isShell); id != "" {
		ui.Note("reattaching to %s %s", id, ui.Dim("· aft kill "+id+" starts a fresh one"))
		opts.SessionID = id
	}
	return terminal.Run(opts)
}

// hostName is what a host may be called: a word aft HOST can open, so never a
// command, a name aft points to a command, or the hidden server command.
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// checkHostName says why name cannot name a host, or nil.
func checkHostName(name string) error {
	if !hostName.MatchString(name) {
		return fmt.Errorf("a host name is letters, digits, dots and dashes: %q", name)
	}
	if isCommand(name) || meant[name] != "" {
		return fmt.Errorf("%q is an aft command, so aft %s could not open it; pick another name", name, name)
	}
	return nil
}

// nameFor picks a host's name from its SSH destination, which may start with
// ssh options: the first label of the machine name (box for
// "-p 2222 fschr@box.example.com"), an IP address with its dots dashed, or the
// machine name with its punctuation dashed when that is not a usable name.
func nameFor(dest string) string {
	f := strings.Fields(dest)
	if len(f) == 0 {
		return ""
	}
	machine := f[len(f)-1]
	if _, after, ok := strings.Cut(machine, "@"); ok {
		machine = after
	}
	dashed := strings.NewReplacer(".", "-", ":", "-").Replace(machine)
	if strings.Trim(machine, "0123456789.:") == "" {
		return dashed // an address: every part of it matters
	}
	if first, _, _ := strings.Cut(machine, "."); checkHostName(first) == nil {
		return first
	}
	return dashed
}

// adoptHost makes dest a host the first time someone types aft DEST: it checks
// SSH can reach it, saves it under a short name, and installs the server.
// probe skips the check when dest plainly is a destination (user@host).
func adoptHost(cfg *config.Config, dest string, probe bool) (transport.Host, bool) {
	if probe {
		reached := ui.Spin("Looking for a host called "+dest, func() error {
			return exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", dest, "true").Run()
		}) == nil
		if !reached {
			return transport.Host{}, false
		}
	}
	name := nameFor(dest)
	if _, taken := cfg.Hosts[name]; taken || checkHostName(name) != nil {
		ui.Error("could not pick a name for %s", dest)
		ui.Hint("aft host add NAME %s", dest)
		return transport.Host{}, false
	}
	keepDefault(cfg)
	cfg.Hosts[name] = config.HostConfig{SSH: dest}
	if err := config.Save(*cfg); err != nil {
		fail(err)
		return transport.Host{}, false
	}
	ui.Note("added %s as host %s; aft %s opens it from now on", dest, ui.Bold(name), name)
	h, _ := cfg.Host(name)
	if err := connect(h); err != nil {
		fail(err)
		return transport.Host{}, false
	}
	if _, err := ensureServer(h); err != nil {
		fail(err)
		return transport.Host{}, false
	}
	return h, true
}

type hostSessions struct {
	host transport.Host
	list []session.Record
	err  error
}

// allSessions asks every host for its sessions at once, connecting without a
// terminal where no connection is open, behind a spinner.
func allSessions(cfg config.Config) []hostSessions {
	var out []hostSessions
	list, _ := session.List()
	if list = append(list, session.Remains()...); len(list) > 0 {
		out = append(out, hostSessions{host: transport.Local, list: list})
	}
	names := cfg.HostNames()
	if len(names) == 0 {
		return out
	}
	remote := make([]hostSessions, len(names))
	label := "Asking " + strings.Join(names, ", ") + " for sessions"
	ui.Spin(label, func() error {
		var wg sync.WaitGroup
		for i, n := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h, err := cfg.Host(n)
				hs := hostSessions{host: h, err: err}
				if err == nil {
					hs.err = transport.EnsureMasterWithin(h, 4) // an offline host must not hold up the list
				}
				if hs.err == nil {
					raw, err := transport.Output(h, nil, "ls", "--all")
					if hs.err = err; err == nil {
						json.Unmarshal(raw, &hs.list)
					}
				}
				remote[i] = hs
			}()
		}
		wg.Wait()
		return nil
	})
	return append(out, remote...)
}

// where names the host and directory a project runs in.
func where(p config.Project) string {
	if p.Host == "local" {
		return "this machine" + ui.Dim(" in "+ui.Path(p.Dir))
	}
	return p.Host + ui.Dim(":"+p.Dir)
}

// describe says what a session is: the program, and what it says it is doing
// (its window title), as in "Claude · Fix the login bug".
func describe(r session.Record) string {
	name := "session"
	if len(r.Command) > 0 {
		name = filepath.Base(r.Command[0])
	}
	if session.IsShell(r.Command) {
		name = "shell" // aft HOST's login-shell wrapper
	}
	for _, a := range config.DefaultAgents {
		if name == a {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
	}
	// Before it has a topic, an agent's title is often just its own name.
	if r.Title == "" || strings.EqualFold(r.Title, name) || strings.EqualFold(r.Title, name+" Code") {
		return name
	}
	return name + ui.Dim(" · ") + r.Title
}

// state says whether a terminal is showing the session, or it runs on with none.
func state(r session.Record) string {
	switch e := r.Ended; {
	case e != nil && e.Lost:
		return ui.Mark("fix") + " lost"
	case e != nil && e.Signal != "":
		return ui.Mark("warn") + " killed by " + e.Signal
	case e != nil && e.Code != 0:
		return ui.Mark("warn") + fmt.Sprintf(" exited %d", e.Code)
	case e != nil:
		return ui.Dim("exited")
	}
	if r.Attached {
		return ui.Accent("attached")
	}
	return ui.Dim("detached")
}

// started renders a record's start time as how long ago it was.
func started(r session.Record) string {
	t, err := time.Parse(time.RFC3339, r.Started)
	if err != nil {
		return r.Started
	}
	return ui.Ago(t)
}

// cmdLs lists sessions on every host.
func cmdLs(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	var rows [][]string
	var failed []hostSessions
	last := ""
	allSeen := allSessions(cfg)
	for _, hs := range allSeen {
		if hs.err != nil {
			failed = append(failed, hs)
			continue
		}
		for _, r := range hs.list {
			last = r.ID
			rows = append(rows, []string{ui.Bold(r.ID) + " (" + describe(r) + ")", state(r), hs.host.Name, ui.Path(r.Dir), ui.Dim(started(r))})
		}
	}
	if len(rows) == 0 {
		fmt.Println(ui.Dim("No sessions."))
	} else {
		ui.Table([]string{"SESSION", "STATE", "HOST", "DIRECTORY", "STARTED"}, rows)
		ended := false
		for _, hs := range allSeen {
			for _, r := range hs.list {
				ended = ended || r.Ended != nil
			}
		}
		switch {
		case ended:
			ui.Hint("aft attach NAME reattaches, or shows how an ended session finished")
		case len(rows) == 1:
			ui.Hint("aft attach %s", last)
		default:
			ui.Hint("aft attach NAME")
		}
	}
	for _, hs := range failed {
		var ce *transport.ConnectError
		if errors.As(hs.err, &ce) {
			ui.Warn("%v", hs.err) // names the host already
		} else {
			ui.Warn("%s: %v", hs.host.Name, hs.err)
		}
		ui.Hint("aft doctor --host %s", hs.host.Name)
	}
	return 0
}

// findSession locates a session id across hosts.
func findSession(cfg config.Config, id string) (transport.Host, session.Record, error) {
	for _, hs := range allSessions(cfg) {
		for _, r := range hs.list {
			if r.ID == id {
				return hs.host, r, nil
			}
		}
	}
	return transport.Host{}, session.Record{}, fmt.Errorf("no session called %q on your hosts", id)
}

// showRemains prints what a session that ended unseen left, its last screen
// and how it ended, then forgets it: attaching to it is how you find out.
func showRemains(h transport.Host, r session.Record) int {
	raw, err := transport.Output(h, nil, "remains", r.ID)
	if err != nil {
		return fail(err)
	}
	var got struct {
		session.Record
		Screen string `json:"screen"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return fail(err)
	}
	if got.Screen != "" {
		fmt.Println(ui.Dim("── the last screen of " + r.ID + " ──"))
		fmt.Print(got.Screen)
		fmt.Println(ui.Dim("──"))
	}
	e := got.Ended
	how := "exited"
	switch {
	case e == nil:
	case e.Lost:
		how = "was lost when its server stopped"
	case e.Signal != "":
		how = "was ended by signal " + e.Signal
	case e.Code != 0:
		how = fmt.Sprintf("exited with code %d", e.Code)
	}
	when := ""
	if e != nil {
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			when = " " + ui.Ago(t)
		}
	}
	ui.Warn("%s (%s) on %s %s%s, with no terminal attached", r.ID, ansi.Strip(describe(r)), h.Name, how, when)
	if len(r.Command) > 0 {
		ui.Hint("run %s again in %s to start over; many agents can resume their last conversation", filepath.Base(r.Command[0]), ui.Path(r.Dir))
	}
	return 0
}

// cmdAttach reattaches to a session.
func cmdAttach(args []string) int {
	fs := flags("attach")
	hostName := fs.String("host", "", "host the session is on")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	id := fs.Arg(0)
	var h transport.Host
	var rec session.Record
	if id == "" {
		all := allSessions(cfg)
		var found []struct {
			h transport.Host
			r session.Record
		}
		for _, hs := range all {
			for _, r := range hs.list {
				found = append(found, struct {
					h transport.Host
					r session.Record
				}{hs.host, r})
			}
		}
		if len(found) == 0 {
			return fail(errors.New("no sessions to attach to"))
		}
		if len(found) > 1 {
			opts := make([]ui.Option, len(found))
			for i, f := range found {
				opts[i] = ui.Option{Label: f.r.ID + " (" + ansi.Strip(describe(f.r)) + ")", Hint: ansi.Strip(state(f.r)) + " · " + f.h.Name + " · " + started(f.r)}
			}
			if !ui.Interactive() {
				ui.Error("several sessions; pass an id")
				for _, o := range opts {
					fmt.Fprintf(os.Stderr, "  %s  %s\n", o.Label, o.Hint)
				}
				return 2
			}
			i := ui.Choose("Attach to which session?", opts, 0)
			found = found[i : i+1]
		}
		h, id, rec = found[0].h, found[0].r.ID, found[0].r
	} else if *hostName != "" {
		h, err = cfg.Host(*hostName)
		if err != nil {
			return fail(err)
		}
		// Look the session up there too, so an ended one shows what it left.
		if raw, err := transport.Output(h, nil, "ls", "--all"); err == nil {
			var list []session.Record
			json.Unmarshal(raw, &list)
			for _, r := range list {
				if r.ID == id {
					rec = r
				}
			}
		}
	} else {
		h, rec, err = findSession(cfg, id)
		if err != nil {
			ui.Error("%v", err)
			ui.Hint("aft ls lists them")
			return 1
		}
	}
	if rec.Ended != nil {
		return showRemains(h, rec)
	}
	opts, err := launcher.SessionOptions(cfg, h.Name, "", config.Project{})
	if err != nil {
		return fail(err)
	}
	opts.Host = h
	opts.SessionID = id
	return terminal.Run(opts)
}

// cmdKill stops a session: the application and its pseudo-terminal.
func cmdKill(args []string) int {
	fs := flags("kill")
	hostName := fs.String("host", "", "host the session is on")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: aft kill [--host H] NAME")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	id := fs.Arg(0)
	var h transport.Host
	var rec session.Record
	if *hostName != "" {
		h, err = cfg.Host(*hostName)
	} else {
		h, rec, err = findSession(cfg, id)
	}
	if err != nil {
		return fail(err)
	}
	if _, err := transport.Output(h, nil, "end", id); err != nil {
		return fail(err)
	}
	if rec.Ended != nil {
		ui.OK("Forgot %s %s", ui.Bold(id), ui.Dim("· it had already ended"))
	} else {
		ui.OK("Killed %s", ui.Bold(id))
	}
	return 0
}

// cmdStatus summarizes where you are, hosts, sessions, mirrored ports and
// projects.
func cmdStatus(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	st, _ := config.LoadState()
	cwd, _ := os.Getwd()
	all := allSessions(cfg)
	row := func(label string, lines ...string) {
		for i, l := range lines {
			if i > 0 {
				label = ""
			}
			fmt.Printf("%s %s\n", ui.Bold(fmt.Sprintf("%-9s", label)), l)
		}
	}
	mapping := func(root string, p config.Project) string { return ui.Path(root) + ui.Dim(" → ") + where(p) }
	if root, p, ok := st.FindProject(cwd); ok {
		row("Here", ui.Mark("ok")+" on  "+mapping(root, p))
	} else {
		row("Here", ui.Mark("info")+" off "+ui.Dim("· aft on makes agents here run on a host"))
	}
	var hosts, sessions []string
	for _, hs := range all {
		for _, r := range hs.list {
			sessions = append(sessions, ui.Bold(r.ID)+" ("+describe(r)+")  "+state(r)+ui.Dim("  on "+hs.host.Name+" · "+started(r)))
		}
		if hs.host.IsLocal() {
			continue
		}
		line := ui.Mark("ok") + " " + hs.host.Name + ui.Dim("  connected")
		if hs.err != nil {
			line = ui.Mark("fix") + " " + hs.host.Name + ui.Dim("  unreachable: "+hs.err.Error())
		}
		if hs.host.Name == cfg.UsedHost() {
			line += "  " + ui.Accent("default")
		}
		hosts = append(hosts, line)
	}
	if len(hosts) > 0 {
		row("Hosts", hosts...)
	}
	if len(sessions) == 0 {
		sessions = []string{ui.Dim("none")}
	}
	row("Sessions", sessions...)
	if m := mirrors(); len(m) > 0 {
		var lines []string
		for id, ports := range m {
			list := make([]string, len(ports))
			for i, p := range ports {
				list[i] = fmt.Sprintf("localhost:%d", p)
			}
			lines = append(lines, strings.Join(list, "  ")+ui.Dim("  from "+id))
		}
		sort.Strings(lines)
		row("Ports", lines...)
	}
	if len(st.Projects) > 0 {
		roots := make([]string, 0, len(st.Projects))
		for r := range st.Projects {
			roots = append(roots, r)
		}
		sort.Strings(roots)
		var lines []string
		for _, r := range roots {
			lines = append(lines, mapping(r, st.Projects[r]))
		}
		row("Projects", lines...)
	}
	return 0
}

// docFinding is one line of a doctor report: a check, its level, and if it is
// not ok, the exact command that fixes it. Fixed is set when --fix repaired it.
type docFinding struct {
	Section string `json:"section"`
	Level   string `json:"level"` // ok | info | warn | fix
	Text    string `json:"text"`
	Hint    string `json:"hint,omitempty"`
	Fixed   bool   `json:"fixed,omitempty"`
}

// doctor accumulates findings under the current section and, when fixing, runs
// each repair as its check is added. With live set it prints each finding as
// it lands, above a spinner that says what is being checked.
type doctor struct {
	fix      bool
	cur      string
	shown    string
	live     *ui.Live
	findings []docFinding
}

func (d *doctor) section(s string) { d.cur = s }

// step says what the doctor is checking now.
func (d *doctor) step(s string) {
	if d.live != nil {
		d.live.Step(s)
	}
}

func (d *doctor) add(f docFinding) {
	d.findings = append(d.findings, f)
	if d.live == nil {
		return
	}
	if f.Level != "info" {
		d.live.Advance()
	}
	if f.Section != d.shown {
		d.live.Println("\n" + ui.Bold(f.Section))
		d.shown = f.Section
	}
	text := f.Text
	if f.Level == "info" {
		text = ui.Dim(text)
	}
	d.live.Println("  " + ui.Mark(f.Level) + " " + text)
	if f.Hint != "" {
		d.live.Println("    " + ui.Dim("→ "+f.Hint))
	}
}

func (d *doctor) ok(text string)         { d.add(docFinding{d.cur, "ok", text, "", false}) }
func (d *doctor) info(text string)       { d.add(docFinding{d.cur, "info", text, "", false}) }
func (d *doctor) warn(text, hint string) { d.add(docFinding{d.cur, "warn", text, hint, false}) }
func (d *doctor) bad(text, hint string)  { d.add(docFinding{d.cur, "fix", text, hint, false}) }

// repair records a fixable finding. Under --fix it runs fn and, on success,
// turns the finding into an ok line describing what it did.
func (d *doctor) repair(text, hint string, fn func() error, done string) {
	f := docFinding{d.cur, "fix", text, hint, false}
	if d.fix {
		d.step("Fixing: " + text)
		if err := fn(); err == nil {
			f.Level, f.Text, f.Hint, f.Fixed = "ok", text+" — "+done, "", true
		} else {
			f.Hint = err.Error()
		}
	}
	d.add(f)
}

// cmdDoctor inspects everything Aft depends on, grouped by where a problem would
// be, and prints the exact command to fix each one. --fix applies the safe
// repairs (reinstall a stale or missing server, re-add the PATH line); --json
// prints machine-readable findings; --host limits host checks to one host.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	doFix := fs.Bool("fix", false, "apply the safe repairs")
	asJSON := fs.Bool("json", false, "print findings as JSON")
	only := fs.String("host", "", "check only this host")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	st, _ := config.LoadState()
	self, _ := os.Executable()
	d := &doctor{fix: *doFix}
	var hosts []string
	for _, n := range cfg.HostNames() {
		if *only == "" || n == *only {
			hosts = append(hosts, n)
		}
	}
	start := time.Now()
	if !*asJSON {
		// Three checks here, three per host.
		d.live = ui.Start("Checking this machine", 3+3*len(hosts))
	}

	d.section("This machine")
	d.info("aft " + version + " · " + ui.Path(home.Dir()) + " · " + ui.Path(home.Data()))
	if _, err := exec.LookPath("ssh"); err == nil {
		d.ok("ssh")
	} else {
		d.bad("ssh missing", "install OpenSSH")
	}
	switch {
	case launcher.OnPath():
		d.ok("launchers on PATH")
	case cfg.EditingDotfiles():
		d.repair("launchers not on PATH", "run aft setup", func() error {
			if _, err := launcher.InstallShellBlock(&st); err != nil {
				return err
			}
			return config.SaveState(st)
		}, "added the PATH line; open a new terminal")
	default:
		d.bad("launchers not on PATH (manual mode)", "add "+launcher.PathDir+" to your PATH")
	}
	var missing, hostOnly []string
	for _, a := range cfg.Launched() {
		if !launcher.Installed(a) {
			missing = append(missing, a)
		} else if _, err := launcher.RealBinary(a); err != nil {
			hostOnly = append(hostOnly, a)
		}
	}
	if len(missing) > 0 {
		d.repair("launchers missing: "+strings.Join(missing, ", "), "run aft setup", func() error {
			return launcher.InstallLaunchers(self, cfg.Launched(), &st)
		}, "reinstalled launchers")
	} else {
		d.ok(fmt.Sprintf("%d launchers", len(cfg.Launched())))
	}
	if len(hostOnly) > 0 {
		d.info("no local copy, so these run only on a host: " + strings.Join(hostOnly, ", "))
	}

	d.section("Here")
	cwd, _ := os.Getwd()
	if root, p, ok := st.FindProject(cwd); ok {
		d.info("on · " + ui.Path(root) + " → " + ansi.Strip(where(p)))
	} else {
		d.info("off · aft on makes agents here run on a host")
	}

	// A machine whose projects all run on itself needs no host.
	runsHere := len(st.Projects) > 0
	for _, p := range st.Projects {
		runsHere = runsHere && p.Host == "local"
	}
	if len(cfg.Hosts) == 0 && !runsHere {
		d.section("Hosts")
		d.bad("no host configured", "run aft setup")
	}
	for _, n := range hosts {
		title := "Host " + n
		if n == cfg.UsedHost() {
			title += " (default)"
		}
		d.section(title)
		h, _ := cfg.Host(n)
		d.step("Connecting to " + n)
		err := transport.EnsureMasterBatch(h)
		if transport.NeedsTerminal(err) && d.live != nil {
			// SSH may need the terminal for a passphrase or a new host key.
			d.live.Pause()
			err = transport.EnsureMaster(h)
			d.live.Resume()
		}
		if err != nil {
			d.bad("cannot connect ("+h.SSH+")", ansi.Strip(err.Error())+"; is it on and online?")
			if d.live != nil {
				d.live.Advance()
				d.live.Advance()
			}
			continue
		}
		d.ok("connected " + ui.Dim("· "+h.SSH))
		d.step("Checking the server on " + n)
		info, err := install.Info(h)
		switch {
		case err != nil || info.Version == "":
			d.repair("server not installed", "run aft setup", func() error { _, e := install.Ensure(h); return e }, "installed the server")
		case !info.Shims:
			d.repair("bridge shims missing", "run aft setup", func() error { _, e := install.Ensure(h); return e }, "reinstalled the server")
		default:
			if ok, e := install.UpToDate(info); e == nil && !ok {
				d.repair("server out of date ("+info.Version+")", "run aft doctor --fix", func() error { _, e := install.Ensure(h); return e }, "updated the server")
			} else {
				d.ok("server ready " + ui.Dim("· "+info.OS+"/"+info.Arch+" · "+info.Version))
			}
		}
		if found := agentsIn(info.Agents); len(found) > 0 {
			d.ok("agents " + ui.Dim("· "+strings.Join(found, ", ")))
		} else {
			d.warn("no agents on the host", "install one there (claude, codex, ...)")
		}
	}

	if d.live != nil {
		d.live.Stop()
	}
	if *asJSON {
		for i := range d.findings {
			d.findings[i].Text = ansi.Strip(d.findings[i].Text)
		}
		b, _ := json.MarshalIndent(d.findings, "", "  ")
		fmt.Println(string(b))
	} else {
		d.summary(time.Since(start))
	}
	for _, f := range d.findings {
		if f.Level == "fix" {
			return 1
		}
	}
	return 0
}

// summary closes the report with one line: all good, or what is left to do.
func (d *doctor) summary(took time.Duration) {
	fixes, warns := 0, 0
	for _, f := range d.findings {
		switch f.Level {
		case "fix":
			fixes++
		case "warn":
			warns++
		}
	}
	fmt.Println()
	t := ui.Dim(fmt.Sprintf(" · %.1fs", took.Seconds()))
	switch {
	case fixes > 0 && d.fix:
		fmt.Println(ui.Mark("fix") + fmt.Sprintf(" %d still to fix; they could not be repaired automatically", fixes) + t)
	case fixes > 0:
		fmt.Println(ui.Mark("fix") + fmt.Sprintf(" %d to fix, %d recommended", fixes, warns) + t)
		ui.Hint("aft doctor --fix applies the safe repairs")
	case warns > 0:
		fmt.Println(ui.Mark("warn") + fmt.Sprintf(" All good, %d recommended above", warns) + t)
	default:
		fmt.Println(ui.Mark("ok") + " Everything looks good" + t)
	}
}

// mirrors reads the ports each running client mirrors, keyed by session.
func mirrors() map[string][]int {
	out := map[string][]int{}
	entries, _ := os.ReadDir(home.Sockets())
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "mirrors-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(home.Sockets(), e.Name()))
		var ports []int
		if err == nil && json.Unmarshal(b, &ports) == nil && len(ports) > 0 {
			out[strings.TrimSuffix(strings.TrimPrefix(e.Name(), "mirrors-"), ".json")] = ports
		}
	}
	return out
}

// cmdLaunchers adds launchers: kind "agents" run in sessions on the host,
// kind "tools" (git, gh) run there once. Either runs locally where aft is off.
func cmdLaunchers(kind string, args []string) int {
	if len(args) < 2 || args[0] != "add" {
		fmt.Fprintf(os.Stderr, "usage: aft %s add NAME...\n", kind)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	st, err := config.LoadState()
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	self, _ = filepath.EvalSymlinks(self)
	have := map[string]bool{}
	for _, n := range cfg.Launched() {
		have[n] = true
	}
	var added []string
	for _, n := range args[1:] {
		if have[n] {
			fmt.Println(ui.Dim(n + " already has a launcher."))
			continue
		}
		have[n] = true
		added = append(added, n)
		if kind == "tools" {
			cfg.Tools = append(cfg.Tools, n)
		} else {
			cfg.Agents = append(cfg.Agents, n)
		}
	}
	if len(added) == 0 {
		return 0
	}
	if err := launcher.InstallLaunchers(self, added, &st); err != nil {
		return fail(err)
	}
	if err := config.Save(cfg); err != nil {
		return fail(err)
	}
	if err := config.SaveState(st); err != nil {
		return fail(err)
	}
	if kind == "tools" {
		ui.OK("%s now run on the server wherever aft is on %s", strings.Join(added, ", "), ui.Dim("· once per command, no session"))
	} else {
		ui.OK("%s now run in sessions on the server wherever aft is on", strings.Join(added, ", "))
	}
	return 0
}

// cmdUninstall removes everything Aft added on every machine.
func cmdUninstall(args []string) int {
	fs := flags("uninstall")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	st, err := config.LoadState()
	if err != nil {
		return fail(err)
	}
	fmt.Println(ui.Bold("This will:"))
	for _, n := range cfg.HostNames() {
		fmt.Printf("  %s end Aft sessions and remove ~/.aft, ~/.local/share/aft and pasted files on %s\n", ui.Dim("•"), ui.Bold(n))
	}
	for _, b := range st.ShellBlocks {
		fmt.Printf("  %s remove the aft block from %s\n", ui.Dim("•"), ui.Path(b.File))
	}
	fmt.Printf("  %s remove %s %s and %s %s\n", ui.Dim("•"), ui.Path(home.Dir()), ui.Dim("(config, state, logs)"), ui.Path(home.Data()), ui.Dim("(launchers, sockets)"))
	fmt.Println(ui.Dim("Your code, SSH config and each agent's own data stay."))
	if !*yes && !ui.Confirm("Continue?", false) {
		return 1
	}
	code := 0
	for _, n := range cfg.HostNames() {
		h, _ := cfg.Host(n)
		if err := connect(h); err != nil {
			ui.Error("%v", err)
			ui.Hint("remove ~/.aft and ~/.local/share/aft on %s by hand", n)
			code = 1
			continue
		}
		if err := ui.Spin("Removing Aft from "+n, func() error { _, err := transport.Output(h, nil, "uninstall"); return err }); err != nil {
			ui.Error("%s: %v", n, err)
			code = 1
		} else {
			ui.OK("Removed Aft from %s", ui.Bold(n))
		}
		transport.CloseMaster(h)
	}
	if list, _ := session.List(); len(list) > 0 {
		for _, r := range list {
			session.Control(r.ID, proto.End)
		}
	}
	removed, kept := launcher.RemoveShellBlocks(&st)
	for _, f := range removed {
		ui.OK("Removed the aft block from %s", ui.Path(f))
	}
	for _, f := range kept {
		ui.Warn("left %s alone: its aft block was edited", ui.Path(f))
		ui.Hint("remove the lines between %q and %q yourself", "# >>> aft >>>", "# <<< aft <<<")
		code = 1
	}
	launcher.RemoveLaunchers(&st)
	for _, d := range []string{home.Dir(), home.Data()} {
		if err := os.RemoveAll(d); err != nil {
			return fail(err)
		}
		ui.OK("Removed %s", ui.Path(d))
	}
	os.RemoveAll(home.Temp()) // pasted files, if the local host received any
	if self, err := os.Executable(); err == nil {
		fmt.Println(ui.Dim("The aft binary itself is at " + ui.Path(self) + "; delete it however you installed it."))
	}
	return code
}

// cmdCompletion prints shell completion for the top-level commands.
func cmdCompletion(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: aft completion bash|zsh|fish")
		return 2
	}
	words := "setup on off ls attach kill host mount status doctor agents tools uninstall completion version help"
	switch args[0] {
	case "bash":
		fmt.Printf("_aft() { COMPREPLY=( $(compgen -W '%s' -- \"${COMP_WORDS[COMP_CWORD]}\") ); }\ncomplete -F _aft aft\n", words)
	case "zsh":
		fmt.Printf("#compdef aft\n_aft() { compadd -- %s; }\ncompdef _aft aft\n", words)
	case "fish":
		for _, w := range strings.Fields(words) {
			fmt.Printf("complete -c aft -n '__fish_use_subcommand' -a %s\n", w)
		}
	default:
		fmt.Fprintln(os.Stderr, "supported shells: bash, zsh, fish")
		return 2
	}
	return 0
}

func remoteDirExists(h transport.Host, dir string) (bool, error) {
	out, err := transport.RunRaw(h, nil, "test -d "+transport.Quote(dir)+" && echo yes || echo no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// resolveRemoteDir expands a leading ~ in a server path to the server's real
// home directory, so the stored path is absolute. The Mac and the server have
// different home directories, and the session uses the path as a real
// working directory, not a shell word, so a literal ~ would never expand.
func resolveRemoteDir(h transport.Host, dir string) (string, error) {
	if dir != "~" && !strings.HasPrefix(dir, "~/") {
		return dir, nil
	}
	out, err := transport.RunRaw(h, nil, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(string(out))
	if home == "" || !strings.HasPrefix(home, "/") {
		return "", fmt.Errorf("could not resolve the home directory on %s", h.Name)
	}
	return expandHome(dir, home), nil
}

// expandHome replaces a leading ~ or ~/ in dir with home.
func expandHome(dir, home string) string {
	if dir == "~" {
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		return home + dir[1:]
	}
	return dir
}
