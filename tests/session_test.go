package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/paulorf0/Ares/client"
)

// Client-side behaviour: the handshake, the data channel, and how failures
// surface to the caller.

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

	// The label is chosen by the offering side and travels in-band, never
	// through the signaling server.
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

// Signaling is only needed for the initial handshake.
func TestSignalingClosesOnceDataChannelOpens(t *testing.T) {
	a, b := connectedPair(t, "teardown")
	waitForOpenChannels(t, a, b)

	waitFor(t, "signaling to be torn down", func() bool {
		return a.SignalingClosed() && b.SignalingClosed()
	})
}

// A full room is upgraded first and closed right after, so a successful dial is
// not a valid session. The failure surfaces on the role read.
func TestThirdPeerIsRejectedBeforeGettingARole(t *testing.T) {
	signalURL := newSignalingServer(t, 0)
	mustJoinRaw(t, signalURL, "full")
	mustJoinRaw(t, signalURL, "full")

	third, err := client.New(signalURL, "full", "3", "Third")
	if err == nil {
		third.Close()
		t.Fatal("a third peer was allowed into the room")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error = %v, want it to point at the role message", err)
	}
}
