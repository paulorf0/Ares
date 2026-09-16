// Package server pairs two peers by room code and relays signaling between them
// until they are connected. It never parses what it forwards, and takes no part
// in the session once the peers are talking directly.
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulorf0/Ares/messages"
)

const (
	sendBuffer = 16

	// Signaling lasts only as long as the handshake, so a peer still connected
	// after this lost its network without sending a close frame. TCP alone
	// takes about two hours to notice.
	defaultSessionTimeout = 5 * time.Minute
)

type Peer struct {
	conn   *websocket.Conn
	send   chan []byte
	polite bool
	roomID string

	// Closed once when the peer is finished. The send channel is deliberately
	// left open: closing it would panic the other peer's relay mid-write.
	done      chan struct{}
	closeOnce sync.Once
}

func newPeer(conn *websocket.Conn, roomID string) *Peer {
	return &Peer{
		conn:   conn,
		send:   make(chan []byte, sendBuffer),
		roomID: roomID,
		done:   make(chan struct{}),
	}
}

// enqueue hands a message to the peer's writePump, or drops it if the peer is
// already gone.
func (p *Peer) enqueue(data []byte) {
	select {
	case p.send <- data:
	case <-p.done:
	}
}

func (p *Peer) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.conn.Close()
	})
}

type Room struct {
	mu    sync.Mutex
	id    string
	peers [2]*Peer
}

func (r *Room) other(p *Peer) *Peer {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, peer := range r.peers {
		if peer != nil && peer != p {
			return peer
		}
	}
	return nil
}

type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room

	// Absolute read deadline given to every peer. Zero disables it.
	sessionTimeout time.Duration
}

func NewHub() *Hub {
	return &Hub{
		rooms:          make(map[string]*Room),
		sessionTimeout: defaultSessionTimeout,
	}
}

// join puts p in the room, creating it if needed. It returns the room and the
// peer already waiting there, if any. A nil room means the room was full.
func (h *Hub) join(roomID string, p *Peer) (*Room, *Peer) {
	h.mu.Lock()
	defer h.mu.Unlock()

	room := h.rooms[roomID]
	if room == nil {
		room = &Room{id: roomID}
		h.rooms[roomID] = room
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	slot := -1
	var other *Peer
	for i, peer := range room.peers {
		if peer == nil {
			if slot < 0 {
				slot = i
			}
			continue
		}
		other = peer
	}

	if slot < 0 {
		return nil, nil
	}

	// Derived from occupancy rather than a counter, so it stays correct after a
	// peer leaves and its slot is reused.
	p.polite = other != nil
	room.peers[slot] = p

	return room, other
}

// leave frees p's slot and returns the peer still in the room, if any. Empty
// rooms are dropped so their codes become reusable.
func (h *Hub) leave(p *Peer) *Peer {
	h.mu.Lock()
	defer h.mu.Unlock()

	room := h.rooms[p.roomID]
	if room == nil {
		return nil
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	var other *Peer
	for i, peer := range room.peers {
		switch {
		case peer == p:
			room.peers[i] = nil
		case peer != nil:
			other = peer
		}
	}

	if other == nil {
		delete(h.rooms, p.roomID)
	}

	return other
}

// Handler returns the signaling HTTP handler for hub, for callers that bring
// their own mux.
func Handler(hub *Hub) http.HandlerFunc {
	return handleWS(hub)
}

// SetSessionTimeout overrides the absolute read deadline given to each peer.
// Zero disables it. Call it before the hub starts serving.
func (h *Hub) SetSessionTimeout(d time.Duration) {
	h.sessionTimeout = d
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWS(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomID := r.URL.Query().Get("room")
		if roomID == "" {
			http.Error(w, "missing room code", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("[Upgrade]: ", err)
			return
		}

		// Never refreshed, so it bounds the whole session rather than the gap
		// between messages.
		if hub.sessionTimeout > 0 {
			if err := conn.SetReadDeadline(time.Now().Add(hub.sessionTimeout)); err != nil {
				log.Println("[SetReadDeadline]: ", err)
				conn.Close()
				return
			}
		}

		peer := newPeer(conn, roomID)

		room, other := hub.join(roomID, peer)
		if room == nil {
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "room is full"))
			conn.Close()
			return
		}

		defer func() {
			peer.close()
			if remaining := hub.leave(peer); remaining != nil {
				notify(remaining, messages.TypePeerLeft)
			}
		}()

		role := messages.RoleMessage{
			Type: messages.TypeRole,
			Payload: messages.RolePayload{
				Polite: peer.polite,
				RoomID: roomID,
			},
		}
		data, err := json.Marshal(role)
		if err != nil {
			// The client blocks waiting for its role, so drop the connection
			// instead of leaving it hanging.
			log.Println("[Role Marshal]: ", err)
			return
		}
		peer.enqueue(data)

		// The waiting peer starts the offer, so it needs to know the room
		// filled up.
		if other != nil {
			notify(other, messages.TypePeerJoined)
		}

		go writePump(peer)
		readPump(peer, room)
	}
}

func notify(p *Peer, msgType string) {
	data, err := json.Marshal(messages.Envelope{Type: msgType})
	if err != nil {
		log.Printf("[Notify %s]: %v", msgType, err)
		return
	}
	p.enqueue(data)
}

func readPump(p *Peer, room *Room) {
	defer p.close()

	for {
		_, data, err := p.conn.ReadMessage()
		if err != nil {
			// gorilla replaces timeouts with its own error type and keeps no
			// wrapped cause, so net.Error is all there is to match on.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				log.Println("[ReadPump] session timed out, releasing the room slot")
			} else {
				log.Println("[ReadPump] connection closed:", err)
			}
			return
		}

		if peer := room.other(p); peer != nil {
			peer.enqueue(data)
		}
	}
}

func writePump(p *Peer) {
	for {
		select {
		case msg := <-p.send:
			if err := p.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				p.close()
				return
			}
		case <-p.done:
			return
		}
	}
}

// Server runs the signaling server on addr until it fails. It brings its own
// mux so that several instances can coexist in one process.
func Server(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", Handler(NewHub()))

	log.Println("[Server] listening on", addr)

	return http.ListenAndServe(addr, mux)
}
