package tests

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/paulorf0/Ares/client"
	"github.com/paulorf0/Ares/messages"
	"github.com/paulorf0/Ares/server"
)

const (
	waitTimeout = 20 * time.Second
	waitStep    = 20 * time.Millisecond
)

// newSignalingServer starts an isolated signaling server on a random port. Every
// case gets its own hub, so nothing leaks between tests and no fixed port has to
// be free. Pass 0 to keep the default session timeout.
func newSignalingServer(t *testing.T, sessionTimeout time.Duration) string {
	t.Helper()

	hub := server.NewHub()
	if sessionTimeout > 0 {
		hub.SetSessionTimeout(sessionTimeout)
	}

	srv := httptest.NewServer(server.Handler(hub))
	t.Cleanup(srv.Close)

	// The scheme is the only difference: a websocket handshake is a plain HTTP
	// GET underneath.
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(waitStep)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newClient(t *testing.T, signalURL, room string) *client.Client {
	t.Helper()

	c, err := client.New(signalURL, room)
	if err != nil {
		t.Fatalf("connect to room %q: %v", room, err)
	}
	t.Cleanup(func() { c.Close() })

	return c
}

// connectedPair brings up two peers in the same room and waits for the WebRTC
// connection to be established on both sides.
func connectedPair(t *testing.T, room string) (*client.Client, *client.Client) {
	t.Helper()

	signalURL := newSignalingServer(t, 0)
	a := newClient(t, signalURL, room)
	b := newClient(t, signalURL, room)

	waitFor(t, "both peers to reach connected", func() bool {
		return a.ConnectionState() == webrtc.PeerConnectionStateConnected &&
			b.ConnectionState() == webrtc.PeerConnectionStateConnected
	})

	return a, b
}

func waitForOpenChannels(t *testing.T, peers ...*client.Client) {
	t.Helper()

	waitFor(t, "data channels to open", func() bool {
		for _, p := range peers {
			dc := p.DataChannel()
			if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
				return false
			}
		}
		return true
	})
}

// Room occupancy is transient by design: once a pair finishes the handshake both
// clients tear signaling down (D6) and the server frees their slots, even though
// the P2P connection is alive. Raw websocket connections never negotiate
// anything, so they hold their slots until closed, which is what makes occupancy
// observable. The role the server hands out reveals it: a peer told it is
// impolite found the room empty.

func joinRaw(signalURL, room string) (*websocket.Conn, bool, error) {
	conn, _, err := websocket.DefaultDialer.Dial(signalURL+"?room="+room, nil)
	if err != nil {
		return nil, false, err
	}

	var role messages.RoleMessage
	if err := conn.ReadJSON(&role); err != nil {
		conn.Close()
		return nil, false, err
	}

	return conn, role.Payload.Polite, nil
}

func mustJoinRaw(t *testing.T, signalURL, room string) (*websocket.Conn, bool) {
	t.Helper()

	conn, polite, err := joinRaw(signalURL, room)
	if err != nil {
		t.Fatalf("join room %q: %v", room, err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn, polite
}

// waitForRawJoin retries until the room accepts a peer, then reports whether it
// was told it is polite. Closing a connection and the server noticing it are not
// the same instant.
func waitForRawJoin(t *testing.T, signalURL, room string) bool {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		conn, polite, err := joinRaw(signalURL, room)
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			return polite
		}
		time.Sleep(waitStep)
	}

	t.Fatalf("room %q never accepted a new peer", room)
	return false
}

// waitForEmptyRoom polls until a newcomer is told it is impolite, meaning every
// slot came back.
func waitForEmptyRoom(t *testing.T, signalURL, room string) {
	t.Helper()

	waitFor(t, "room "+room+" to be released", func() bool {
		conn, polite, err := joinRaw(signalURL, room)
		if err != nil {
			return false
		}
		defer conn.Close()
		return !polite
	})
}
