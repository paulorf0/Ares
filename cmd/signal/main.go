// Command signal runs the Ares signaling server. It only introduces peers to
// each other and can be shut down once a call is up.
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
