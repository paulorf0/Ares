// Package peer owns the PeerConnection and the offer/answer exchange. It
// sends descriptions and candidates through a Signaler, so the same code can
// negotiate over the signaling server or, later, over the data channel.
package peer

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/paulorf0/Ares/messages"
	"github.com/pion/interceptor"
	"github.com/pion/mediadevices"
	"github.com/pion/webrtc/v4"
)

// Signaler carries descriptions and candidates to the other peer.
type Signaler interface {
	Send(msgType string, payload any) error
	Closed() bool
}

// Config wires the connection to the rest of the client. The callbacks run on
// pion goroutines, except OnSendersReady, which runs inside Handle.
type Config struct {
	Codecs       *mediadevices.CodecSelector
	Signaler     Signaler
	ChannelLabel string

	OnTrack       func(*webrtc.TrackRemote)
	OnDataChannel func(*webrtc.DataChannel) // both the one created here and the one received
	// OnSendersReady runs once the descriptions have started the RTP senders.
	OnSendersReady func()
	// OnEnded runs when the connection closes or fails, possibly more than once.
	OnEnded func()
}

// Peer is one side of the call.
type Peer struct {
	cfg Config
	pc  *webrtc.PeerConnection

	// Only touched from Handle, which runs on the signaling goroutine.
	pendingICE []webrtc.ICECandidateInit
}

// New builds the PeerConnection with the given codecs and hooks up the
// callbacks. Nothing is sent until Handle gets a peer_joined or an offer.
func New(cfg Config) (*Peer, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	mediaEngine := &webrtc.MediaEngine{}
	cfg.Codecs.Populate(mediaEngine)

	// A custom MediaEngine skips pion's defaults, so NACK and RTCP reports
	// have to be registered by hand.
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	)

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}
	p := &Peer{cfg: cfg, pc: pc}

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		slog.Info("remote track", "kind", track.Kind().String(), "codec", track.Codec().MimeType)
		cfg.OnTrack(track)
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil || cfg.Signaler.Closed() {
			return
		}
		if err := cfg.Signaler.Send(messages.TypeICE, candidate.ToJSON()); err != nil {
			slog.Error("send ice candidate", "error", err)
		}
	})

	// Fires only on the answering side; the offering peer creates its own.
	pc.OnDataChannel(cfg.OnDataChannel)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("peer connection state changed", "state", state.String())
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			cfg.OnEnded()
		}
	})

	return p, nil
}

// Conn returns the underlying connection, for adding tracks and transceivers
// before the first offer.
func (p *Peer) Conn() *webrtc.PeerConnection {
	return p.pc
}

// Handle takes the negotiation messages: peer_joined, sdp and ice.
func (p *Peer) Handle(env messages.Envelope) error {
	switch env.Type {
	case messages.TypePeerJoined:
		return p.startOffer()

	case messages.TypeSDP:
		var desc webrtc.SessionDescription
		if err := json.Unmarshal(env.Payload, &desc); err != nil {
			return fmt.Errorf("decode session description: %w", err)
		}
		return p.handleRemoteDescription(desc)

	case messages.TypeICE:
		var candidate webrtc.ICECandidateInit
		if err := json.Unmarshal(env.Payload, &candidate); err != nil {
			return fmt.Errorf("decode ice candidate: %w", err)
		}
		return p.handleRemoteCandidate(candidate)

	default:
		slog.Warn("unknown signaling message", "type", env.Type)
		return nil
	}
}

// startOffer creates the data channel and sends the offer. It runs on the peer
// that was already in the room when the second one joined.
func (p *Peer) startOffer() error {
	dc, err := p.pc.CreateDataChannel(p.cfg.ChannelLabel, nil)
	if err != nil {
		return fmt.Errorf("create data channel: %w", err)
	}
	p.cfg.OnDataChannel(dc)

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	// SetLocalDescription is what starts ICE gathering.
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	return p.cfg.Signaler.Send(messages.TypeSDP, offer)
}

func (p *Peer) handleRemoteDescription(desc webrtc.SessionDescription) error {
	if err := p.pc.SetRemoteDescription(desc); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	p.flushPendingICE()

	if desc.Type != webrtc.SDPTypeOffer {
		p.cfg.OnSendersReady()
		return nil
	}

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	p.cfg.OnSendersReady()

	return p.cfg.Signaler.Send(messages.TypeSDP, answer)
}

// handleRemoteCandidate adds the candidate, or holds it until the remote
// description is set.
func (p *Peer) handleRemoteCandidate(candidate webrtc.ICECandidateInit) error {
	if p.pc.RemoteDescription() == nil {
		p.pendingICE = append(p.pendingICE, candidate)
		return nil
	}
	return p.pc.AddICECandidate(candidate)
}

func (p *Peer) flushPendingICE() {
	for _, candidate := range p.pendingICE {
		if err := p.pc.AddICECandidate(candidate); err != nil {
			slog.Error("add buffered ice candidate", "error", err)
		}
	}
	p.pendingICE = nil
}

// State reports the state of the peer connection.
func (p *Peer) State() webrtc.PeerConnectionState {
	return p.pc.ConnectionState()
}

// RTT is the latest round trip ICE measured on the path in use. ok is false
// until a path exists.
func (p *Peer) RTT() (rtt time.Duration, ok bool) {
	// A closed transport logs an error when asked, so don't ask.
	switch p.pc.ConnectionState() {
	case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateFailed:
		return 0, false
	}
	ice := p.pc.SCTP().Transport().ICETransport()
	stats, ok := ice.GetSelectedCandidatePairStats()
	if !ok || stats.ResponsesReceived == 0 {
		return 0, false
	}
	return time.Duration(stats.CurrentRoundTripTime * float64(time.Second)), true
}

// Close closes the peer connection.
func (p *Peer) Close() error {
	return p.pc.Close()
}
