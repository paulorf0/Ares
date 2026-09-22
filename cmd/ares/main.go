// Command ares connects two peers over WebRTC and keeps the connection open.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/paulorf0/Ares/client"
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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	room := "123"
	id := "1"
	name := "ferlin"
	c, err := client.New(*signalURL, room, id, name)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	//c.CreateAudioTrack()

	<-stop
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
