package messages

import "encoding/json"

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
