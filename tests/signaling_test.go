package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulorf0/Ares/client"
)

// Server-side behaviour: room capacity, slot bookkeeping, blind relaying and the
// session deadline. The peers here are mostly raw connections, since what is
// under test is the server, not WebRTC.

func TestRoomHoldsExactlyTwoPeers(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	if _, polite := mustJoinRaw(t, signalURL, "capacity"); polite {
		t.Error("the first peer should be impolite")
	}
	if _, polite := mustJoinRaw(t, signalURL, "capacity"); !polite {
		t.Error("the second peer should be polite")
	}

	if conn, _, err := joinRaw(signalURL, "capacity"); err == nil {
		conn.Close()
		t.Fatal("a third peer was let into a full room")
	}
}

// The server must never parse what it forwards: that is what lets audio and
// video be added later without touching it (D6).
func TestRelayForwardsMessagesBlindly(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	first, _ := mustJoinRaw(t, signalURL, "relay")
	second, _ := mustJoinRaw(t, signalURL, "relay")

	// Deliberately not valid SDP: the server has no business noticing.
	sent := []byte(`{"type":"sdp","payload":{"sdp":"not really an offer"}}`)
	if err := first.WriteMessage(websocket.TextMessage, sent); err != nil {
		t.Fatalf("send from the first peer: %v", err)
	}

	// joinRaw already consumed the role message, so the relay is next in line.
	_, received, err := second.ReadMessage()
	if err != nil {
		t.Fatalf("read on the second peer: %v", err)
	}
	if string(received) != string(sent) {
		t.Errorf("received %q, want the bytes to come through untouched (%q)", received, sent)
	}
}

func TestSlotIsFreedWhenOnePeerLeaves(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	first, _ := mustJoinRaw(t, signalURL, "slots")
	mustJoinRaw(t, signalURL, "slots")

	first.Close()

	// Polite means the newcomer found the remaining peer: the slot was freed
	// without evicting anyone else.
	if !waitForRawJoin(t, signalURL, "slots") {
		t.Error("the rejoining peer should have found the remaining one")
	}
}

func TestRoomIsReleasedWhenBothPeersLeave(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	first, _ := mustJoinRaw(t, signalURL, "release")
	second, _ := mustJoinRaw(t, signalURL, "release")

	first.Close()
	second.Close()

	waitForEmptyRoom(t, signalURL, "release")
}

// D6 in reverse: finishing the handshake is what releases the room, because both
// clients drop signaling as soon as the data channel opens.
func TestRoomIsReleasedOnceTheHandshakeCompletes(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	a := newClient(t, signalURL, "handoff")
	b := newClient(t, signalURL, "handoff")
	waitForOpenChannels(t, a, b)

	waitForEmptyRoom(t, signalURL, "handoff")
}

// A peer disconnecting while the other is still relaying used to crash the
// server with "send on closed channel". Churn plus traffic is what exposed it.
func TestPeerChurnDoesNotBreakTheServer(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	for i := 0; i < 20; i++ {
		a, err := client.New(signalURL, "churn")
		if err != nil {
			t.Fatalf("iteration %d, first peer: %v", i, err)
		}
		b, err := client.New(signalURL, "churn")
		if err != nil {
			t.Fatalf("iteration %d, second peer: %v", i, err)
		}

		// Tear down mid-handshake, while candidates are still being relayed.
		a.Close()
		b.Close()
	}

	// The server has to still be serving, and the room free, after all that.
	if waitForRawJoin(t, signalURL, "churn") {
		t.Error("the room should be empty after the churn")
	}
}

func TestEmptyRoomCodeIsRejectedAtHandshake(t *testing.T) {
	signalURL := newSignalingServer(t, 0)

	c, err := client.New(signalURL, "")
	if err == nil {
		c.Close()
		t.Fatal("an empty room code was accepted")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want the server's 400 status", err)
	}
}

// D10: a peer whose network dies never sends a close frame, so its slot would be
// held until TCP gives up. The absolute deadline is what releases it.
func TestStalePeerIsDroppedWhenTheSessionTimesOut(t *testing.T) {
	signalURL := newSignalingServer(t, 200*time.Millisecond)

	// One peer alone: nobody comes to meet it, so the handshake never starts and
	// the connection just sits there holding a slot.
	stale := newClient(t, signalURL, "stale")
	if stale.Polite() {
		t.Error("a peer alone in a room should be impolite")
	}

	waitForEmptyRoom(t, signalURL, "stale")
}

// The deadline must not cut a handshake that is simply taking its time.
func TestSessionTimeoutDoesNotCutAHealthyHandshake(t *testing.T) {
	signalURL := newSignalingServer(t, 10*time.Second)

	a := newClient(t, signalURL, "healthy")
	b := newClient(t, signalURL, "healthy")

	waitForOpenChannels(t, a, b)
}
