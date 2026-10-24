// Package udp carries a session's frames over QUIC on UDP, the way mosh does
// over its own protocol: no TCP stalls after a lost packet, and a connection
// that follows the client across networks. SSH stays the way in. The session
// opens a listener on request and tells the client, over SSH, the port, its
// certificate's fingerprint and a token; the client then dials the host
// directly, pins the certificate and presents the token. Nothing else is
// accepted. When UDP is blocked the client keeps using SSH.
package udp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// Ports is the range a session listens in: mosh's, so a firewall opened for
// mosh also lets aft through.
const (
	firstPort = 60000
	lastPort  = 61000
)

// alpn names aft's protocol in the TLS handshake.
const alpn = "aft/1"

// Offer is what a session tells the client over SSH so it can dial directly.
type Offer struct {
	Port  int    `json:"port"`
	Cert  string `json:"cert"`  // SHA-256 of the session's certificate, hex
	Token string `json:"token"` // presented first on every stream, hex
}

func config() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 3 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
		KeepAlivePeriod:      5 * time.Second,
	}
}

// Listener accepts clients that present the session's token.
type Listener struct {
	ln       *quic.Listener
	token    []byte
	Offer    Offer
	admitted chan net.Conn
	done     chan struct{} // closed by Close
	once     sync.Once
	open     sync.WaitGroup // connections admitted and not yet closed
}

// Listen opens a listener on a free port in the mosh range, with a fresh
// certificate and token.
func Listen() (*Listener, error) {
	cert, sum, err := selfSigned()
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	rand.Read(token)
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}, MinVersion: tls.VersionTLS13}
	for _, i := range mrand.Perm(lastPort - firstPort + 1) {
		port := firstPort + i
		ln, err := quic.ListenAddr(fmt.Sprintf(":%d", port), tlsConf, config())
		if err != nil {
			continue
		}
		l := &Listener{ln: ln, token: token, Offer: Offer{Port: port, Cert: sum, Token: hex.EncodeToString(token)}, admitted: make(chan net.Conn), done: make(chan struct{})}
		go l.serve()
		return l, nil
	}
	return nil, errors.New("no free UDP port in 60000-61000")
}

// serve checks each connection's token on its own, so one that never sends
// it cannot hold up the next. It ends when the listener closes.
func (l *Listener) serve() {
	// However accepting ends, Accept must report it rather than wait.
	defer l.Close()
	for {
		c, err := l.ln.Accept(context.Background())
		if err != nil {
			return
		}
		go func() {
			s, err := l.admit(c)
			if err != nil {
				c.CloseWithError(1, "not admitted")
				return
			}
			select {
			case l.admitted <- s:
			case <-c.Context().Done():
			case <-l.done:
				s.Close()
			}
		}()
	}
}

// Accept returns the next client stream whose token matched. Any connection
// that does not present the token within a few seconds is dropped.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case s := <-l.admitted:
		return s, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *Listener) admit(c *quic.Conn) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	got := make([]byte, len(l.token))
	s.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(s, got); err != nil || subtle.ConstantTimeCompare(got, l.token) != 1 {
		return nil, errors.New("bad token")
	}
	s.SetReadDeadline(time.Time{})
	l.open.Add(1)
	return &conn{Stream: s, c: c, closed: l.open.Done}, nil
}

// Drain waits, up to timeout, for every admitted connection to close, so
// what was last written to them, such as the frame saying the application
// exited, is delivered and acknowledged before the process ends. A lost
// datagram is retransmitted only while the process lives.
func (l *Listener) Drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() { l.open.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Close stops accepting; streams already open keep running.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.ln.Close()
}

// Dial connects to a session's listener on host and presents the token.
func Dial(host string, o Offer) (net.Conn, error) {
	token, err := hex.DecodeString(o.Token)
	if err != nil {
		return nil, err
	}
	tlsConf := &tls.Config{
		NextProtos: []string{alpn},
		MinVersion: tls.VersionTLS13,
		// The certificate is self-signed; trust comes from pinning the
		// fingerprint the session sent over SSH.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != o.Cert {
				return errors.New("certificate does not match the session's")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := quic.DialAddr(ctx, net.JoinHostPort(host, fmt.Sprint(o.Port)), tlsConf, config())
	if err != nil {
		return nil, err
	}
	s, err := c.OpenStreamSync(ctx)
	if err == nil {
		_, err = s.Write(token)
	}
	if err != nil {
		c.CloseWithError(0, "")
		return nil, err
	}
	return &conn{Stream: s, c: c}, nil
}

// conn makes one QUIC stream a net.Conn; closing it closes the connection.
type conn struct {
	*quic.Stream
	c      *quic.Conn
	closed func() // tells the listener the connection is gone, if it has one
}

func (c *conn) LocalAddr() net.Addr  { return c.c.LocalAddr() }
func (c *conn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }

// Close ends the stream so what was written still arrives, such as the frame
// saying the application exited, then closes the connection once the peer
// has hung up or a moment has passed. Closing the connection at once would
// discard data still in flight.
func (c *conn) Close() error {
	err := c.Stream.Close()
	go func() {
		select {
		case <-c.c.Context().Done():
		case <-time.After(3 * time.Second):
		}
		c.c.CloseWithError(0, "")
		if c.closed != nil {
			c.closed()
		}
	}()
	return err
}

func init() {
	// quic-go warns on stderr when it cannot enlarge the UDP buffers; in the
	// client that would land on the user's screen. The default size works.
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
}

// selfSigned makes a throwaway certificate and its SHA-256 fingerprint.
func selfSigned() (tls.Certificate, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, hex.EncodeToString(sum[:]), nil
}
