// Command signal runs the Ares signaling server. It only introduces two peers
// to each other; once they are connected it takes no further part, so it can be
// shut down as soon as a call is up.
package main

import (
	"flag"
	"log"

	"github.com/paulorf0/Ares/server"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	flag.Parse()

	log.Fatal(server.Server(*addr))
}
