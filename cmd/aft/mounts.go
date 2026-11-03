package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/fschrhunt/aft/internal/bridge/transport"
	"github.com/fschrhunt/aft/internal/config"
	"github.com/fschrhunt/aft/internal/ui"
)

// detected describes a network mount discovered under a path.
type detected struct {
	Point  string // the local mountpoint
	Kind   string // "smb" or "nfs"
	Server string // host or address the mount comes from
	Share  string // SMB share name
	Export string // NFS export path on the server
}

// detectMount returns the most specific SMB or NFS mount whose mountpoint
// contains dir. dir should already be resolved of symlinks.
func detectMount(dir string) (detected, bool) {
	mounts := listMounts()
	best := -1
	var chosen detected
	for _, m := range mounts {
		if dir == m.Point || strings.HasPrefix(dir, strings.TrimRight(m.Point, "/")+"/") {
			if len(m.Point) > best {
				best, chosen = len(m.Point), m
			}
		}
	}
	return chosen, best >= 0
}

func listMounts() []detected {
	if runtime.GOOS == "linux" {
		return parseProcMounts()
	}
	return parseBSDMounts()
}

// parseProcMounts reads cifs and nfs mounts from /proc/mounts (Linux).
func parseProcMounts() []detected {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []detected
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		src, point, fstype := unescapeMount(fields[0]), unescapeMount(fields[1]), fields[2]
		switch {
		case fstype == "cifs" || fstype == "smb3":
			server, share := splitUNC(src)
			out = append(out, detected{Point: point, Kind: "smb", Server: server, Share: share})
		case strings.HasPrefix(fstype, "nfs"):
			server, export := splitNFS(src)
			out = append(out, detected{Point: point, Kind: "nfs", Server: server, Export: export})
		}
	}
	return out
}

// parseBSDMounts parses `mount` output (macOS and BSD).
func parseBSDMounts() []detected {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return nil
	}
	var res []detected
	for _, line := range strings.Split(string(out), "\n") {
		// "//user@host/Share on /Volumes/Share (smbfs, ...)"
		// "host:/export on /mnt/x (nfs, ...)"
		src, rest, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		if i := strings.LastIndex(rest, " ("); i >= 0 {
			point := rest[:i]
			opts := rest[i+2:]
			switch {
			case strings.HasPrefix(opts, "smbfs"):
				server, share := splitUNC(src)
				res = append(res, detected{Point: point, Kind: "smb", Server: server, Share: share})
			case strings.HasPrefix(opts, "nfs"):
				server, export := splitNFS(src)
				res = append(res, detected{Point: point, Kind: "nfs", Server: server, Export: export})
			}
		}
	}
	return res
}

// splitUNC parses //[user@]server/share into server and share.
func splitUNC(s string) (server, share string) {
	s = strings.TrimPrefix(s, "//")
	host, sh, _ := strings.Cut(s, "/")
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	return host, strings.SplitN(sh, "/", 2)[0]
}

// splitNFS parses server:/export into server and export.
func splitNFS(s string) (server, export string) {
	host, ex, ok := strings.Cut(s, ":")
	if !ok {
		return "", ""
	}
	return host, ex
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces etc.
func unescapeMount(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var n int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &n); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// hostForServer finds a configured host whose SSH destination resolves to the
// mount's server address, so a detected mount can be tied to an aft host.
func hostForServer(cfg config.Config, server string) string {
	for _, name := range cfg.HostNames() {
		h, err := cfg.Host(name)
		if err != nil {
			continue
		}
		if sameServer(h, server) {
			return name
		}
	}
	return ""
}

// sameServer reports whether an aft host points at the given server, comparing
// by resolved address so a mount that names an IP matches a host configured
// with a DNS name (such as a Tailscale MagicDNS name), and vice versa.
func sameServer(h transport.Host, server string) bool {
	hn := sshHostName(h)
	if hn == server || h.Dest() == server {
		return true
	}
	a, b := resolveSet(server), resolveSet(hn)
	for ip := range a {
		if b[ip] {
			return true
		}
	}
	return false
}

// sshHostName returns the HostName ssh resolves for a destination, or the
// destination itself when ssh gives nothing useful.
func sshHostName(h transport.Host) string {
	if a, err := h.Address(); err == nil {
		return a // as ssh resolves it, with the host's own options
	}
	return h.Dest()
}

// resolveSet returns the IP addresses a host or address resolves to, including
// the literal itself. A bare IP resolves to just itself.
func resolveSet(host string) map[string]bool {
	set := map[string]bool{host: true}
	if net.ParseIP(host) != nil {
		return set
	}
	if addrs, err := net.LookupHost(host); err == nil {
		for _, a := range addrs {
			set[a] = true
		}
	}
	return set
}

// serverPathFor derives the server directory a detected mount exposes: the NFS
// export directly, or the SMB share resolved through the server's server.
func serverPathFor(h transport.Host, d detected) (string, error) {
	if d.Kind == "nfs" {
		if d.Export == "" {
			return "", fmt.Errorf("could not read the NFS export path")
		}
		return strings.TrimRight(d.Export, "/"), nil
	}
	out, err := transport.Output(h, nil, "sharepath", d.Share)
	if err != nil {
		return "", fmt.Errorf("ask %s for the path of share %q: %w", h.Name, d.Share, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("the server did not return a path for share %q", d.Share)
	}
	return strings.TrimRight(path, "/"), nil
}

func cmdMount(args []string) int {
	if len(args) == 0 || args[0] == "list" {
		cfg, err := config.Load()
		if err != nil {
			return fail(err)
		}
		if len(cfg.Mounts) == 0 {
			fmt.Println("No mounts configured.")
			return 0
		}
		for _, mapping := range cfg.Mounts {
			fmt.Printf("%s -> %s:%s\n", ui.Path(mapping.Local), mapping.Host, mapping.Path)
		}
		return 0
	}
	switch args[0] {
	case "add":
		fs := flags("mount add")
		local := fs.String("local", "", "local mountpoint")
		host := fs.String("host", "", "aft host the mount comes from")
		remote := fs.String("remote", "", "server path the mount exposes")
		if err := fs.Parse(args[1:]); err != nil {
			return flagExit(err)
		}
		if *local == "" || *host == "" || *remote == "" {
			fmt.Fprintln(os.Stderr, "usage: aft mount add --local DIR --host HOST --remote DIR")
			return 2
		}
		abs, _ := filepath.Abs(*local)
		abs = config.RealPath(abs) // mounts are kept by their real path; see MapLocal
		cfg, err := config.Load()
		if err != nil {
			return fail(err)
		}
		if _, err := cfg.Host(*host); err != nil {
			return fail(err)
		}
		cfg.Mounts = addMount(cfg.Mounts, config.Mount{Local: abs, Host: *host, Path: strings.TrimRight(*remote, "/")})
		if err := config.Save(cfg); err != nil {
			return fail(err)
		}
		ui.OK("Mapped %s -> %s:%s", abs, *host, strings.TrimRight(*remote, "/"))
		return 0
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: aft mount remove LOCAL")
			return 2
		}
		abs, _ := filepath.Abs(args[1])
		cfg, err := config.Load()
		if err != nil {
			return fail(err)
		}
		// Either spelling names the mount, which is kept by its real path.
		real := config.RealPath(abs)
		var kept []config.Mount
		for _, m := range cfg.Mounts {
			if l := filepath.Clean(m.Local); l != filepath.Clean(abs) && l != real {
				kept = append(kept, m)
			}
		}
		if len(kept) == len(cfg.Mounts) {
			ui.Error("no mount at %s", abs)
			ui.Hint("aft mount list shows them")
			return 1
		}
		cfg.Mounts = kept
		if err := config.Save(cfg); err != nil {
			return fail(err)
		}
		ui.OK("Removed mount %s", abs)
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: aft mount list | add ... | remove LOCAL")
	return 2
}

// addMount replaces any mapping with the same local root, then appends.
func addMount(ms []config.Mount, m config.Mount) []config.Mount {
	var out []config.Mount
	for _, e := range ms {
		if filepath.Clean(e.Local) != filepath.Clean(m.Local) {
			out = append(out, e)
		}
	}
	out = append(out, m)
	sort.Slice(out, func(i, j int) bool { return out[i].Local < out[j].Local })
	return out
}

// resolveLocalDir turns a local working directory into the host and server
// directory to use. It checks configured mounts first, then offers to register
// a detected network mount, and otherwise returns ok=false so the caller falls
// back to its normal path logic.
func resolveLocalDir(cfg *config.Config, cwd string) (host, dir string, ok bool) {
	phys, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		phys = cwd
	}
	if h, r, found := cfg.MapLocal(phys); found {
		return h, r, true
	}
	d, found := detectMount(phys)
	if !found {
		return "", "", false
	}
	hostName := hostForServer(*cfg, d.Server)
	if hostName == "" {
		fmt.Printf("This directory is on a %s mount from %s, but no aft host matches that server.\n", strings.ToUpper(d.Kind), d.Server)
		fmt.Printf("Register it once with: aft mount add --local %s --host HOST --remote /server/path\n", d.Point)
		return "", "", false
	}
	h, err := cfg.Host(hostName)
	if err != nil {
		return "", "", false
	}
	if d.Kind == "smb" {
		if err := connect(h); err != nil {
			ui.Error("%v", err)
			return "", "", false
		}
		if _, err := ensureServer(h); err != nil {
			ui.Error("%v", err)
			return "", "", false
		}
	}
	serverPath, err := serverPathFor(h, d)
	if err != nil {
		ui.Error("%v", err)
		fmt.Printf("Register it once with: aft mount add --local %s --host %s --remote /server/path\n", d.Point, hostName)
		return "", "", false
	}
	fmt.Printf("This directory is on %s, a %s mount of %s:%s.\n", d.Point, strings.ToUpper(d.Kind), hostName, serverPath)
	if !ui.Confirm("Map it so aft uses the files in place (no clone, no sync)?", false) {
		return "", "", false
	}
	cfg.Mounts = addMount(cfg.Mounts, config.Mount{Local: d.Point, Host: hostName, Path: serverPath})
	if err := config.Save(*cfg); err != nil {
		ui.Error("%v", err)
	}
	h2, r, _ := cfg.MapLocal(phys)
	return h2, r, true
}
