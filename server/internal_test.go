package server

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// These cover internals that have no exported surface by design: room slot
// bookkeeping, the send-after-close guard and the ICE buffer. Behavioural tests
// live in the tests package.

func TestHubFreesTheSlotWhenAPeerLeaves(t *testing.T) {
	hub := NewHub()

	first := newPeer(nil, "room")
	room, other := hub.join("room", first)
	if room == nil {
		t.Fatal("first peer was refused")
	}
	if other != nil {
		t.Error("first peer should find an empty room")
	}

	second := newPeer(nil, "room")
	if _, other = hub.join("room", second); other != first {
		t.Error("second peer should find the first one waiting")
	}

	if full, _ := hub.join("room", newPeer(nil, "room")); full != nil {
		t.Error("a third peer was let into a full room")
	}

	if remaining := hub.leave(first); remaining != second {
		t.Errorf("leave returned %v, want the remaining peer", remaining)
	}

	// The freed slot has to be reusable, otherwise a peer that drops locks the
	// room code forever.
	third := newPeer(nil, "room")
	reused, other := hub.join("room", third)
	if reused == nil {
		t.Fatal("the freed slot was not reused")
	}
	if other != second {
		t.Error("the rejoining peer should find the remaining one")
	}
	if !third.polite {
		t.Error("a peer joining an occupied room should be polite")
	}
}

func TestHubDropsEmptyRooms(t *testing.T) {
	hub := NewHub()

	first := newPeer(nil, "room")
	second := newPeer(nil, "room")
	hub.join("room", first)
	hub.join("room", second)

	hub.leave(first)
	if len(hub.rooms) != 1 {
		t.Fatalf("room dropped while still occupied: %d rooms", len(hub.rooms))
	}

	hub.leave(second)
	if len(hub.rooms) != 0 {
		t.Errorf("empty room kept in the hub: %d rooms", len(hub.rooms))
	}
}

// enqueue must stay safe once a peer is gone: the other peer's readPump keeps
// relaying for as long as it has not noticed the disconnect. Closing the send
// channel instead of selecting on done is what used to panic here.
func TestEnqueueToAClosedPeerDoesNotPanic(t *testing.T) {
	p := newPeer(nil, "room")
	p.closeOnce.Do(func() { close(p.done) }) // conn is nil, so skip closing it

	for i := 0; i < sendBuffer*2; i++ {
		p.enqueue([]byte("late relay"))
	}
}

// pion rejects candidates that arrive before the remote description, so the
// client has to hold them. This is what keeps the handshake from breaking on
// slow links, where candidates overtake the answer.
func TestEarlyICECandidatesAreBuffered(t *testing.T) {
	c := &Client{closed: make(chan struct{})}
	if err := c.createPeer(); err != nil {
		t.Fatalf("create peer: %v", err)
	}
	t.Cleanup(func() { c.connRTC.Close() })

	candidate := webrtc.ICECandidateInit{
		Candidate: "candidate:1 1 udp 2130706431 127.0.0.1 50000 typ host",
	}

	if err := c.handleRemoteCandidate(candidate); err != nil {
		t.Fatalf("buffering an early candidate should not fail: %v", err)
	}
	if len(c.pendingICE) != 1 {
		t.Fatalf("pendingICE holds %d candidates, want 1", len(c.pendingICE))
	}

	// Proves the buffer is load-bearing and not defensive decoration.
	if err := c.connRTC.AddICECandidate(candidate); err == nil {
		t.Error("expected pion to reject a candidate while the remote description is nil")
	}
}

func TestSendingWithoutASocketIsReported(t *testing.T) {
	c := &Client{closed: make(chan struct{})}

	if err := c.sendEnvelope("whatever", struct{}{}); !errors.Is(err, ErrNoWebSocketConnection) {
		t.Errorf("error = %v, want %v", err, ErrNoWebSocketConnection)
	}
}

// After the data channel opens the client tears signaling down (D6), and late
// ICE candidates still land here. They must report the teardown rather than
// write to a dead socket.
func TestSendingAfterTeardownReportsClosedSignaling(t *testing.T) {
	srv := httptest.NewServer(Handler(NewHub()))
	t.Cleanup(srv.Close)

	c, err := NewClient("ws"+strings.TrimPrefix(srv.URL, "http"), "teardown")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	c.closeSignaling()

	if err := c.sendEnvelope("whatever", struct{}{}); !errors.Is(err, ErrSignalingClosed) {
		t.Errorf("error = %v, want %v", err, ErrSignalingClosed)
	}
}
