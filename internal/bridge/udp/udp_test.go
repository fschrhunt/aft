package udp

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestOnlyTheOfferedClientGetsIn connects with the offer and echoes bytes, and
// refuses a client with the wrong token or a server with the wrong certificate.
func TestOnlyTheOfferedClientGetsIn(t *testing.T) {
	l, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()

	c, err := Dial("127.0.0.1", l.Offer)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	got := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo: %q %v", got, err)
	}
	c.Close()

	bad := l.Offer
	bad.Token = strings.Repeat("00", 32)
	if c, err := Dial("127.0.0.1", bad); err == nil {
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("ping"))
		if _, err := io.ReadFull(c, got); err == nil {
			t.Fatal("a wrong token was admitted")
		}
	}

	bad = l.Offer
	bad.Cert = strings.Repeat("ab", 32)
	if _, err := Dial("127.0.0.1", bad); err == nil {
		t.Fatal("a certificate that does not match the offer was trusted")
	}
}

// TestSilentClientDoesNotBlockOthers opens a connection that never sends its
// token, then a real client: the real one is admitted at once.
func TestSilentClientDoesNotBlockOthers(t *testing.T) {
	l, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	silent := l.Offer
	silent.Token = "" // completes the handshake, never sends a token
	go Dial("127.0.0.1", silent)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	go func() {
		if c, err := Dial("127.0.0.1", l.Offer); err == nil {
			c.Write([]byte("hi"))
		}
	}()
	if _, err := l.Accept(); err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("real client admitted after %v: %v", time.Since(start), err)
	}
}

// TestDrainWaitsForTheClientToHangUp keeps the listener's side alive until an
// admitted connection is closed by both ends, so the last frames reach the
// client, and gives up after its timeout when the client never hangs up.
func TestDrainWaitsForTheClientToHangUp(t *testing.T) {
	l, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := Dial("127.0.0.1", l.Offer)
	if err != nil {
		t.Fatal(err)
	}
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	server.Write([]byte("bye"))
	server.Close()
	start := time.Now()
	l.Drain(5 * time.Second) // the client has not hung up: this waits the full 3 s close grace
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("drain returned after %v without the client hanging up", d)
	}
	got := make([]byte, 3)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "bye" {
		t.Fatalf("the last frame was lost: %q %v", got, err)
	}
	client.Close()
}
