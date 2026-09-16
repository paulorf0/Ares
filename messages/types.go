package messages

import (
	"encoding/json"
	"time"
)

const (
	TypeRole       = "role"
	TypePeerJoined = "peer_joined"
	TypePeerLeft   = "peer_left"
	TypeSDP        = "sdp"
	TypeICE        = "ice"
)

type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type RolePayload struct {
	Polite bool   `json:"polite"`
	RoomID string `json:"roomID"`
}

type RoleMessage struct {
	Type    string      `json:"type"`
	Payload RolePayload `json:"payload"`
}

const (
	TypeString = "string"
)

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`

	// Filled in by the client on send.
	ClientID   string    `json:"ClientID"`
	ClientName string    `json:"ClientName"`
	SendAt     time.Time `json:"CurrentTime"`
}
