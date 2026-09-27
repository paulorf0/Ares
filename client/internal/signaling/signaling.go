// Package signaling is the client side of the signaling server: it joins a
// room, learns the negotiation role and relays envelopes until the peers can
// talk directly.
package signaling

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulorf0/Ares/messages"
)

const writeTimeout = 10 * time.Second

// ErrClosed is returned by Send once the connection has been torn down.
var ErrClosed = errors.New("client: signaling connection closed")

// Conn is a joined room on the signaling server.
type Conn struct {
	ws     *websocket.Conn
	polite bool

	writeMu sync.Mutex

	closed    chan struct{}
	closeOnce sync.Once
}

// Dial joins roomID and waits for the role the server assigns.
func Dial(signalURL, roomID string) (*Conn, error) {
	u, err := url.Parse(signalURL)
	if err != nil {
		return nil, fmt.Errorf("parse signaling url: %w", err)
	}
	query := u.Query()
	query.Set("room", roomID)
	u.RawQuery = query.Encode()

	ws, res, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("signaling handshake rejected (%s): %w", res.Status, err)
		}
		return nil, fmt.Errorf("dial signaling server: %w", err)
	}

	var role messages.RoleMessage
	if err := ws.ReadJSON(&role); err != nil {
		ws.Close()
		return nil, fmt.Errorf("read role message: %w", err)
	}
	if role.Type != messages.TypeRole {
		ws.Close()
		return nil, fmt.Errorf("expected %q as first message, got %q", messages.TypeRole, role.Type)
	}

	return &Conn{
		ws:     ws,
		polite: role.Payload.Polite,
		closed: make(chan struct{}),
	}, nil
}

// Polite reports the negotiation role assigned by the server.
func (c *Conn) Polite() bool {
	return c.polite
}

// Run reads envelopes and passes each to handle until the connection closes,
// then closes it for good.
func (c *Conn) Run(handle func(messages.Envelope) error) {
	defer c.Close()

	for {
		var env messages.Envelope
		if err := c.ws.ReadJSON(&env); err != nil {
			if !c.Closed() {
				slog.Error("read signaling message", "error", err)
			}
			return
		}
		if err := handle(env); err != nil {
			slog.Error("handle signaling message", "type", env.Type, "error", err)
		}
	}
}

// Send wraps payload in a typed envelope and writes it to the server.
func (c *Conn) Send(msgType string, payload any) error {
	if c.Closed() {
		return ErrClosed
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", msgType, err)
	}
	data, err := json.Marshal(messages.Envelope{Type: msgType, Payload: raw})
	if err != nil {
		return fmt.Errorf("encode %s envelope: %w", msgType, err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// Close tears down the websocket once, whichever goroutine gets there first.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)

		c.writeMu.Lock()
		defer c.writeMu.Unlock()

		_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = c.ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = c.ws.Close()
	})
}

// Closed reports whether the connection has been torn down.
func (c *Conn) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
