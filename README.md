# Ares

A peer-to-peer chat application built with WebRTC in pure Go, using
[pion/webrtc](https://github.com/pion/webrtc).

Text messaging first, then audio/video, then screen sharing with audio.
Scoped to two participants.

Media and messages travel **directly between peers**, no server relays your
conversation. A small signaling server is used only to introduce the two peers
to each other, and can be shut down once the connection is established.

## Status

Text chat works. Two peers meet through the signaling server, complete the
WebRTC handshake, and exchange messages over a data channel while the server
sits idle or stays off entirely.

Not there yet: room codes are typed by hand rather than generated, the interface
is a bare terminal, and nothing has been tested across two real networks, so NAT
traversal is still unproven. Audio, video and screen sharing have not started.

This is a learning project: the goal is to understand the WebRTC protocol from
the ground up, which is why it uses pion directly rather than embedding a
browser.

## Running it

Two binaries. Start the signaling server on one machine:

```bash
go run ./cmd/signal            # listens on :8080, override with -addr
```

Then run the client on both ends, with the same room code:

```bash
go run ./cmd/ares -room ABC123 -id 1 -name Alice
go run ./cmd/ares -room ABC123 -id 2 -name Bob
```

Type a line and press Enter to send it. Ctrl+C or Ctrl+D quits.

To reach someone on another network, expose the signaling server with a tunnel
and point both clients at it:

```bash
ngrok http 8080
go run ./cmd/ares -signal wss://<your-tunnel>/ws -room ABC123 -id 1 -name Alice
```

Note the `wss://` scheme and the `/ws` path. The client cross-compiles to a
single static binary, so the other side needs no Go installation:

```bash
GOOS=windows GOARCH=amd64 go build -o ares.exe ./cmd/ares
```

## Known limitations

- **No TURN server.** Connections behind CGNAT or symmetric NAT on both ends
  will fail to establish. Roughly 10-20% of real-world pairs.
- **No peer authentication.** DTLS encrypts the connection but does not prove
  who is on the other end, so whoever controls the signaling server could
  intercept it.
- **No offline messages.** Both peers must be online at the same time. The app
  is session-based, like a phone call.

## Design

Architecture decisions, open questions, and the roadmap live in
[DECISIONS.md](DECISIONS.md).

## Contributing

Contributions are welcome. Issues, ideas, and pull requests are all appreciated,
especially from anyone who has fought with NAT traversal before.

Since the project is still taking shape, opening an issue to discuss a change
before writing code will save you time.

## License

[Mozilla Public License 2.0](LICENSE). Changes to existing files stay open, and
the code can be used alongside software under other licenses.
