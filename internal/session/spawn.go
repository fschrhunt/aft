package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arocomputer/aft/internal/home"
)

var idOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidID reports whether a session id is safe to use in paths.
func ValidID(id string) bool { return idOK.MatchString(id) }

// Start launches a detached session process for o and waits until its socket
// accepts connections. server is the argv prefix that runs server
// subcommands: the server binary, or the aft binary, which is the server when
// started as aft-server.
func Start(server []string, o Options) error {
	if len(server) == 0 {
		return fmt.Errorf("no server command")
	}
	if !ValidID(o.ID) {
		return fmt.Errorf("invalid session id %q", o.ID)
	}
	if fi, err := os.Stat(o.Dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the folder %s is gone on this host; recreate it there, or run aft on --dir PATH in the project to point it at another", o.Dir)
	}
	if err := home.Ensure(); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(home.Logs(), o.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	args := append(append([]string{}, server[1:]...), "serve", "--id", o.ID, "--dir", o.Dir, "--cols", strconv.Itoa(o.Cols), "--rows", strconv.Itoa(o.Rows))
	for _, kv := range o.Env {
		args = append(args, "--env", kv)
	}
	for _, w := range o.Wrapper {
		args = append(args, "--wrap", w)
	}
	args = append(args, "--")
	args = append(args, o.Command...)
	cmd := exec.Command(server[0], args...)
	cmd.Args[0] = home.ServerName // so ps shows aft-server serve --id NAME
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the session if it exits; otherwise it outlives us by design.
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(SocketPath(o.ID)); err == nil {
			return nil
		}
		select {
		case <-exited:
			deadline = time.Time{}
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The name was claimed with an empty record (see NewID); give it back.
	if fi, err := os.Stat(RecordPath(o.ID)); err == nil && fi.Size() == 0 {
		os.Remove(RecordPath(o.ID))
	}
	tail, _ := os.ReadFile(filepath.Join(home.Logs(), o.ID+".log"))
	msg := strings.TrimSpace(string(tail))
	if len(msg) > 500 {
		msg = msg[len(msg)-500:]
	}
	return fmt.Errorf("session %s did not start: %s", o.ID, msg)
}
