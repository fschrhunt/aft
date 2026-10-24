// Package session is the part of Aft on the server, one of its three parts
// beside the terminal and the bridge. It keeps one application running inside a
// pseudo-terminal, tracks its screen, and lets one client at a time attach to
// it. It is Aft's replacement for tmux: it has no appearance of its own, passes
// bytes through untouched while a client is attached, answers the terminal's
// queries while nobody is, and repaints the screen when a client returns.
package session

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/arocomputer/aft/internal/bridge/proto"
	"github.com/arocomputer/aft/internal/bridge/udp"
	"github.com/arocomputer/aft/internal/home"
	"github.com/arocomputer/aft/internal/vt"
)

// Options describe the session to run.
type Options struct {
	ID      string
	Command []string
	Dir     string
	Env     []string // KEY=VALUE pairs allowed in from the client
	Wrapper []string // optional command to run the agent inside, e.g. nix develop -c
	Cols    int
	Rows    int
}

// Record is the JSON file that lists a live session.
type Record struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
	Dir     string   `json:"dir"`
	PID     int      `json:"pid"`     // the application's process id
	Server  int      `json:"server"`  // the session server's process id
	Started string   `json:"started"` // RFC 3339
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Title   string   `json:"title,omitempty"` // what the application says it is doing, from its window title
	// Attached is whether a terminal is showing the session. A detached session
	// keeps running on the server with no terminal.
	Attached bool `json:"attached,omitempty"`
	// Ended is set on the remains of a session that ended unseen.
	Ended *Ending `json:"ended,omitempty"`
}

// clientGone is how long an attached client that sends heartbeats may stay
// silent before the session counts it as gone and detaches it.
const clientGone = 30 * time.Second

// SocketPath is where clients attach to a session.
func SocketPath(id string) string { return filepath.Join(home.Sockets(), id+".sock") }

// BridgePath is where shims running inside the session reach the client.
func BridgePath(id string) string { return filepath.Join(home.Sockets(), id+".bridge.sock") }

// RecordPath is the session's JSON record.
func RecordPath(id string) string { return filepath.Join(home.Sessions(), id+".json") }

// client is one attached connection with its own writer goroutine, so a slow
// or dead client never stalls the pty reader. Closing flushes queued frames
// first, which is how a Detached or Exit notice reaches the client in time.
type client struct {
	conn net.Conn
	out  chan []byte
	quit chan struct{} // closed by close(); the writer drains, then closes conn
	done chan struct{} // closed once the connection is really closed
	once sync.Once
}

func newClient(conn net.Conn) *client {
	c := &client{conn: conn, out: make(chan []byte, 512), quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer conn.Close()
		w := bufio.NewWriterSize(conn, 64<<10)
		write := func(b []byte) bool {
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := w.Write(b)
			return err == nil
		}
		for {
			select {
			case b := <-c.out:
				if !write(b) {
					return
				}
				for len(c.out) > 0 {
					if !write(<-c.out) {
						return
					}
				}
				if w.Flush() != nil {
					return
				}
			case <-c.quit:
				for len(c.out) > 0 {
					if !write(<-c.out) {
						return
					}
				}
				w.Flush()
				return
			}
		}
	}()
	return c
}

// send queues one frame; it drops the client if it has gone away.
func (c *client) send(t proto.Type, payload []byte) {
	var buf []byte
	hdr := [5]byte{byte(t), byte(len(payload) >> 24), byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}
	buf = append(buf, hdr[:]...)
	buf = append(buf, payload...)
	select {
	case c.out <- buf:
	case <-c.quit:
	}
}

func (c *client) sendJSON(t proto.Type, v any) {
	b, _ := json.Marshal(v)
	c.send(t, b)
}

func (c *client) close() {
	c.once.Do(func() { close(c.quit) })
}

// Session is a running session.
type Session struct {
	opts   Options
	ptmx   *os.File
	cmd    *exec.Cmd
	term   *vt.Terminal
	record Record

	mu       sync.Mutex
	cur      *client
	attached bool

	bridgeMu sync.Mutex
	bridgeID uint32
	waiting  map[uint32]chan proto.BridgeResponse

	exited chan struct{}
	exit   proto.ExitInfo

	udpMu sync.Mutex
	udp   *udp.Listener // opened on the first ReqUDP, closed when the session ends

	dirty  bool // output arrived since the screen was last saved
	killed bool // aft kill ended it, so it leaves no remains
}

// saveScreen keeps the screen as plain text, so it outlives the session.
// The caller holds mu.
func (s *Session) saveScreen() {
	os.WriteFile(ScreenPath(s.opts.ID), []byte(screenText(s.term.Snapshot())), 0o600)
	s.dirty = false
}

// keepScreen saves the screen every half minute while output changes it.
func (s *Session) keepScreen() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.exited:
			return
		case <-t.C:
			s.mu.Lock()
			if s.dirty {
				s.saveScreen()
			}
			s.mu.Unlock()
		}
	}
}

// Serve runs the session until the application exits. It is the body of the
// detached session process.
func Serve(o Options) error {
	if err := home.Ensure(); err != nil {
		return err
	}
	if o.Cols < 1 || o.Rows < 1 {
		o.Cols, o.Rows = 80, 24
	}
	if self, err := os.Executable(); err == nil {
		home.InstallShims(self)
	}
	s := &Session{opts: o, term: vt.New(o.Cols, o.Rows), waiting: map[uint32]chan proto.BridgeResponse{}, exited: make(chan struct{})}
	s.term.Reply = s.replyWhenDetached

	cmd := buildCommand(o.Wrapper, o.Command)
	cmd.Dir = o.Dir
	cmd.Env = s.environment()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(o.Rows), Cols: uint16(o.Cols)})
	if err != nil {
		return fmt.Errorf("start %s: %w", o.Command[0], err)
	}
	s.ptmx, s.cmd = ptmx, cmd
	s.record = Record{ID: o.ID, Command: o.Command, Dir: o.Dir, PID: cmd.Process.Pid, Server: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339), Cols: o.Cols, Rows: o.Rows}
	if err := writeJSON(RecordPath(o.ID), s.record); err != nil {
		return err
	}
	defer os.Remove(RecordPath(o.ID))

	if c, err := net.Dial("unix", SocketPath(o.ID)); err == nil {
		c.Close()
		return fmt.Errorf("session %s is already running", o.ID)
	}
	os.Remove(SocketPath(o.ID)) // a socket a dead session left behind
	ln, err := net.Listen("unix", SocketPath(o.ID))
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(SocketPath(o.ID))
	os.Remove(BridgePath(o.ID))
	bln, err := net.Listen("unix", BridgePath(o.ID))
	if err != nil {
		return err
	}
	defer bln.Close()
	defer os.Remove(BridgePath(o.ID))

	go s.readPTY()
	go s.acceptClients(ln)
	go s.acceptBridge(bln)
	go s.pollListeners()
	go s.keepScreen()

	err = cmd.Wait()
	s.exit = exitInfo(err)
	close(s.exited)
	s.udpMu.Lock()
	l := s.udp
	s.udpMu.Unlock()
	if l != nil {
		l.Close()
		defer l.Drain(3 * time.Second) // after the Exit frame below is sent
	}
	s.mu.Lock()
	switch {
	case s.cur != nil:
		s.cur.sendJSON(proto.Exit, s.exit)
		s.cur.close()
		<-s.cur.done
		ClearRemains(o.ID) // whoever was attached saw it end
	case s.killed:
		ClearRemains(o.ID)
	default:
		// It ended unseen: keep how, and its last screen, for aft ls and attach.
		s.saveScreen()
		leaveRemains(s.record, Ending{At: time.Now().UTC().Format(time.RFC3339), Code: s.exit.Code, Signal: s.exit.Signal})
	}
	s.mu.Unlock()
	log.Printf("session %s: %s exited: %+v", o.ID, o.Command[0], s.exit)
	return nil
}

// buildCommand builds the process to run in the session. With no wrapper it
// runs the agent through the user's login shell, so the server PATH and
// environment match a normal terminal there, with Aft's shim directory first.
// A configured wrapper replaces that: the agent runs as wrapper + command, so
// the user can launch it inside a container, a nix or mise environment, a
// specific shell, or anything else, described per host or per project. The
// session environment (AFT_SHIMS, AFT_BRIDGE) is set either way, so a wrapper
// that inherits the environment keeps the bridges working.
func buildCommand(wrapper, argv []string) *exec.Cmd {
	if len(wrapper) > 0 {
		full := append(append([]string(nil), wrapper...), argv...)
		return exec.Command(full[0], full[1:]...)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	script := `export PATH="$AFT_SHIMS:$PATH"; exec "$0" "$@"`
	args := append([]string{"-l", "-c", script}, argv...)
	return exec.Command(shell, args...)
}

func (s *Session) environment() []string {
	env := os.Environ()
	drop := map[string]bool{"AFT_SESSION": true, "AFT_BRIDGE": true, "AFT_SHIMS": true, "TERM": true, "COLORTERM": true, "TERM_PROGRAM": true, "TERM_PROGRAM_VERSION": true, "LANG": true, "LC_ALL": true}
	out := make([]string, 0, len(env)+len(s.opts.Env)+4)
	for _, kv := range env {
		k, _, _ := cutKV(kv)
		if !drop[k] {
			out = append(out, kv)
		}
	}
	out = append(out, s.opts.Env...)
	out = append(out, "AFT_SESSION="+s.opts.ID, "AFT_BRIDGE="+BridgePath(s.opts.ID), "AFT_SHIMS="+home.Shims())
	return out
}

func cutKV(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

func exitInfo(err error) proto.ExitInfo {
	if err == nil {
		return proto.ExitInfo{Code: 0}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return proto.ExitInfo{Code: 128 + int(st.Signal()), Signal: st.Signal().String()}
		}
		return proto.ExitInfo{Code: ee.ExitCode()}
	}
	return proto.ExitInfo{Code: 1}
}

func (s *Session) replyWhenDetached(b []byte) {
	// Called with s.mu held by readPTY.
	if !s.attached {
		s.ptmx.Write(b)
	}
}

func (s *Session) readPTY() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.term.Write(buf[:n])
			s.dirty = true
			if t := cleanTitle(s.term.Title()); t != s.record.Title {
				s.record.Title = t
				select {
				case <-s.exited: // the record is being removed; do not write it back
				default:
					writeJSON(RecordPath(s.opts.ID), s.record)
				}
			}
			if s.cur != nil {
				s.cur.send(proto.Output, buf[:n])
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) acceptClients(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.serveClient(conn)
	}
}

// serveClient attaches one client: it waits for the client's size, repaints,
// then relays input until the client leaves.
func (s *Session) serveClient(conn net.Conn) {
	r := bufio.NewReaderSize(conn, 64<<10)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, err := proto.Read(r)
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	c := newClient(conn)
	cols, rows, ok := proto.DecodeResize(first.Payload)
	if first.Type != proto.Resize || !ok {
		c.send(proto.Error, []byte("expected a resize frame"))
		c.close()
		return
	}
	if first.Type == proto.Resize && cols == 0 {
		// A control-only connection (end, udp) never attaches.
		s.control(c, r)
		return
	}

	s.mu.Lock()
	if old := s.cur; old != nil {
		old.send(proto.Detached, nil)
		old.close()
	}
	s.cur = c
	s.attached = true
	s.record.Attached = true
	resized := cols != s.record.Cols || rows != s.record.Rows
	if resized {
		s.resize(cols, rows)
	} else {
		writeJSON(RecordPath(s.opts.ID), s.record)
	}
	c.sendJSON(proto.Hello, proto.HelloInfo{ID: s.opts.ID, Command: s.opts.Command, Dir: s.opts.Dir, PID: s.record.PID, Cols: cols, Rows: rows, Started: s.record.Started})
	c.send(proto.Output, s.term.Render(vt.RenderOptions{Scrollback: true, Title: true}))
	s.mu.Unlock()
	if !resized {
		// Same size means no SIGWINCH from the resize; ask the app to redraw.
		syscall.Kill(-s.record.PID, syscall.SIGWINCH)
	}
	log.Printf("session %s: client attached (%dx%d)", s.opts.ID, cols, rows)

	// A client that sends heartbeats and then goes quiet has gone, even if its
	// connection has not closed yet, as when a laptop sleeps.
	heartbeat := false
	for {
		if heartbeat {
			conn.SetReadDeadline(time.Now().Add(clientGone))
		}
		f, err := proto.Read(r)
		if err != nil {
			break
		}
		switch f.Type {
		case proto.Input:
			s.ptmx.Write(f.Payload)
		case proto.Resize:
			if cols, rows, ok := proto.DecodeResize(f.Payload); ok && cols > 0 {
				s.mu.Lock()
				s.resize(cols, rows)
				s.mu.Unlock()
			}
		case proto.Detach:
			c.send(proto.Detached, nil)
			goto done
		case proto.Ping:
			heartbeat = true
			c.send(proto.Pong, nil)
		case proto.ReqSnapshot:
			s.mu.Lock()
			snap := s.term.Snapshot()
			s.mu.Unlock()
			var buf bytes.Buffer
			if gob.NewEncoder(&buf).Encode(snap) == nil {
				c.send(proto.Snapshot, buf.Bytes())
			}
		case proto.BridgeReply:
			var resp proto.BridgeResponse
			if json.Unmarshal(f.Payload, &resp) == nil {
				s.bridgeMu.Lock()
				ch := s.waiting[resp.ID]
				delete(s.waiting, resp.ID)
				s.bridgeMu.Unlock()
				if ch != nil {
					ch <- resp
				}
			}
		case proto.End:
			s.end()
		}
	}
done:
	s.mu.Lock()
	if s.cur == c {
		s.cur = nil
		s.attached = false
		s.record.Attached = false
		select {
		case <-s.exited: // the record is being removed; do not write it back
		default:
			writeJSON(RecordPath(s.opts.ID), s.record)
			s.saveScreen()
		}
	}
	s.mu.Unlock()
	c.close()
	log.Printf("session %s: client detached", s.opts.ID)
}

// openUDP starts the session's UDP listener, once, and returns how to reach
// it. Clients it admits are served exactly like those on the unix socket.
func (s *Session) openUDP() (udp.Offer, error) {
	s.udpMu.Lock()
	defer s.udpMu.Unlock()
	select {
	case <-s.exited:
		return udp.Offer{}, errors.New(endedText)
	default:
	}
	if s.udp == nil {
		l, err := udp.Listen()
		if err != nil {
			return udp.Offer{}, err
		}
		s.udp = l
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					// The listener died; forget it, so the next ReqUDP opens a
					// new one instead of offering a dead port.
					s.udpMu.Lock()
					if s.udp == l {
						s.udp = nil
					}
					s.udpMu.Unlock()
					log.Printf("session %s: UDP listener stopped: %v", s.opts.ID, err)
					return
				}
				go s.serveClient(conn)
			}
		}()
		log.Printf("session %s: listening on UDP %d", s.opts.ID, l.Offer.Port)
	}
	return s.udp.Offer, nil
}

// control serves a connection that only sends control frames.
func (s *Session) control(c *client, r *bufio.Reader) {
	defer c.close()
	for {
		f, err := proto.Read(r)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.End:
			s.end()
			c.send(proto.Pong, nil)
		case proto.ReqUDP:
			if o, err := s.openUDP(); err != nil {
				c.send(proto.Error, []byte(err.Error()))
			} else {
				c.sendJSON(proto.UDPOffer, o)
			}
		case proto.Ping:
			c.send(proto.Pong, nil)
		}
	}
}

func (s *Session) resize(cols, rows int) {
	s.record.Cols, s.record.Rows = cols, rows
	s.term.Resize(cols, rows)
	pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	writeJSON(RecordPath(s.opts.ID), s.record)
}

// end stops the application: hang up, then kill if it lingers.
func (s *Session) end() {
	s.mu.Lock()
	s.killed = true
	s.mu.Unlock()
	pid := s.record.PID
	syscall.Kill(-pid, syscall.SIGHUP)
	go func() {
		select {
		case <-s.exited:
		case <-time.After(3 * time.Second):
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

// acceptBridge serves shims inside the session that need the user's machine.
func (s *Session) acceptBridge(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(90 * time.Second))
			var req proto.BridgeRequest
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				return
			}
			resp := s.bridge(req)
			json.NewEncoder(conn).Encode(resp)
		}()
	}
}

func (s *Session) bridge(req proto.BridgeRequest) proto.BridgeResponse {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()
	if c == nil {
		return proto.BridgeResponse{OK: false, Error: "no client is attached to this session"}
	}
	ch := make(chan proto.BridgeResponse, 1)
	s.bridgeMu.Lock()
	s.bridgeID++
	req.ID = s.bridgeID
	s.waiting[req.ID] = ch
	s.bridgeMu.Unlock()
	c.sendJSON(proto.BridgeReq, req)
	select {
	case resp := <-ch:
		return resp
	case <-time.After(60 * time.Second):
		s.bridgeMu.Lock()
		delete(s.waiting, req.ID)
		s.bridgeMu.Unlock()
		return proto.BridgeResponse{OK: false, Error: "the client did not answer"}
	case <-c.done:
		return proto.BridgeResponse{OK: false, Error: "the client disconnected"}
	}
}

// pollListeners reports listening TCP ports to the attached client when they
// change, so the client can mirror them.
func (s *Session) pollListeners() {
	var last []int
	for {
		select {
		case <-s.exited:
			return
		case <-time.After(3 * time.Second):
		}
		s.mu.Lock()
		c := s.cur
		s.mu.Unlock()
		if c == nil {
			continue
		}
		ports, err := Listeners()
		if err != nil {
			continue
		}
		sort.Ints(ports)
		if equalInts(ports, last) {
			continue
		}
		last = ports
		c.send(proto.Listeners, proto.EncodePorts(ports))
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns live sessions, pruning records whose session is gone.
func List() ([]Record, error) {
	entries, err := os.ReadDir(home.Sessions())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var r Record
		b, err := os.ReadFile(filepath.Join(home.Sessions(), e.Name()))
		if err != nil || json.Unmarshal(b, &r) != nil {
			continue
		}
		if syscall.Kill(r.Server, 0) != nil {
			// Its server died without cleaning up, so how it ended is unknown.
			if !hasRemains(r.ID) {
				leaveRemains(r, Ending{At: time.Now().UTC().Format(time.RFC3339), Lost: true})
			}
			os.Remove(filepath.Join(home.Sessions(), e.Name()))
			os.Remove(SocketPath(r.ID))
			os.Remove(BridgePath(r.ID))
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
	return out, nil
}

// Load returns one session record.
func Load(id string) (Record, error) {
	var r Record
	b, err := os.ReadFile(RecordPath(id))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// Attach relays frames between rw and the session until either side ends.
// This is the whole body of the SSH-side relay: a byte pump in each direction.
func Attach(id string, rw io.ReadWriter) error {
	conn, err := dial(id)
	if err != nil {
		return err
	}
	defer conn.Close()
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(conn, rw); errc <- err }()
	go func() { _, err := io.Copy(rw, conn); errc <- err }()
	<-errc
	return nil
}

// notRunning is the error for a session that is not running: it ended, or
// never existed. Callers tell it apart with errors.As, not by its words.
type notRunning string

func (id notRunning) Error() string { return "session " + string(id) + " is not running" }

// endedText is how a session that has just exited answers a request.
const endedText = "the session has ended"

// dial connects to a session's socket. Only a missing socket or a refused
// connection means the session is not running; any other failure, such as a
// full backlog or no free file descriptors, is reported as it is, since the
// session may well be alive.
func dial(id string) (net.Conn, error) {
	conn, err := net.Dial("unix", SocketPath(id))
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, notRunning(id)
	}
	if err != nil {
		return nil, fmt.Errorf("reach session %s: %w", id, err)
	}
	return conn, nil
}

// hasRemains reports whether session id ended and left its remains.
func hasRemains(id string) bool {
	_, err := os.Stat(remainsPath(id))
	return err == nil
}

// GoneText tells someone reaching for session id that it is gone, and how
// to see what it left if it left anything.
func GoneText(id string) string {
	if hasRemains(id) {
		return "session " + id + " has ended; aft attach " + id + " shows its last screen and how it ended"
	}
	return "session " + id + " is not running"
}

// Control opens a control connection to a session and sends one frame.
func Control(id string, t proto.Type) error {
	_, err := request(id, t)
	return err
}

// OfferUDP asks a session to listen on UDP and returns how to reach it.
func OfferUDP(id string) (udp.Offer, error) {
	f, err := request(id, proto.ReqUDP)
	var o udp.Offer
	if err == nil && f.Type == proto.Error {
		err = errors.New(string(f.Payload))
		if string(f.Payload) == endedText {
			err = notRunning(id) // it exited while being asked
		}
	}
	if err == nil {
		err = json.Unmarshal(f.Payload, &o)
	}
	return o, err
}

// request sends one control frame to a session and returns its answer.
func request(id string, t proto.Type) (proto.Frame, error) {
	conn, err := dial(id)
	if err != nil {
		return proto.Frame{}, err
	}
	defer conn.Close()
	if err := proto.Write(conn, proto.Resize, proto.EncodeResize(0, 0)); err != nil {
		return proto.Frame{}, err
	}
	if err := proto.Write(conn, t, nil); err != nil {
		return proto.Frame{}, err
	}
	wait := 5 * time.Second
	if t == proto.ReqUDP {
		wait = time.Second // a session from an older server never answers
	}
	conn.SetReadDeadline(time.Now().Add(wait))
	return proto.Read(bufio.NewReader(conn))
}
