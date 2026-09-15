// Command ares connects two peers over WebRTC and keeps the connection open.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/paulorf0/Ares/client"
)

func main() {
	signalURL := flag.String("signal", "ws://localhost:8080/ws",
		"signaling server address (use the ngrok wss:// url to reach another network)")
	room := flag.String("room", "", "room code to create or join")
	flag.Parse()

	if *room == "" {
		log.Fatal("a room code is required: pass -room")
	}

	c, err := client.New(*signalURL, *room)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	log.Printf("joined room %q, waiting for the other peer", *room)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
