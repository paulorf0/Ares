package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"

	_ "github.com/paulorf0/Ares/microphone"
)

// TestMicrophoneRecordsToOgg captures the default microphone through the same
// Opus pipeline the client sends over WebRTC and writes the RTP packets to an
// Ogg file, so the result can be checked by ear. It needs a real microphone and
// someone to listen, so it only runs with ARES_MIC_TEST=1. The file goes to
// ARES_MIC_OUT, or ares-mic.ogg in the system temp dir.
func TestMicrophoneRecordsToOgg(t *testing.T) {
	if os.Getenv("ARES_MIC_TEST") != "1" {
		t.Skip("set ARES_MIC_TEST=1 to record from the microphone")
	}

	const (
		duration = 5 * time.Second
		channels = 2
	)

	out := os.Getenv("ARES_MIC_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "ares-mic.ogg")
	}

	params, err := opus.NewParams()
	if err != nil {
		t.Fatalf("opus params: %v", err)
	}
	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Audio: func(c *mediadevices.MediaTrackConstraints) {
			c.SampleRate = prop.Int(48000)
			c.ChannelCount = prop.Int(channels)
		},
		Codec: mediadevices.NewCodecSelector(mediadevices.WithAudioEncoders(&params)),
	})
	if err != nil {
		t.Fatalf("get user media: %v", err)
	}

	tracks := stream.GetAudioTracks()
	if len(tracks) == 0 {
		t.Fatal("no audio track")
	}
	track := tracks[0].(*mediadevices.AudioTrack)
	defer track.Close()

	reader, err := track.NewRTPReader("opus", 1, 1200)
	if err != nil {
		t.Fatalf("rtp reader: %v", err)
	}
	defer reader.Close()

	writer, err := oggwriter.New(out, 48000, channels)
	if err != nil {
		t.Fatalf("create %s: %v", out, err)
	}

	t.Logf("recording %s, speak into the microphone", duration)

	var packets, bytes int
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		pkts, release, err := reader.Read()
		if err != nil {
			writer.Close()
			t.Fatalf("read rtp: %v", err)
		}
		for _, pkt := range pkts {
			if err := writer.WriteRTP(pkt); err != nil {
				release()
				writer.Close()
				t.Fatalf("write ogg: %v", err)
			}
			packets++
			bytes += len(pkt.Payload)
		}
		release()
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close ogg: %v", err)
	}

	// Opus sends one packet per 20 ms frame; far fewer means capture stalled.
	want := int(duration / (20 * time.Millisecond))
	if packets < want*9/10 {
		t.Errorf("got %d packets in %s, want about %d", packets, duration, want)
	}

	t.Logf("wrote %d packets (%d bytes of Opus) to %s", packets, bytes, out)
}
