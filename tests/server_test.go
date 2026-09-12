package tests

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulorf0/Ares/server"
)

const addr = "127.0.0.1:8080"

func TestMain(m *testing.M) {
	go server.Server()
	waitForServer()
	m.Run()
}

func waitForServer() {
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	panic("server did not start on " + addr)
}

func dial(t *testing.T, room string) *websocket.Conn {
	t.Helper()
	url := fmt.Sprintf("ws://%s/ws?room=%s", addr, room)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", url, err)
	}
	return conn
}

type roleMessage struct {
	Type    string `json:"type"`
	Payload struct {
		Polite bool `json:"polite"`
	} `json:"payload"`
}

func readRole(t *testing.T, conn *websocket.Conn) roleMessage {
	t.Helper()
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read role message: %v", err)
	}
	var msg roleMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("role is not valid JSON: %v (%s)", err, data)
	}
	if msg.Type != "role" {
		t.Fatalf("expected type=role, got %q", msg.Type)
	}
	return msg
}

func TestJoinAssignsPoliteImpoliteRoles(t *testing.T) {
	connA := dial(t, "room-role")
	defer connA.Close()
	roleA := readRole(t, connA)

	connB := dial(t, "room-role")
	defer connB.Close()
	roleB := readRole(t, connB)

	if roleA.Payload.Polite {
		t.Errorf("first peer should be impolite (polite=false), got polite=%v", roleA.Payload.Polite)
	}
	if !roleB.Payload.Polite {
		t.Errorf("second peer should be polite (polite=true), got polite=%v", roleB.Payload.Polite)
	}
}

func TestRelayForwardsMessagesBlindly(t *testing.T) {
	connA := dial(t, "room-relay")
	defer connA.Close()
	readRole(t, connA)

	connB := dial(t, "room-relay")
	defer connB.Close()
	readRole(t, connB)

	sent := []byte(`{"type":"sdp","payload":{"sdp":"fake-offer"}}`)
	if err := connA.WriteMessage(websocket.TextMessage, sent); err != nil {
		t.Fatalf("failed to send from A: %v", err)
	}

	_, received, err := connB.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read on B: %v", err)
	}

	if string(received) != string(sent) {
		t.Errorf("B received %q, expected %q", received, sent)
	}
}

func TestFullRoomRejectsThirdPeer(t *testing.T) {
	connA := dial(t, "room-full")
	defer connA.Close()
	readRole(t, connA)

	connB := dial(t, "room-full")
	defer connB.Close()
	readRole(t, connB)

	connC := dial(t, "room-full")
	defer connC.Close()

	_, _, err := connC.ReadMessage()
	if err == nil {
		t.Fatal("expected an error/close when a third peer joins the room, but the connection stayed open")
	}

	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected *websocket.CloseError, got %T: %v", err, err)
	}
	if closeErr.Code != websocket.CloseTryAgainLater {
		t.Errorf("expected close code %d, got %d", websocket.CloseTryAgainLater, closeErr.Code)
	}
}
