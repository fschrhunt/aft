// Package config reads and writes Aft's settings and state on the user's
// machine: the TOML config, the record of which projects are on, and the
// optional aft.toml a repository may commit.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/arocomputer/aft/internal/bridge/transport"
	"github.com/arocomputer/aft/internal/home"
)

// DefaultAgents are the launchers setup creates when they are installed.
// Agents run in sessions; tools, which people add themselves, run once.
var DefaultAgents = []string{"claude", "codex", "opencode", "pi", "gemini", "amp", "aider", "goose"}

// Config is ~/.aft/config.toml. Top-level keys set defaults; a [host.<name>]
// table or a repository's aft.toml can override most of them. Keys are flat and
// snake_case; a host is addressed by name, so its table is singular: host.mini.
type Config struct {
	Use          string                `toml:"use,omitempty"` // the host used when none is given; a lone host needs no line
	Agents       []string              `toml:"agents"`
	Tools        []string              `toml:"tools,omitempty"` // commands like git and gh that run once on the host, not in a session
	Predict      *bool                 `toml:"predict,omitempty"`
	UDP          *bool                 `toml:"udp,omitempty"`           // reach sessions over UDP when the host allows it; nil means yes
	Prefix       *string               `toml:"prefix,omitempty"`        // key for copy mode and detach; nil means the default ctrl-]
	DetachKey    string                `toml:"detach,omitempty"`        // key after the prefix that detaches
	EditDotfiles *bool                 `toml:"edit_dotfiles,omitempty"` // let setup edit the shell config; nil means yes
	Wrapper      []string              `toml:"wrapper,omitempty"`       // default command wrapper on the host
	Env          []string              `toml:"env,omitempty"`           // extra variables to pass to the host
	Ports        string                `toml:"ports,omitempty"`         // default mirror range, e.g. "3000-3999"
	Hosts        map[string]HostConfig `toml:"host"`
	Mounts       []Mount               `toml:"mount"`
}

// HostConfig describes one host and overrides the top-level settings for it.
type HostConfig struct {
	SSH     string   `toml:"ssh"`               // destination, may carry ssh options
	Wrapper []string `toml:"wrapper,omitempty"` // command wrapper on the host
	Predict *bool    `toml:"predict,omitempty"` // prediction for sessions on this host
	UDP     *bool    `toml:"udp,omitempty"`     // UDP for sessions on this host
	Ports   string   `toml:"ports,omitempty"`   // mirror range for this host
	Env     []string `toml:"env,omitempty"`     // extra variables to pass to this host
	Alive   int      `toml:"alive,omitempty"`   // ServerAlive interval seconds, 0 = default
	Persist int      `toml:"persist,omitempty"` // ControlPersist seconds, 0 = default
}

// Mount maps a locally mounted tree to the host directory it comes from, for
// network mounts such as SMB or NFS where the files are the host's and the
// local machine only views them. A project under Local needs no clone and no
// sync; aft just runs commands against the real path on the host.
type Mount struct {
	Local string `toml:"local"` // the local mountpoint, e.g. /Volumes/Room
	Host  string `toml:"host"`  // the host the mount comes from
	Path  string `toml:"path"`  // the host path the mount exposes, e.g. /home/fschr/room
}

// Project is one directory that is on.
type Project struct {
	Host string `json:"host"`
	Dir  string `json:"dir"`
}

// ShellBlock records a block Aft added to a shell config file.
type ShellBlock struct {
	File  string `json:"file"`
	Block string `json:"block"`
}

// State is ~/.aft/state.json: everything Aft changed that uninstall must undo.
type State struct {
	Projects    map[string]Project `json:"projects"`
	ShellBlocks []ShellBlock       `json:"shell_blocks"`
	Shims       []string           `json:"shims"`
	Binary      string             `json:"binary"`
}

// RepoConfig is an optional aft.toml at a repository root. It pins a project to
// a host so every machine that clones the repo runs it on the same host.
type RepoConfig struct {
	Host    string   `toml:"host"`
	Dir     string   `toml:"dir"`
	Ports   string   `toml:"ports"`
	Wrapper []string `toml:"wrapper"` // command wrapper for this project
	Env     []string `toml:"env"`     // extra variables for this project
}

// Settings are the effective per-session settings after merging global, host
// and project configuration. Project overrides host overrides global.
type Settings struct {
	Wrapper []string
	Predict bool
	UDP     bool
	Ports   string
	Env     []string
	Alive   int
	Persist int
}

// Resolve merges settings for a host and an optional repository config.
func (c Config) Resolve(hostName string, repo *RepoConfig) Settings {
	h := c.Hosts[hostName]
	s := Settings{
		Wrapper: c.Wrapper,
		Predict: c.PredictOn(),
		UDP:     c.UDP == nil || *c.UDP,
		Ports:   c.Ports,
		Env:     append([]string(nil), c.Env...),
		Alive:   h.Alive,
		Persist: h.Persist,
	}
	if len(h.Wrapper) > 0 {
		s.Wrapper = h.Wrapper
	}
	if h.Predict != nil {
		s.Predict = *h.Predict
	}
	if h.UDP != nil {
		s.UDP = *h.UDP
	}
	if h.Ports != "" {
		s.Ports = h.Ports
	}
	s.Env = append(s.Env, h.Env...)
	if repo != nil {
		if len(repo.Wrapper) > 0 {
			s.Wrapper = repo.Wrapper
		}
		if repo.Ports != "" {
			s.Ports = repo.Ports
		}
		s.Env = append(s.Env, repo.Env...)
	}
	return s
}

// EditingDotfiles reports whether setup should edit shell config files itself,
// the default. When off it prints the line instead, for people who manage
// their dotfiles.
func (c Config) EditingDotfiles() bool { return c.EditDotfiles == nil || *c.EditDotfiles }

// Path is the config file.
// Launched is every name with a launcher: the agents, then the tools.
func (c Config) Launched() []string { return append(append([]string{}, c.Agents...), c.Tools...) }

// IsTool reports whether name is a tool, which runs once rather than in a
// session.
func (c Config) IsTool(name string) bool {
	for _, t := range c.Tools {
		if t == name {
			return true
		}
	}
	return false
}

func Path() string { return filepath.Join(home.Dir(), "config.toml") }

// StatePath is the state file.
func StatePath() string { return filepath.Join(home.Dir(), "state.json") }

// Load reads the config, returning defaults when none exists.
func Load() (Config, error) {
	c := Config{Hosts: map[string]HostConfig{}}
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", Path(), err)
	}
	if c.Hosts == nil {
		c.Hosts = map[string]HostConfig{}
	}
	return c, nil
}

// RealPath spells p without symlinks. If p does not exist, its folder is
// resolved and its name kept.
func RealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Join(resolve(filepath.Dir(p)), filepath.Base(p))
}

// Save writes the config.
func Save(c Config) error {
	if err := home.Ensure(); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFile(Path(), b)
}

// PredictOn reports the prediction setting, on by default.
func (c Config) PredictOn() bool { return c.Predict == nil || *c.Predict }

// Host resolves a host name. "local" is always this machine. With no name it
// uses c.Use, falling back to the only configured host so a single-host setup
// needs no `use` line.
func (c Config) Host(name string) (transport.Host, error) {
	if name == "" {
		name = c.Use
	}
	if name == "" && len(c.Hosts) == 1 {
		for n := range c.Hosts {
			name = n
		}
	}
	if name == "local" {
		return transport.Local, nil
	}
	if name == "" {
		return transport.Host{}, errors.New("no host configured; run aft setup")
	}
	h, ok := c.Hosts[name]
	if !ok {
		return transport.Host{}, fmt.Errorf("unknown host %q; run aft setup", name)
	}
	if h.SSH == "" {
		return transport.Host{}, fmt.Errorf("host %q has no ssh destination", name)
	}
	return transport.Host{Name: name, SSH: h.SSH, Alive: h.Alive, Persist: h.Persist}, nil
}

// UsedHost is the name of the host used when none is given: c.Use, or the only
// host when there is exactly one, or "" when it is unset or ambiguous.
func (c Config) UsedHost() string {
	if c.Use != "" {
		return c.Use
	}
	if len(c.Hosts) == 1 {
		for n := range c.Hosts {
			return n
		}
	}
	return ""
}

// HostNames lists configured hosts in order.
func (c Config) HostNames() []string {
	names := make([]string, 0, len(c.Hosts))
	for n := range c.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadState reads the state file.
func LoadState() (State, error) {
	s := State{Projects: map[string]Project{}}
	b, err := os.ReadFile(StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", StatePath(), err)
	}
	if s.Projects == nil {
		s.Projects = map[string]Project{}
	}
	return s, nil
}

// SaveState writes the state file.
func SaveState(s State) error {
	if err := home.Ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(StatePath(), b)
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// FindProject returns the project dir belongs to, with its root as stored,
// choosing the most specific. Roots are kept by their real path (aft on sees
// to it), and dir is compared as given and by its real spelling, so a folder
// reached through a symlink (~/room for /Volumes/Room) is found either way.
// Only dir is ever resolved: it is where the caller stands, so it is alive;
// a stored root may sit on a share whose server is offline, and resolving it
// would hang every command.
func (s State) FindProject(dir string) (root string, p Project, ok bool) {
	best := ""
	for _, d := range spellings(dir) {
		for r := range s.Projects {
			if _, inside := relInside(d, r); inside && len(r) > len(best) {
				best = r
			}
		}
	}
	if best == "" {
		return "", Project{}, false
	}
	return best, s.Projects[best], true
}

// spellings is a path as given and, when that differs, by its real path.
func spellings(p string) []string {
	p = filepath.Clean(p)
	if real := RealPath(p); real != p {
		return []string{p, real}
	}
	return []string{p}
}

// relInside returns dir relative to root when dir is root or inside it.
func relInside(dir, root string) (string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// RemoteDir maps a local directory inside a project to its server directory.
func RemoteDir(root string, p Project, dir string) string {
	rel := "."
	for _, d := range spellings(dir) {
		if r, ok := relInside(d, root); ok {
			rel = r
			break
		}
	}
	if rel == "." {
		return p.Dir
	}
	return p.Dir + "/" + filepath.ToSlash(rel)
}

// LoadRepo reads aft.toml from dir or its parents, returning the root it was
// found in.
func LoadRepo(dir string) (RepoConfig, string, bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "aft.toml"))
		if err == nil {
			var r RepoConfig
			if toml.Unmarshal(b, &r) == nil {
				return r, d, true
			}
			return RepoConfig{}, d, false
		}
		if filepath.Dir(d) == d {
			return RepoConfig{}, "", false
		}
	}
}

// DefaultRemoteDir maps a local directory to the same path relative to home.
func DefaultRemoteDir(local string) string {
	h, err := os.UserHomeDir()
	if err == nil {
		if rel, err := filepath.Rel(h, local); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return local
}

// PrefixKey is the control byte that opens aft's own commands (copy mode,
// detach). It defaults to Ctrl-] when unset; "", "off" or "none" disable it.
func (c Config) PrefixKey() (byte, error) {
	s := "ctrl-]"
	if c.Prefix != nil {
		s = *c.Prefix
	}
	return DetachByte(s)
}

// DetachByte turns a detach setting such as "ctrl-]" into a byte.
func DetachByte(s string) (byte, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "off" || s == "none" {
		return 0, nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "ctrl-"), "^")
	if len(s) != 1 {
		return 0, fmt.Errorf("detach must look like \"ctrl-]\"")
	}
	ch := s[0]
	switch {
	case ch >= 'a' && ch <= 'z':
		return ch - 'a' + 1, nil
	case ch >= '[' && ch <= '_':
		return ch - '[' + 27, nil
	}
	return 0, fmt.Errorf("detach must be a control character such as \"ctrl-]\"")
}

// MapLocal returns the host and server directory for a local path that lives
// under a configured mount, choosing the most specific mount. Mounts are kept
// by their real path (aft mount add and detection see to it), so the path is
// compared as given and, when it is reached through a symlink (~/room may be
// /Volumes/Room), by its real spelling too. Mounts themselves are never
// resolved here, which could block on a share whose server is offline;
// callers pass paths they have just used, which cannot.
func (c Config) MapLocal(local string) (host, remote string, ok bool) {
	forms := spellings(local)
	best := -1
	var chosen Mount
	var rest string
	for _, m := range c.Mounts {
		ml := filepath.Clean(m.Local)
		for _, f := range forms {
			if (f == ml || strings.HasPrefix(f, ml+string(filepath.Separator))) && len(ml) > best {
				best, chosen, rest = len(ml), m, strings.TrimPrefix(f, ml)
			}
		}
	}
	if best < 0 {
		return "", "", false
	}
	return chosen.Host, strings.TrimRight(chosen.Path, "/") + filepath.ToSlash(rest), true
}
