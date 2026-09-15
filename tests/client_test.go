package tests

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/paulorf0/Ares/server"
)

const (
	waitTimeout = 20 * time.Second
	waitStep    = 20 * time.Millisecond
)

// newSignalingServer starts an isolated signaling server on a random port,
// instead of the fixed :8080 of server.Server(). Each case gets a fresh hub and
// nothing collides with TestMain's server.
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

func newClient(t *testing.T, signalURL, room string) *server.Client {
	t.Helper()

	c, err := server.NewClient(signalURL, room)
	if err != nil {
		t.Fatalf("connect to room %q: %v", room, err)
	}
	t.Cleanup(func() { c.Close() })

	return c
}

// connectedPair brings up two peers in the same room and waits for the WebRTC
// connection to be established on both sides.
func connectedPair(t *testing.T, room string) (*server.Client, *server.Client) {
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

func waitForOpenChannels(t *testing.T, peers ...*server.Client) {
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

func TestFirstPeerIsImpoliteSecondIsPolite(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	a := newClient(t, signalURL, "roles")
	if a.Polite() {
		t.Error("the peer that creates the room should be impolite")
	}

	b := newClient(t, signalURL, "roles")
	if !b.Polite() {
		t.Error("the peer that joins should be polite")
	}
}

func TestHandshakeOpensDataChannelOnBothSides(t *testing.T) {
	a, b := connectedPair(t, "handshake")
	waitForOpenChannels(t, a, b)

	// The label is chosen by the offering side and reaches the other one
	// in-band over DCEP, never through the signaling server.
	const want = "CHANNEL-handshake"
	if got := a.DataChannel().Label(); got != want {
		t.Errorf("offering side label = %q, want %q", got, want)
	}
	if got := b.DataChannel().Label(); got != want {
		t.Errorf("answering side label = %q, want %q", got, want)
	}
}

func TestDataChannelCarriesTextBothWays(t *testing.T) {
	a, b := connectedPair(t, "chat")
	waitForOpenChannels(t, a, b)

	atB := make(chan string, 1)
	atA := make(chan string, 1)
	b.DataChannel().OnMessage(func(msg webrtc.DataChannelMessage) { atB <- string(msg.Data) })
	a.DataChannel().OnMessage(func(msg webrtc.DataChannelMessage) { atA <- string(msg.Data) })

	receive := func(t *testing.T, from string, ch <-chan string) string {
		t.Helper()
		select {
		case got := <-ch:
			return got
		case <-time.After(waitTimeout):
			t.Fatalf("no message received from %s", from)
			return ""
		}
	}

	if err := a.DataChannel().SendText("ping"); err != nil {
		t.Fatalf("send from offering side: %v", err)
	}
	if got := receive(t, "the offering side", atB); got != "ping" {
		t.Errorf("received %q, want %q", got, "ping")
	}

	if err := b.DataChannel().SendText("pong"); err != nil {
		t.Fatalf("send from answering side: %v", err)
	}
	if got := receive(t, "the answering side", atA); got != "pong" {
		t.Errorf("received %q, want %q", got, "pong")
	}
}

// D6: the signaling server is only needed for the initial handshake.
func TestSignalingClosesOnceDataChannelOpens(t *testing.T) {
	a, b := connectedPair(t, "teardown")
	waitForOpenChannels(t, a, b)

	waitFor(t, "signaling to be torn down", func() bool {
		return a.SignalingClosed() && b.SignalingClosed()
	})
}

func TestThirdPeerIsRejectedBeforeGettingARole(t *testing.T) {
	signalURL := newSignalingServer(t, 0)
	newClient(t, signalURL, "full")
	newClient(t, signalURL, "full")

	// The upgrade succeeds and the server closes right after, so the failure
	// surfaces on the role read rather than on the dial.
	third, err := server.NewClient(signalURL, "full")
	if err == nil {
		third.Close()
		t.Fatal("a third peer was allowed into the room")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error = %v, want it to point at the role message", err)
	}
}

func TestEmptyRoomCodeIsRejectedAtHandshake(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	c, err := server.NewClient(signalURL, "")
	if err == nil {
		c.Close()
		t.Fatal("an empty room code was accepted")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want the server's 400 status", err)
	}
}

func TestRoomCodeIsReusableAfterBothPeersLeave(t *testing.T) {
	signalURL := newSignalingServer(t, 0)
	const room = "reuse"

	first, err := server.NewClient(signalURL, room)
	if err != nil {
		t.Fatalf("first peer: %v", err)
	}
	second, err := server.NewClient(signalURL, room)
	if err != nil {
		t.Fatalf("second peer: %v", err)
	}

	first.Close()
	second.Close()

	waitFor(t, "the room to be released", func() bool { return roomIsEmpty(signalURL, room) })
}

// A peer whose network dies never sends a close frame, so the slot would be held
// until TCP gives up. The absolute deadline (D10) is what releases it.
func TestStalePeerIsDroppedWhenTheSessionTimesOut(t *testing.T) {
	signalURL := newSignalingServer(t, 200*time.Millisecond)

	// One peer alone: nobody comes to meet it, so the handshake never starts
	// and the connection just sits there holding a slot.
	stale := newClient(t, signalURL, "stale")
	if stale.Polite() {
		t.Error("a peer alone in a room should be impolite")
	}

	waitFor(t, "the stale peer's slot to be released", func() bool {
		return roomIsEmpty(signalURL, "stale")
	})
}

// The deadline must not cut a handshake that is simply taking its time.
func TestSessionTimeoutDoesNotCutAHealthyHandshake(t *testing.T) {
	signalURL := newSignalingServer(t, 10*time.Second)

	a := newClient(t, signalURL, "healthy")
	b := newClient(t, signalURL, "healthy")

	waitForOpenChannels(t, a, b)
}

// roomIsEmpty probes the room from the outside: a peer that finds it empty is
// told it is impolite, so the role the server hands out reveals the occupancy
// without reaching into the hub.
func roomIsEmpty(signalURL, room string) bool {
	probe, err := server.NewClient(signalURL, room)
	if err != nil {
		return false
	}
	defer probe.Close()

	return !probe.Polite()
}
