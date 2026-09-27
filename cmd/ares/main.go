// Command ares connects two peers over WebRTC and keeps the connection open.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/paulorf0/Ares/client"
	"github.com/paulorf0/Ares/hotkey"
	"github.com/paulorf0/Ares/messages"
)

// Messages in a session are all from today, so the date would only add noise.
const timeLayout = "15:04:05"

// How long to wait for pending messages to leave before shutting down.
const flushTimeout = 2 * time.Second

func showClient(name string, id string, sentAt time.Time) {
	fmt.Printf("[%s] %s(%s): ", sentAt.Local().Format(timeLayout), name, id)
}

func showMessage(envelope messages.Message) {
	name := envelope.ClientName
	id := envelope.ClientID
	sendAt := envelope.SendAt

	if envelope.Type != messages.TypeString {
		log.Printf("ignoring message of unknown type %q", envelope.Type)
		return
	}

	var text string
	if err := json.Unmarshal(envelope.Payload, &text); err != nil {
		log.Printf("decode message payload: %v", err)
		return
	}

	showClient(name, id, sendAt)
	fmt.Println(text)
}

func main() {
	signalURL := flag.String("signal", "ws://localhost:8080/ws",
		"signaling server address (use the ngrok wss:// url to reach another network)")
	room := flag.String("room", "", "room code to create or join")
	id := flag.String("id", "", "client id")
	name := flag.String("name", "", "client name")
	audio := flag.Bool("audio", false, "join with voice: send the mic and play the other peer")
	statsEvery := flag.Duration("stats", 0,
		"print connection stats this often, like 5s (0 = only on /stats)")
	pttKey := flag.String("ptt-key", "",
		"push-to-talk key, like ctrl+shift+f9; talk only while it is held (needs -audio)")
	flag.Parse()

	if *room == "" {
		log.Fatal("a room code is required: pass -room")
	}
	if *id == "" {
		log.Fatal("missing id: pass -id")
	}
	if *name == "" {
		log.Fatal("missing name: pass -name")
	}
	if *pttKey != "" && !*audio {
		log.Fatal("-ptt-key needs -audio")
	}

	var opts []client.Option
	if *audio {
		opts = append(opts, client.WithAudio())
	}

	// The key is grabbed before joining, so a key that is taken fails here
	// instead of dropping the other peer mid-handshake.
	var joined atomic.Pointer[client.Client]
	if *pttKey != "" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := hotkey.Listen(ctx, *pttKey, func(talking bool) {
			if c := joined.Load(); c != nil {
				if err := c.SetTalking(talking); err != nil {
					log.Printf("push-to-talk: %v", err)
				}
			}
		})
		if err != nil {
			log.Fatal(err)
		}
		opts = append(opts, client.WithVoiceMode(client.VoicePushToTalk))
		log.Printf("push-to-talk: hold %s to talk", *pttKey)
	}

	c, err := client.New(*signalURL, *room, *id, *name, opts...)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	joined.Store(c)

	log.Printf("joined room %q, waiting for the other peer", *room)

	if *statsEvery > 0 {
		go func() {
			for range time.Tick(*statsEvery) {
				// The last report after the call ends is the final one.
				if !showStats(c) {
					return
				}
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	msgChan := readInput()

	// Closed when stdin runs out, which is one of the two ways the program ends.
	inputDone := make(chan struct{})

	go func() {
		defer close(inputDone)

		for msg := range msgChan {
			switch msg {
			case "/ping":
				showPing(c)
				continue
			case "/stats":
				showStats(c)
				continue
			}

			rawMsg, err := json.Marshal(msg)
			if err != nil {
				log.Printf("encode message payload: %v", err)
				continue
			}

			envelope := messages.Message{Type: messages.TypeString, Payload: rawMsg}
			if err := c.SendMessage(envelope); err != nil {
				log.Printf("send message: %v", err)
			}
		}
	}()

	c.ReceiveMessage(func(msg []byte) {
		var envelope messages.Message
		if err := json.Unmarshal(msg, &envelope); err != nil {
			log.Printf("decode incoming message: %v", err)
			return
		}
		showMessage(envelope)
	})

	select {
	case <-stop:
	case <-inputDone:
	}

	flush(c)
}

// showPing prints the round trip to the other peer. It is local: nothing is
// sent.
func showPing(c *client.Client) {
	rtt, err := c.Ping()
	if err != nil {
		fmt.Println("ping:", err)
		return
	}
	fmt.Printf("ping: %.1f ms\n", float64(rtt.Microseconds())/1000)
}

// showStats prints how the call is doing, for debugging a bad connection.
// Like /ping, it is local. It reports false once the call has ended.
func showStats(c *client.Client) bool {
	stats := c.Stats()
	ms := func(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }

	switch {
	case stats.Ended:
		fmt.Println("call ended, final numbers:")
	case stats.RTT > 0:
		fmt.Printf("rtt %.1f ms\n", float64(stats.RTT.Microseconds())/1000)
	default:
		fmt.Println("rtt: not connected yet")
	}

	if r := stats.Receiving; r != nil {
		lossPct := 0.0
		if total := r.Received + r.Lost; total > 0 {
			lossPct = 100 * float64(r.Lost) / float64(total)
		}
		fmt.Printf("receiving: jitter %s, delay %s (target %s), lost %d (%.1f%%), late %d, ran dry %dx\n",
			ms(r.Jitter), ms(r.Buffered), ms(r.Target), r.Lost, lossPct, r.Late, r.Underruns)
		fmt.Printf("           silence trimmed %s, added %s, speech cut %s\n",
			ms(r.SilenceTrimmed), ms(r.SilenceAdded), ms(r.SpeechCut))
	} else {
		fmt.Println("receiving: no audio")
	}

	switch {
	case !stats.Audio:
		fmt.Println("sending: no audio")
	case stats.Ended:
		fmt.Printf("sending: mic dropped %d chunks\n", stats.MicDropped)
	case stats.Transmitting:
		fmt.Printf("sending: on, mic dropped %d chunks\n", stats.MicDropped)
	default:
		fmt.Printf("sending: muted, mic dropped %d chunks\n", stats.MicDropped)
	}
	return !stats.Ended
}

// flush waits for the data channel to drain so a message sent just before exit
// still reaches the peer. Sending only queues the data; closing the connection
// right after would drop whatever is still in the buffer.
func flush(c *client.Client) {
	channel := c.DataChannel()
	if channel == nil {
		return
	}

	deadline := time.Now().Add(flushTimeout)
	for channel.BufferedAmount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
