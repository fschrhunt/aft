// Package transport reaches a server's server. Over SSH it uses the user's own
// OpenSSH client with one shared master connection per host, so keys, agents,
// jump hosts and host trust all come from the user's SSH configuration. The
// reserved host "local" runs the server on this machine, which is how tests
// and single-machine trials work.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/arocomputer/aft/internal/home"
)

// Host is a server to reach. SSH is the destination given to ssh, usually an
// alias from ~/.ssh/config. It may also carry ssh options before the
// destination, such as "-p 2222 user@box". Name "local" means this machine.
type Host struct {
	Name    string
	SSH     string
	Alive   int // ServerAliveInterval seconds, 0 uses the default
	Persist int // ControlPersist seconds, 0 uses the default
}

// sshDest splits the SSH field into options and the destination.
func (h Host) sshDest() (opts []string, dest string) {
	f := strings.Fields(h.SSH)
	if len(f) == 0 {
		return nil, ""
	}
	return f[:len(f)-1], f[len(f)-1]
}

// Dest is the ssh destination without options, for messages.
func (h Host) Dest() string {
	_, d := h.sshDest()
	return d
}

// Address is the network address SSH reaches h at, after ~/.ssh/config: what
// a direct UDP connection should dial. It fails when SSH reaches the host
// through a jump host or proxy, which UDP cannot follow.
func (h Host) Address() (string, error) {
	hostOpts, dest := h.sshDest()
	out, err := exec.Command("ssh", append(append(append([]string{}, hostOpts...), "-G", "--"), dest)...).Output()
	if err != nil {
		return "", err
	}
	addr := ""
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "hostname":
			addr = v
		case "proxyjump", "proxycommand":
			if v != "none" && v != "" {
				return "", fmt.Errorf("%s is reached through %s, which UDP cannot follow", h.Name, k)
			}
		}
	}
	if addr == "" {
		return "", fmt.Errorf("no address for %s", h.Name)
	}
	return addr, nil
}

// Local is the host that runs sessions on this machine.
var Local = Host{Name: "local"}

// IsLocal reports whether the host is this machine.
func (h Host) IsLocal() bool { return h.Name == "local" || h.SSH == "" }

// RemoteHome is Aft's plumbing directory on the server (home.Data there),
// expanded by the remote shell. AFT_REMOTE_HOME overrides it, which tests use
// to keep a server on this same machine away from the real one.
func RemoteHome() string {
	if d := os.Getenv("AFT_REMOTE_HOME"); d != "" {
		return d
	}
	return "~/.local/share/aft"
}

// RemoteServer is the installed server binary's path on a host.
func RemoteServer() string { return RemoteHome() + "/" + home.ServerName }

// RemotePrefix sets AFT_HOME for the server when the remote home is overridden.
func RemotePrefix() string {
	if os.Getenv("AFT_REMOTE_HOME") != "" {
		return "AFT_HOME=" + Quote(RemoteHome()) + " "
	}
	return ""
}

func controlPath() string { return filepath.Join(home.Sockets(), "cm-%C") }

// sshArgs builds the arguments for one ssh invocation: shared options, the
// host's own options, then the destination.
func sshArgs(h Host, batch bool, extra ...string) []string {
	return sshArgsWithin(h, batch, connectTimeout, extra...)
}

// connectTimeout is how long SSH tries to reach a host, in seconds: an
// offline host fails in seconds, not minutes.
const connectTimeout = 10

// sshArgsWithin is sshArgs giving up on connecting after secs seconds.
func sshArgsWithin(h Host, batch bool, secs int, extra ...string) []string {
	hostOpts, dest := h.sshDest()
	args := append(sshOptions(h, batch, secs), hostOpts...)
	args = append(args, extra...)
	return append(args, "--", dest)
}

// sshOptions are shared by every ssh invocation.
func sshOptions(h Host, batch bool, connectSecs int) []string {
	persist, alive := 600, 5
	if h.Persist > 0 {
		persist = h.Persist
	}
	if h.Alive > 0 {
		alive = h.Alive
	}
	opts := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath(),
		"-o", fmt.Sprintf("ControlPersist=%d", persist),
		"-o", fmt.Sprintf("ServerAliveInterval=%d", alive),
		"-o", fmt.Sprintf("ConnectTimeout=%d", connectSecs),
		"-o", "ServerAliveCountMax=3",
	}
	if batch {
		opts = append(opts, "-o", "BatchMode=yes")
	}
	return opts
}

// EnsureMaster makes sure a master connection to the host exists. The first
// connection may prompt for a passphrase, so it inherits the terminal; callers
// run it before entering raw mode.
func EnsureMaster(h Host) error {
	if h.IsLocal() {
		return nil
	}
	if err := home.Ensure(); err != nil {
		return err
	}
	if MasterAlive(h) {
		return nil
	}
	cmd := exec.Command("ssh", sshArgs(h, false, "-fN")...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("connect to %s: %w", h.Dest(), err)
	}
	return nil
}

// MasterAlive reports whether the shared connection is up.
func MasterAlive(h Host) bool {
	if h.IsLocal() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", controlArgs(h, "check")...)
	return cmd.Run() == nil
}

// EnsureMasterBatch connects without a terminal, so it never prompts: for use
// while attached, or behind a spinner.
func EnsureMasterBatch(h Host) error { return EnsureMasterWithin(h, connectTimeout) }

// EnsureMasterWithin is EnsureMasterBatch giving up on an unreachable host
// after secs seconds, for listings that should not wait on an offline machine.
func EnsureMasterWithin(h Host, secs int) error {
	if h.IsLocal() || MasterAlive(h) {
		return nil
	}
	if err := home.Ensure(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", sshArgsWithin(h, true, secs, "-fN")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return &ConnectError{Host: h.Name, Dest: h.Dest(), Msg: msg}
	}
	return nil
}

// ConnectError is a failed connection without a terminal, naming the host as
// the user calls it and, when SSH's message does not, the address. NeedsTerminal says
// whether SSH wanted to ask something (a passphrase, a new host key), so a
// caller with a terminal can retry interactively; otherwise the host is
// simply unreachable and asking would not help.
type ConnectError struct{ Host, Dest, Msg string }

func (e *ConnectError) Error() string {
	if e.Dest != "" && e.Dest != e.Host && !strings.Contains(e.Msg, e.Dest) {
		return "cannot reach " + e.Host + " (" + e.Dest + "): " + e.Msg
	}
	return "cannot reach " + e.Host + ": " + e.Msg
}

// NeedsTerminal reports whether SSH stopped because it needed to ask.
func (e *ConnectError) NeedsTerminal() bool {
	for _, s := range []string{"Permission denied", "Host key verification failed", "passphrase", "password"} {
		if strings.Contains(e.Msg, s) {
			return true
		}
	}
	return false
}

// NeedsTerminal reports whether err is a connection that SSH could complete
// only by asking in the terminal.
func NeedsTerminal(err error) bool {
	var ce *ConnectError
	return errors.As(err, &ce) && ce.NeedsTerminal()
}

// controlArgs addresses the master connection for -O commands.
func controlArgs(h Host, op string, extra ...string) []string {
	hostOpts, dest := h.sshDest()
	args := append([]string{"-o", "ControlPath=" + controlPath()}, hostOpts...)
	args = append(args, "-O", op)
	args = append(args, extra...)
	return append(args, "--", dest)
}

// CloseMaster ends the shared connection.
func CloseMaster(h Host) {
	if h.IsLocal() {
		return
	}
	exec.Command("ssh", controlArgs(h, "exit")...).Run()
}

// Proc is a running server command with its pipes.
type Proc struct {
	cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	stderr bytes.Buffer
}

// Server starts a server subcommand on the host with piped stdin and stdout.
func Server(h Host, args ...string) (*Proc, error) {
	cmd, err := serverCommand(h, args)
	if err != nil {
		return nil, err
	}
	p := &Proc{cmd: cmd}
	cmd.Stderr = &p.stderr
	p.Stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	p.Stdout, err = cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

// Wait reaps the process and returns its stderr with any error.
func (p *Proc) Wait() (string, error) {
	err := p.cmd.Wait()
	return strings.TrimSpace(p.stderr.String()), err
}

// Kill stops the process.
func (p *Proc) Kill() {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

// Stderr returns what the server has written to stderr so far.
func (p *Proc) Stderr() string { return strings.TrimSpace(p.stderr.String()) }

// Output runs a server subcommand to completion and returns its stdout.
func Output(h Host, stdin io.Reader, args ...string) ([]byte, error) {
	return OutputContext(context.Background(), h, stdin, args...)
}

// OutputContext is Output that stops the command when ctx is cancelled.
func OutputContext(ctx context.Context, h Host, stdin io.Reader, args ...string) ([]byte, error) {
	cmd, err := serverCommand(h, args)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { cmd.Process.Kill() })
	defer stop()
	err = cmd.Wait()
	out := stdout.Bytes()
	if err != nil && ctx.Err() != nil {
		return out, ctx.Err() // stopped on request; a command that finished first stands
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s", msg)
	}
	return out, nil
}

// serverCommand builds the command that runs a server subcommand on the host.
func serverCommand(h Host, args []string) (*exec.Cmd, error) {
	if h.IsLocal() {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		// The aft binary is the server when started as aft-server.
		cmd := exec.Command(self, args...)
		cmd.Args[0] = home.ServerName
		return cmd, nil
	}
	remote := RemotePrefix() + RemoteServer() + " " + shellJoin(args)
	return exec.Command("ssh", append(sshArgs(h, true, "-T"), remote)...), nil
}

// Shell runs a command on the host with this process's stdin, stdout and
// stderr. It asks for a remote terminal only when stdin and stdout are one, so
// piped input and output pass through byte for byte.
func Shell(h Host, script string) error {
	var cmd *exec.Cmd
	if h.IsLocal() {
		cmd = exec.Command("sh", "-c", script)
	} else {
		tty := "-T"
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			tty = "-t"
		}
		cmd = exec.Command("ssh", append(sshArgs(h, false, tty), script)...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// RunRaw runs a remote shell script with the given stdin and returns stdout.
func RunRaw(h Host, stdin io.Reader, script string) ([]byte, error) {
	var cmd *exec.Cmd
	if h.IsLocal() {
		cmd = exec.Command("sh", "-c", script)
	} else {
		cmd = exec.Command("ssh", append(sshArgs(h, true, "-T"), script)...)
	}
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s", msg)
	}
	return out, nil
}

// Forward adds or cancels a loopback port forward on the master connection.
func Forward(h Host, port int, add bool) error {
	if h.IsLocal() {
		return nil
	}
	op := "forward"
	if !add {
		op = "cancel"
	}
	spec := fmt.Sprintf("127.0.0.1:%d:localhost:%d", port, port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", controlArgs(h, op, "-L", spec)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s port %d: %s", op, port, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Quote makes one POSIX shell word.
func Quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;~#") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}
