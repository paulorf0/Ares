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

Audio is in progress: the microphone is captured, encoded with Opus and sent
to the other peer, but received audio is not played yet, and noise suppression
and echo cancellation are still being wired in. Video and screen sharing have
not started.

Not there yet: room codes are typed by hand rather than generated, the interface
is a bare terminal, and nothing has been tested across two real networks, so NAT
traversal is still unproven.

This is a learning project: the goal is to understand the WebRTC protocol from
the ground up, which is why it uses pion directly rather than embedding a
browser.

## Requirements

- Go 1.27
- A C/C++ compiler (`gcc`/`g++` on Linux, MinGW-w64 on Windows). Audio capture,
  Opus encoding and audio processing are C/C++ libraries built through cgo; no
  audio library has to be installed on the system.

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

Note the `wss://` scheme and the `/ws` path. The client builds to a single
binary, so the other side needs no Go installation. Because of cgo, building
for Windows from Linux needs the MinGW-w64 cross compiler:

```bash
CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ \
  GOOS=windows GOARCH=amd64 go build -o ares.exe ./cmd/ares
```

To check that your microphone is captured correctly, record five seconds to an
Ogg file and listen to it:

```bash
ARES_MIC_TEST=1 go test -run TestMicrophoneRecordsToOgg -v ./tests/
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
