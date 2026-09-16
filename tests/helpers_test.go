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

// newSignalingServer starts an isolated signaling server on a random port, so
// cases share no state and need no fixed port. Pass 0 for the default timeout.
func newSignalingServer(t *testing.T, sessionTimeout time.Duration) string {
	t.Helper()

	hub := server.NewHub()
	if sessionTimeout > 0 {
		hub.SetSessionTimeout(sessionTimeout)
	}

	srv := httptest.NewServer(server.Handler(hub))
	t.Cleanup(srv.Close)

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

	c, err := client.New(signalURL, room, "test-id", "Tester")
	if err != nil {
		t.Fatalf("connect to room %q: %v", room, err)
	}
	t.Cleanup(func() { c.Close() })

	return c
}

// connectedPair brings up two peers in the same room and waits for both to
// reach connected.
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

// Room occupancy is transient: a pair that finishes the handshake drops
// signaling and frees both slots. Raw websocket connections negotiate nothing
// and hold their slots until closed, which keeps occupancy observable. The role
// the server hands out reveals it, since an impolite peer found the room empty.

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
// was told it is polite. The server does not notice a close instantly.
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

// waitForEmptyRoom polls until a newcomer is told it is impolite, meaning both
// slots came back.
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
