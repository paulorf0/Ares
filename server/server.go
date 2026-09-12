// Package server introduces two peers to each other so they can exchange SDP and
// establish a connection.
// Once the peers are connected, the server no longer takes part in the session.
// It keeps track of the open rooms so that peers can find each other by room code.
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type Peer struct {
	conn   *websocket.Conn
	send   chan []byte
	polite bool
	roomID string
}

type Room struct {
	mu    sync.Mutex
	id    string
	peers [2]*Peer
	count int
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
}

func (h *Hub) join(roomID string, p *Peer) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()

	room := h.rooms[roomID]
	if room == nil {
		room = &Room{id: roomID}
		h.rooms[roomID] = room
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	if room.count >= len(room.peers) {
		return nil
	}

	p.polite = room.count == 1
	room.peers[room.count] = p
	room.count++

	return room
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type roleMessage struct {
	Type    string `json:"type"`
	Payload struct {
		Polite bool `json:"polite"`
	} `json:"payload"`
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

		peer := &Peer{conn: conn, send: make(chan []byte, 16), roomID: roomID}

		room := hub.join(roomID, peer)
		if room == nil {
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "room is full"))
			conn.Close()
			return
		}

		role := roleMessage{Type: "role"}
		role.Payload.Polite = peer.polite
		if data, err := json.Marshal(role); err == nil {
			peer.send <- data
		}

		go writePump(peer)
		readPump(peer, room)
	}
}

func readPump(p *Peer, room *Room) {
	defer func() {
		p.conn.Close()
		close(p.send)
	}()

	for {
		_, data, err := p.conn.ReadMessage()
		if err != nil {
			log.Println("[ReadPump] connection closed:", err)
			return
		}

		if peer := room.other(p); peer != nil {
			peer.send <- data
		}
	}
}

func writePump(p *Peer) {
	for msg := range p.send {
		if err := p.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

func Server() {
	hub := &Hub{rooms: make(map[string]*Room)}

	http.HandleFunc("/ws", handleWS(hub))

	addr := ":8080"
	log.Println("[Server] listening on", addr)
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatal("[Listen And Serve]: ", err)
	}
}
