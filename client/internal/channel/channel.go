// Package channel is the data channel between the peers: it stamps and sends
// messages, hands control messages to whoever registered their type and
// everything else to the user's handler.
package channel

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/paulorf0/Ares/messages"
	"github.com/pion/webrtc/v4"
)

// Channel holds the negotiated data channel, nil until the handshake gets
// that far.
type Channel struct {
	id, name string

	mu        sync.Mutex
	dc        *webrtc.DataChannel
	onMessage func([]byte)
	onOpen    func()
	control   map[string]func(payload json.RawMessage)
}

// New returns a channel that stamps outgoing messages with id and name.
func New(id, name string) *Channel {
	return &Channel{
		id:      id,
		name:    name,
		control: make(map[string]func(json.RawMessage)),
	}
}

// Handle routes incoming messages of msgType to fn instead of the user's
// handler.
func (c *Channel) Handle(msgType string, fn func(payload json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.control[msgType] = fn
}

// OnOpen registers fn to run once the channel opens.
func (c *Channel) OnOpen(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onOpen = fn
}

// OnMessage registers fn as the handler for the user's messages. Messages
// that arrive with no handler set are dropped.
func (c *Channel) OnMessage(fn func(msg []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage = fn
}

// Attach takes the data channel, created locally or received from the peer.
func (c *Channel) Attach(dc *webrtc.DataChannel) {
	c.mu.Lock()
	c.dc = dc
	c.mu.Unlock()

	dc.OnOpen(func() {
		slog.Info("data channel open", "label", dc.Label())
		c.mu.Lock()
		fn := c.onOpen
		c.mu.Unlock()
		if fn != nil {
			fn()
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		c.dispatch(msg.Data)
	})
}

// dispatch hands msg to its control handler, if its type has one, or to the
// user's handler.
func (c *Channel) dispatch(msg []byte) {
	var head struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	parsed := json.Unmarshal(msg, &head) == nil

	c.mu.Lock()
	control := c.control[head.Type]
	handler := c.onMessage
	c.mu.Unlock()

	if parsed && control != nil {
		control(head.Payload)
		return
	}
	if handler == nil {
		slog.Warn("incoming message dropped: no handler registered")
		return
	}
	handler(msg)
}

// DataChannel returns the negotiated channel, or nil while the handshake is
// still in flight.
func (c *Channel) DataChannel() *webrtc.DataChannel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dc
}

// Ready reports whether the channel is open.
func (c *Channel) Ready() bool {
	dc := c.DataChannel()
	return dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen
}

// Send stamps msg with the local identity and sends it.
func (c *Channel) Send(msg messages.Message) error {
	dc := c.DataChannel()
	if dc == nil {
		return fmt.Errorf("send message: channel == nil")
	}

	msg.SendAt = time.Now()
	msg.ClientName = c.name
	msg.ClientID = c.id
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	if err := dc.Send(raw); err != nil {
		return fmt.Errorf("channel sendtext: %w", err)
	}
	return nil
}
