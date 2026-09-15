// Package server introduces two peers to each other so they can exchange SDP and
// establish a connection.
// Once the peers are connected, the server no longer takes part in the session.
// It keeps track of the open rooms so that peers can find each other by room code.
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

	// defaultSessionTimeout bounds how long a peer may hold a room slot. By D6
	// signaling only lives through the handshake, so anything still connected
	// after this is a ghost: a peer whose network died without a close frame.
	// TCP on its own would take about two hours to notice.
	defaultSessionTimeout = 5 * time.Minute
)

type Peer struct {
	conn   *websocket.Conn
	send   chan []byte
	polite bool
	roomID string

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

// enqueue hands a message to the peer's writePump. It is a no-op once the peer
// is gone, so relaying to a peer that just disconnected is always safe.
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

	// sessionTimeout is the absolute read deadline given to every peer. Zero
	// disables it.
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

	// Perfect Negotiation (D3): whoever finds someone already in the room is
	// the polite one. Derived from occupancy so it stays correct after a peer
	// leaves and the slot is reused.
	p.polite = other != nil
	room.peers[slot] = p

	return room, other
}

// leave frees p's slot and returns the peer still in the room, if any. An empty
// room is dropped from the hub, otherwise its slots would stay occupied by
// ghosts and every later join would be told the room is full.
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

// Handler returns the signaling HTTP handler for hub. Server mounts it on /ws;
// callers that bring their own mux, tests included, can mount it themselves.
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

		// Absolute, never refreshed: it bounds the whole signaling session
		// rather than the gap between messages. A sliding deadline would keep a
		// stalled handshake alive forever as long as candidates trickle in.
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
			// Without a role the client blocks forever waiting for it, so this
			// has to end the connection rather than be ignored.
			log.Println("[Role Marshal]: ", err)
			return
		}
		peer.enqueue(data)

		// The peer that was already waiting is the one that starts the offer,
		// so it needs to know the room filled up. The peer that just joined
		// needs no notice: it knows it joined.
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
			// Either way the deferred leave frees the slot; the distinction is
			// only there to tell a ghost apart from a peer that said goodbye.
			// gorilla replaces a timeout with its own error type, which keeps
			// no wrapped cause: net.Error is the only thing left to match on.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				log.Println("[ReadPump] session timed out, releasing the room slot")
			} else {
				log.Println("[ReadPump] connection closed:", err)
			}
			return
		}

		// The payload stays opaque here: the server relays bytes and never
		// parses SDP, which is what lets media be added without touching it.
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

func Server() {
	hub := NewHub()

	http.HandleFunc("/ws", handleWS(hub))

	addr := ":8080"
	log.Println("[Server] listening on", addr)
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatal("[Listen And Serve]: ", err)
	}
}
