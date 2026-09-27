package tests

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/wave"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/paulorf0/Ares/rtpstream"
)

// toneTrack is a mic stand-in: 10 ms chunks of a fixed pattern, paced in real
// time so the encoder is not flooded.
type toneTrack struct{}

func (toneTrack) ID() string   { return "tone" }
func (toneTrack) Close() error { return nil }
func (toneTrack) Read() (wave.Audio, func(), error) {
	time.Sleep(10 * time.Millisecond)
	chunk := wave.NewInt16Interleaved(wave.ChunkInfo{Len: 480, Channels: 1, SamplingRate: 48000})
	for i := range chunk.Data {
		chunk.Data[i] = int16(i%48*500 - 12000)
	}
	return chunk, func() {}, nil
}

// connectPeers joins two peer connections in process, without signaling.
func connectPeers(t *testing.T, a, b *webrtc.PeerConnection) {
	t.Helper()

	connected := make(chan struct{})
	var once sync.Once
	a.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	offer, err := a.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(a)
	if err := a.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := b.SetRemoteDescription(*a.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := b.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered = webrtc.GatheringCompletePromise(b)
	if err := b.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := a.SetRemoteDescription(*b.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-connected:
	case <-time.After(waitTimeout):
		t.Fatal("peers did not connect")
	}
}

func TestTrackSwapsKeepTheStreamContinuous(t *testing.T) {
	t.Run("same track", func(t *testing.T) { testTrackSwaps(t, false) })
	// How the camera works: a new track each time it is turned on.
	t.Run("new track each time", func(t *testing.T) { testTrackSwaps(t, true) })
}

func testTrackSwaps(t *testing.T, fresh bool) {
	params, err := opus.NewParams()
	if err != nil {
		t.Fatal(err)
	}
	codecs := mediadevices.NewCodecSelector(mediadevices.WithAudioEncoders(&params))
	newPeer := func() *webrtc.PeerConnection {
		engine := &webrtc.MediaEngine{}
		codecs.Populate(engine)
		pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(engine)).NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		return pc
	}
	a, b := newPeer(), newPeer()

	stream := rtpstream.New()
	mic := stream.Wrap(mediadevices.NewAudioTrack(toneTrack{}, codecs))
	sender, err := a.AddTrack(mic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var received []rtp.Header
	b.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			mu.Lock()
			received = append(received, pkt.Header)
			mu.Unlock()
		}
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(received)
	}

	connectPeers(t, a, b)

	// Push-to-talk: the mic goes out and back in on the live sender.
	const turns = 6
	const pause = 300 * time.Millisecond
	for turn := range turns {
		before := count()
		waitFor(t, "audio of this turn to arrive", func() bool { return count() >= before+5 })
		if turn == turns-1 {
			break
		}
		if err := sender.ReplaceTrack(nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(pause)
		if fresh {
			mic = stream.Wrap(mediadevices.NewAudioTrack(toneTrack{}, codecs))
		}
		if err := sender.ReplaceTrack(mic); err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	pauses := 0
	for i := 1; i < len(received); i++ {
		prev, cur := received[i-1], received[i]
		if cur.SequenceNumber != prev.SequenceNumber+1 {
			t.Fatalf("sequence jumps from %d to %d", prev.SequenceNumber, cur.SequenceNumber)
		}
		// Within a turn the timestamp moves one packet; across a pause, about
		// the time the mic was out.
		if gap := time.Duration(cur.Timestamp-prev.Timestamp) * time.Second / 48000; gap > 100*time.Millisecond {
			pauses++
			if gap < pause || gap > 10*pause {
				t.Errorf("timestamp moved %v across a %v pause", gap, pause)
			}
		}
	}
	if pauses != turns-1 {
		t.Errorf("found %d pauses in the timestamps, want %d", pauses, turns-1)
	}
	if sent := stream.Stats().Packets; sent < uint64(len(received)) {
		t.Errorf("stream counted %d packets sent, %d arrived", sent, len(received))
	}
}
