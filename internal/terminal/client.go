// Package terminal is the part of Aft on the user's machine, one of its three
// parts beside the session and the bridge. It runs in the user's terminal,
// attaches to a session on a server, passes the application's output through
// to the real terminal, predicts typed characters so typing feels local, and
// reconnects when the connection drops.
package terminal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/arocomputer/aft/internal/bridge"
	"github.com/arocomputer/aft/internal/bridge/ports"
	"github.com/arocomputer/aft/internal/bridge/proto"
	"github.com/arocomputer/aft/internal/bridge/transport"
	"github.com/arocomputer/aft/internal/bridge/udp"
	"github.com/arocomputer/aft/internal/home"
	sess "github.com/arocomputer/aft/internal/session"
	"github.com/arocomputer/aft/internal/session/install"
	"github.com/arocomputer/aft/internal/ui"
	"github.com/arocomputer/aft/internal/vt"
)

// Options describe what to run or attach to.
type Options struct {
	Host      transport.Host
	SessionID string   // attach to this session; empty starts a new one
	Command   []string // for a new session
	Dir       string   // working directory on the server
	Env       []string // KEY=VALUE pairs to pass through
	Wrapper   []string // optional command to run the agent inside
	Predict   bool
	UDP       bool // reach the session over UDP when the host allows it
	// ServerPath names a local file on the host when it is already there,
	// through a mounted folder; a pasted file it names is not uploaded.
	ServerPath func(local string) (string, bool)
	Ports      ports.Range
	DetachKey  byte // 0 disables
	Prefix     byte // 0 disables; opens copy mode and detach
	LatencyMS  int  // simulated one-way delay each direction, for the selftest
	Quiet      bool
}

// Run attaches the terminal to a session and returns the process exit code.
// saneSize defaults a terminal size to 80x24 when the terminal reports an error
// or a zero dimension. A 0x0 size would otherwise be sent as a resize the
// session reads as a control connection, so the session would never attach.
func saneSize(cols, rows int, err error) (int, int) {
	if err != nil || cols <= 0 || rows <= 0 {
		return 80, 24
	}
	return cols, rows
}

func Run(o Options) int {
	// Advanced/testing env overrides: AFT_LATENCY_MS simulates a slow link each
	// direction, and AFT_PREDICT=0 turns prediction off, so you can feel the
	// difference without a remote server.
	if o.LatencyMS == 0 {
		if n, err := strconv.Atoi(os.Getenv("AFT_LATENCY_MS")); err == nil && n > 0 {
			o.LatencyMS = n
		}
	}
	if v := os.Getenv("AFT_PREDICT"); v == "0" || v == "false" {
		o.Predict = false
	}
	in, out := os.Stdin, os.Stdout
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		ui.Error("a terminal is required")
		return 2
	}
	if err := transport.EnsureMaster(o.Host); err != nil {
		ui.Error("%v", err)
		return 1
	}
	if _, err := install.Ensure(o.Host); err != nil {
		ui.Error("%v", err)
		return 1
	}
	cols, rows := saneSize(term.GetSize(int(out.Fd())))

	c := &session{o: o, in: in, out: out, cols: cols, rows: rows, id: o.SessionID, command: o.Command}
	c.outbound = make(chan []byte, 4096)
	c.uploads = make(chan uploadResult, 1)
	c.unflash = make(chan struct{}, 1)
	c.sendDone = make(chan struct{})
	defer close(c.sendDone)
	go c.writer()
	c.mirror = ports.New(o.Host, o.Ports)
	c.pred = NewPredictor(cols, rows, out, o.Predict)
	// A copy made by the agent on the server (OSC 52) sets the laptop clipboard,
	// even when the terminal itself blocks OSC 52.
	c.pred.OnClipboard(func(data []byte) { go bridge.SetClipboard(data) })
	defer c.mirror.Close()
	defer c.clearMirrors()

	proc, err := c.open(true)
	var gone sessionEnded
	if errors.As(err, &gone) {
		c.goneMessage(gone)()
		return 1
	}
	if err != nil {
		ui.Error("%v", err)
		return 1
	}
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		ui.Error("%v", err)
		return 1
	}
	restore := func() { term.Restore(int(in.Fd()), state) }
	defer restore()
	// If the terminal window closes (SIGHUP) or we are killed (SIGTERM), restore
	// the terminal before exiting so it is never left in raw mode. The session
	// keeps running on the server and can be reattached.
	death := make(chan os.Signal, 1)
	signal.Notify(death, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		<-death
		restore()
		os.Exit(1)
	}()
	code := c.loop(proc)
	restore()
	if c.message != nil {
		c.message()
	}
	return code
}

type session struct {
	o          Options
	in, out    *os.File
	cols, rows int
	id         string
	pred       *Predictor
	mirror     *ports.Mirror
	message    func() // prints why the client left, after the terminal is restored

	command []string      // what the session runs, for the leaving message
	unflash chan struct{} // a flashed notice is due to be taken down

	cancelled    bool               // Ctrl-C cancelled the upload in progress
	noticeUp     bool               // a notice covers the top row
	cancelUpload context.CancelFunc // stops the paste upload in progress
	uploadCtx    context.Context    // done once the upload has finished or stopped

	offer      *udp.Offer // how to reach the session over UDP, once it offered
	udpAddr    string     // the host's address for UDP
	udpBlocked bool       // UDP failed after an offer: stay on SSH

	wmu sync.Mutex
	wr  io.Writer
	rd  io.Reader

	z     Tokenizer        // input tokenizer, kept across the loop
	flush <-chan time.Time // fires to release a lone Escape held by the tokenizer

	outbound chan []byte   // frames queued for the writer goroutine
	sendDone chan struct{} // closed on exit to stop the writer

	copy          *copyMode // non-nil while the scrollback pager is open
	prefixPending bool      // the prefix key was pressed; the next key is a command
	awaitSnap     bool      // a snapshot was requested to open copy mode

	holding  bool              // a pasted file is uploading; input is held
	held     []Token           // keys typed during the upload, sent after it
	uploads  chan uploadResult // finished paste uploads (paste.go)
	pastedAt time.Time         // last paste or paste key; opens clipboard reads
}

// link is one connection to the session: a relay over SSH, or a QUIC stream
// over UDP. Wait ends it and returns what the far side said about why.
type link interface {
	io.Reader
	io.Writer
	Close()
	Wait() string
}

// sshLink is the session relayed through an ssh process: Close kills the
// process, and Wait reaps it and returns what the relay wrote to stderr, which
// is how a failure to start a session is explained.
type sshLink struct{ p *transport.Proc }

func (l sshLink) Read(b []byte) (int, error)  { return l.p.Stdout.Read(b) }
func (l sshLink) Write(b []byte) (int, error) { return l.p.Stdin.Write(b) }
func (l sshLink) Close()                      { l.p.Kill() }
func (l sshLink) Wait() string                { msg, _ := l.p.Wait(); return msg }

// udpLink is the session reached directly over QUIC; Wait just closes it, as a
// dropped connection has nothing to say.
type udpLink struct{ net.Conn }

func (l udpLink) Close()       { l.Conn.Close() }
func (l udpLink) Wait() string { l.Conn.Close(); return "" }

// open starts or attaches to the session and performs the handshake.
func (c *session) open(first bool) (link, error) {
	l, err := c.dial()
	if err != nil {
		return nil, err
	}
	return l, c.use(l)
}

// use makes l the connection the writer and reader use, and sends the
// handshake.
func (c *session) use(l link) error {
	c.wmu.Lock()
	c.wr, c.rd = l, l
	if d := time.Duration(c.o.LatencyMS) * time.Millisecond / 2; d > 0 {
		c.wr = laggyWriter{l, d}
		c.rd = laggyReader(l, d)
	}
	c.wmu.Unlock()
	if err := c.send(proto.Resize, proto.EncodeResize(c.cols, c.rows)); err != nil {
		l.Close()
		return err
	}
	return nil
}

// dial connects to the session over UDP when it can, else over SSH. It asks
// the server over SSH to open the session's UDP listener, starting the session
// if needed, then dials it. When UDP is blocked the client stops trying and
// stays on SSH. reconnect redials an offered listener without SSH.
func (c *session) dial() (link, error) {
	if c.o.UDP && !c.o.Host.IsLocal() && !c.udpBlocked {
		if addr, err := c.o.Host.Address(); err == nil {
			out, err := transport.Output(c.o.Host, nil, append([]string{"udp"}, c.sessionArgs()...)...)
			var got struct {
				ID string `json:"id"`
				udp.Offer
				Gone    bool `json:"gone"`    // the session is not running
				Remains bool `json:"remains"` // and it left its last screen
			}
			if err != nil && c.id == "" && !strings.Contains(err.Error(), "unknown server command") {
				return nil, fmt.Errorf("could not start the session: %w", err)
			}
			answered := err == nil && json.Unmarshal(out, &got) == nil
			if answered && got.Gone {
				return nil, sessionEnded{Remains: got.Remains}
			}
			if answered {
				c.id = got.ID
				if got.Port != 0 {
					c.offer, c.udpAddr = &got.Offer, addr
					if conn, err := udp.Dial(addr, got.Offer); err == nil {
						return udpLink{conn}, nil
					}
					// The host offered UDP and it did not get through: the
					// network blocks it, so stop asking for this client.
					c.udpBlocked = true
				}
			}
			// Otherwise the request itself failed (an older server, a slow
			// answer): use SSH this time and ask again on the next connect.
		}
	}
	args := []string{"attach", c.id}
	if c.id == "" {
		args = append([]string{"new"}, c.sessionArgs()...)
	}
	proc, err := transport.Server(c.o.Host, args...)
	if err != nil {
		return nil, err
	}
	return sshLink{proc}, nil
}

// sessionArgs names the session to attach to, or describes the one to start.
func (c *session) sessionArgs() []string {
	if c.id != "" {
		return []string{c.id}
	}
	args := []string{"--dir", c.o.Dir, "--cols", strconv.Itoa(c.cols), "--rows", strconv.Itoa(c.rows)}
	for _, kv := range c.o.Env {
		args = append(args, "--env", kv)
	}
	for _, w := range c.o.Wrapper {
		args = append(args, "--wrap", w)
	}
	return append(append(args, "--"), c.o.Command...)
}

// send queues a frame for the writer goroutine. It never writes to the network
// itself, so a slow or congested link cannot delay the input loop or the local
// prediction. Frames keep their order through the single queue and writer.
func (c *session) send(t proto.Type, payload []byte) error {
	var buf bytes.Buffer
	if err := proto.Write(&buf, t, payload); err != nil {
		return err
	}
	select {
	case c.outbound <- buf.Bytes():
	case <-c.sendDone:
	}
	return nil
}

// writer drains the outbound queue to the current connection. It is the only
// goroutine that writes to the network, so a blocking write (backpressure,
// the simulated latency) stalls only the queue, never typing.
func (c *session) writer() {
	for {
		select {
		case b := <-c.outbound:
			c.wmu.Lock()
			w := c.wr
			c.wmu.Unlock()
			if w != nil {
				w.Write(b)
			}
		case <-c.sendDone:
			return
		}
	}
}

func (c *session) title(s string) {
	c.out.Write([]byte("\x1b]2;" + s + "\x1b\\"))
}

type frameOrErr struct {
	f   proto.Frame
	err error
}

// loop runs until the application exits, the client detaches, or the user
// gives up on a reconnect.
func (c *session) loop(proc link) int {
	keys := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := c.in.Read(buf)
			if n > 0 {
				keys <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(keys)
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	// Heartbeats let the session tell a closed lid from a quiet terminal.
	beat := time.NewTicker(10 * time.Second)
	defer beat.Stop()

	connected := true
	frames := c.readFrames(proc)
	for {
		// Keystrokes take priority: drain every queued key before touching
		// output, so a burst of agent output never stalls your typing.
		if connected {
		drain:
			for {
				select {
				case b, ok := <-keys:
					if leave, code := c.handleBatch(b, ok, connected); leave {
						return code
					}
				default:
					break drain
				}
			}
		}
		select {
		case b, ok := <-keys:
			if leave, code := c.handleBatch(b, ok, connected); leave {
				return code
			}
		case <-c.flush:
			c.flush = nil
			if connected && !c.handleKeys(c.z.Flush()) {
				return 0
			}
		case <-winch:
			if cols, rows, err := term.GetSize(int(c.out.Fd())); err == nil {
				c.cols, c.rows = cols, rows
				c.pred.Resize(cols, rows)
				if c.copy != nil {
					c.copy.resize(cols, rows)
					c.out.Write(c.copy.view())
				}
				if connected {
					c.send(proto.Resize, proto.EncodeResize(cols, rows))
				}
			}
		case <-tick.C:
			c.pred.Tick()
		case <-c.unflash:
			if c.copy == nil && c.noticeUp {
				c.pred.RestoreRow(0)
				c.noticeUp = false
			}
		case <-beat.C:
			if connected {
				c.send(proto.Ping, nil)
			}
		case u := <-c.uploads:
			if !c.finishUpload(u) {
				return 0
			}
		case fe := <-frames:
			if fe.err != nil {
				// The connection dropped without the application ending.
				connected = false
				// Leave copy mode: its snapshot is stale and the reattach repaints.
				c.copy = nil
				c.awaitSnap = false
				msg := proc.Wait()
				if c.id == "" {
					c.message = func() { ui.Error("could not start the session: %s", msg) }
					return 1
				}
				c.title("aft · reconnecting")
				var typed []byte
				var err error
				proc, typed, err = c.reconnect(keys)
				var gone sessionEnded
				if errors.As(err, &gone) {
					c.pred.Repaint() // clear the reconnecting notice
					c.noticeUp = false
					c.message = c.goneMessage(gone)
					return 1
				}
				if errors.Is(err, errLeft) {
					c.pred.Repaint() // clear the reconnecting notice
					c.noticeUp = false
					c.message = c.detachMessage("left %s while disconnected; it keeps running")
					return 1
				}
				if err != nil {
					c.message = func() { ui.Error("%v", err) }
					return 1
				}
				connected = true
				frames = c.readFrames(proc)
				// Deliver what was typed while disconnected, in order.
				if len(typed) > 0 && !c.handleKeys(c.z.Feed(typed)) {
					return 0
				}
				continue
			}
			switch code, done := c.handleFrame(fe.f); {
			case done:
				proc.Wait()
				return code
			}
		}
	}
}

func (c *session) readFrames(proc link) <-chan frameOrErr {
	ch := make(chan frameOrErr, 256)
	rd := c.rd
	go func() {
		r := bufio.NewReaderSize(rd, 256<<10)
		for {
			f, err := proto.Read(r)
			if err != nil {
				ch <- frameOrErr{err: err}
				return
			}
			ch <- frameOrErr{f: f}
		}
	}()
	return ch
}

// handleKeys routes input: to copy mode when open, to a prefix command when the
// prefix was just pressed, otherwise to the application with prediction. While
// a pasted file uploads, keys are held and replayed after the paste, except
// Ctrl-C, which cancels the upload. It returns false when the client should
// leave (the user detached).
func (c *session) handleKeys(toks []Token) bool {
	for _, t := range toks {
		switch {
		case c.holding && len(t.Bytes) == 1 && t.Bytes[0] == 0x03 && (c.uploading() || c.cancelled):
			// Ctrl-C while a pasted file uploads cancels it; pressing it
			// again before the cancel lands is the same request, not a key
			// for the app.
			c.cancelUpload()
			c.cancelled = true
		case c.holding:
			c.held = append(c.held, t)
		case c.copy != nil:
			exit, copied := c.copy.key(t)
			if copied != nil {
				go bridge.SetClipboard(copied)
			}
			if exit {
				c.exitCopy()
			} else {
				c.out.Write(c.copy.view())
			}
		case c.prefixPending:
			c.prefixPending = false
			if !c.prefixCommand(t) {
				return false // prefix-d (detach) asks the loop to leave
			}
		case c.o.Prefix != 0 && len(t.Bytes) == 1 && t.Bytes[0] == c.o.Prefix:
			c.prefixPending = true
		case c.o.DetachKey != 0 && len(t.Bytes) == 1 && t.Bytes[0] == c.o.DetachKey:
			c.send(proto.Detach, nil)
			c.message = c.detachMessage("detached from %s; it keeps running")
			return false
		default:
			// Draw the local prediction first, so your echo is never delayed by
			// a slow or congested link, then forward the keystroke.
			c.pred.Key(t)
			c.notePaste(t)
			if t.Kind == TokPaste && c.startUpload(t) {
				continue
			}
			if err := c.send(proto.Input, t.Bytes); err != nil {
				return true
			}
		}
	}
	return true
}

// handleBatch tokenizes one read from the terminal and routes it. It returns
// leave=true with an exit code when the client should stop (stdin closed, or a
// detach). It is called both from the priority drain and the main select.
func (c *session) handleBatch(b []byte, ok, connected bool) (leave bool, code int) {
	if !ok {
		c.send(proto.Detach, nil)
		return true, 0
	}
	if !connected {
		// While disconnected, only Ctrl-C leaves; other keys are dropped.
		if len(b) == 1 && b[0] == 0x03 {
			c.message = c.detachMessage("left %s while disconnected; it keeps running")
			return true, 1
		}
		return false, 0
	}
	toks := c.z.Feed(b)
	if c.z.Pending() {
		c.flush = time.After(40 * time.Millisecond)
	} else {
		c.flush = nil
	}
	if !c.handleKeys(toks) {
		return true, 0
	}
	return false, 0
}

// prefixCommand runs the key after the prefix. It returns false only for detach,
// which asks the loop to leave.
func (c *session) prefixCommand(t Token) bool {
	switch {
	case len(t.Bytes) == 1 && t.Bytes[0] == c.o.Prefix:
		// The prefix twice sends the literal key to the application.
		c.pred.Key(t)
		c.send(proto.Input, t.Bytes)
	case string(t.Bytes) == "[":
		c.enterCopy()
	case string(t.Bytes) == "d":
		c.send(proto.Detach, nil)
		c.message = c.detachMessage("detached from %s; it keeps running")
		return false
	}
	return true
}

// exitMessage says the session is over, the way ssh says a connection
// closed: what ended, where, and how, if it failed.
func (c *session) exitMessage(e proto.ExitInfo) func() {
	what := "the program"
	switch {
	case sess.IsShell(c.command):
		what = "the shell" // aft HOST's login shell
	case len(c.command) > 0:
		what = filepath.Base(c.command[0])
	}
	where := c.o.Host.Name
	if c.o.Host.IsLocal() {
		where = "this machine"
	}
	return func() {
		switch {
		case e.Signal != "":
			ui.Warn("%s on %s ended by signal %s", what, where, e.Signal)
		case e.Code != 0:
			ui.Warn("%s on %s exited with code %d", what, where, e.Code)
		default:
			ui.Note("%s on %s exited; session %s is closed", what, where, c.id)
		}
	}
}

// detachMessage says the session runs on without this terminal, and how to
// come back to it. format names the session with %s.
func (c *session) detachMessage(format string) func() {
	id := c.id
	return func() {
		ui.Note(format, id)
		ui.Hint("aft attach %s", id)
	}
}

// enterCopy requests a snapshot; copy mode opens when it arrives.
func (c *session) enterCopy() {
	if c.awaitSnap || c.copy != nil {
		return
	}
	c.awaitSnap = true
	c.send(proto.ReqSnapshot, nil)
}

// exitCopy closes the pager and restores the live screen from the shadow.
func (c *session) exitCopy() {
	c.copy = nil
	c.pred.Repaint()
	c.noticeUp = false
}

// handleFrame applies one frame. done is true when the client should exit.
func (c *session) handleFrame(f proto.Frame) (code int, done bool) {
	switch f.Type {
	case proto.Output:
		if c.copy != nil {
			// The pager owns the screen; keep the shadow current for the repaint
			// on exit, but do not draw the live output over the pager.
			c.pred.Absorb(f.Payload)
			return 0, false
		}
		if err := c.pred.Output(f.Payload); err != nil {
			return 1, true
		}
	case proto.Snapshot:
		if c.awaitSnap {
			c.awaitSnap = false
			var sd vt.SnapshotData
			if gob.NewDecoder(bytes.NewReader(f.Payload)).Decode(&sd) == nil {
				c.copy = newCopyMode(sd, c.cols, c.rows)
				c.out.Write(c.copy.view())
			}
		}
	case proto.Hello:
		var h proto.HelloInfo
		if json.Unmarshal(f.Payload, &h) == nil {
			c.id = h.ID
			if len(h.Command) > 0 {
				c.command = h.Command
			}
		}
	case proto.Exit:
		var e proto.ExitInfo
		json.Unmarshal(f.Payload, &e)
		if !c.o.Quiet {
			c.message = c.exitMessage(e)
		}
		return e.Code, true
	case proto.Detached:
		if c.message == nil { // not our own detach: another terminal took the session
			c.message = c.detachMessage("detached from %s; another terminal took it, and it keeps running")
		}
		return 0, true
	case proto.Error:
		if c.noticeUp {
			// A reconnect may have drawn a notice that no repaint will
			// cover now; take it down before saying why we leave.
			c.pred.RestoreRow(0)
			c.noticeUp = false
		}
		msg := string(f.Payload)
		c.message = func() { ui.Error("%s", msg) }
		return 1, true
	case proto.BridgeReq:
		var req proto.BridgeRequest
		if json.Unmarshal(f.Payload, &req) == nil {
			ok := c.clipboardOK()
			go func() {
				resp := bridge.Handle(req, ok)
				b, _ := json.Marshal(resp)
				c.send(proto.BridgeReply, b)
			}()
		}
	case proto.Listeners:
		go func() { c.mirror.Update(proto.DecodePorts(f.Payload)); c.writeMirrors() }()
	case proto.Pong:
	}
	return 0, false
}

// reconnect reattaches with backoff. Keys typed meanwhile are kept and
// returned for delivery, except Ctrl-C, which leaves the session running and
// quits the client.
func (c *session) reconnect(keys <-chan []byte) (link, []byte, error) {
	delay := 500 * time.Millisecond
	var typed []byte
	lost := time.Now()
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			c.notice(fmt.Sprintf("aft - no contact with %s for %ds - reconnecting - Ctrl-C leaves, the session keeps running", c.o.Host.Name, int(time.Since(lost).Seconds())))
		}
		wait := time.After(delay)
	collect:
		for {
			select {
			case b, ok := <-keys:
				if !ok || (len(b) == 1 && b[0] == 0x03) {
					return nil, nil, errLeft
				}
				typed = append(typed, b...)
			case <-wait:
				break collect
			}
		}
		// A session reached over UDP is redialed directly first: no SSH round
		// trip, and it works the moment the new network does.
		if c.offer != nil && !c.udpBlocked {
			if conn, err := udp.Dial(c.udpAddr, *c.offer); err == nil {
				if l := (udpLink{conn}); c.use(l) == nil {
					c.pred.Reset()
					c.noticeUp = false // the reattach repaints over it
					c.title("")
					return l, typed, nil
				}
			}
		}
		if err := transport.EnsureMasterBatch(c.o.Host); err == nil {
			// A session that is gone says so: the UDP request answers
			// "gone" (see dial) and an SSH attach sends an Error frame, which
			// ends the client with the reason. No extra question is needed.
			proc, err := c.open(false)
			var gone sessionEnded
			if errors.As(err, &gone) {
				return nil, nil, gone
			}
			if err == nil {
				c.pred.Reset()
				c.noticeUp = false // the reattach repaints over it
				c.title("")
				return proc, typed, nil
			}
		}
		if delay < 5*time.Second {
			delay *= 2
		}
		if attempt%4 == 0 {
			c.title(fmt.Sprintf("aft · reconnecting (%d)", attempt))
		}
	}
}

// flash shows a notice across the top row for a few seconds, then restores
// the row; the input loop does the restoring when unflash fires.
func (c *session) flash(text string) {
	c.notice(text)
	time.AfterFunc(5*time.Second, func() {
		select {
		case c.unflash <- struct{}{}:
		default:
		}
	})
}

// sessionEnded is the client giving up because the session is gone, as the
// host answered: it ended unattended and left its remains, or it vanished.
type sessionEnded struct{ Remains bool }

func (sessionEnded) Error() string { return "the session ended" }

// goneMessage says the session is gone and, if it left its last screen, how
// to see it.
func (c *session) goneMessage(gone sessionEnded) func() {
	id, host := c.id, c.o.Host.Name
	return func() {
		ui.Warn("%s is no longer running on %s", id, host)
		if gone.Remains {
			ui.Hint("aft attach %s shows its last screen and how it ended", id)
		}
	}
}

// errLeft is reconnect giving up because the user left; the session runs on.
var errLeft = errors.New("left while disconnected")

// notice draws a one-line message over the top row, as mosh does, since a
// title is easy to miss: while the connection is down, where the repaint on
// reconnect draws over it, and through flash, which takes it down itself.
func (c *session) notice(text string) {
	c.pred.Notice(text)
	c.noticeUp = true
}

// writeMirrors records the currently mirrored ports so `aft status` can report
// them. It is keyed by session id and removed when the client exits.
func (c *session) writeMirrors() {
	if c.id == "" {
		return
	}
	active := c.mirror.Active()
	path := filepath.Join(home.Sockets(), "mirrors-"+c.id+".json")
	if len(active) == 0 {
		os.Remove(path)
		return
	}
	if b, err := json.Marshal(active); err == nil {
		os.WriteFile(path, b, 0o600)
	}
}

// clearMirrors removes this session's mirror record.
func (c *session) clearMirrors() {
	if c.id != "" {
		os.Remove(filepath.Join(home.Sockets(), "mirrors-"+c.id+".json"))
	}
}
