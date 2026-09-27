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

Voice calls work: the microphone is cleaned up (noise suppression, automatic
gain, echo cancellation), encoded with Opus and sent, and the other peer's
audio is played. The mic can stay open or work as push-to-talk with a global
key. A call between Linux and Windows through an ngrok tunnel has worked.

Camera video works: H.264 in both directions, shown in a small window, and
the camera can be turned on and off at any point in the call. A video call
between Linux and Windows through ngrok has worked. Screen sharing has not
started.

Not there yet: room codes are typed by hand rather than generated, the interface
is a terminal plus a video window (a graphical one is planned), and connections
across restrictive NATs are unproven.

This is a learning project: the goal is to understand the WebRTC protocol from
the ground up, which is why it uses pion directly rather than embedding a
browser.

## Requirements

- Go 1.27
- A C/C++ compiler (`gcc`/`g++` on Linux, MinGW-w64 on Windows). Audio capture,
  Opus and H.264 encoding, H.264 decoding and audio processing are C/C++
  libraries built through cgo; no audio or video library has to be installed
  on the system.
- On Linux, the X11 headers (`libx11-dev`) for the push-to-talk key, and the
  OpenGL/X11 headers for the video window:
  `sudo apt install libgl1-mesa-dev libxcursor-dev libxi-dev libxinerama-dev libxrandr-dev libxxf86vm-dev`.

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

Type a line and press Enter to send it. Ctrl+C or Ctrl+D quits. Two local
commands help when a call sounds bad; neither sends anything:

- `/ping` shows the round trip to the other peer;
- `/stats` adds the received audio (jitter, delay, lost and late packets, how
  often playback ran dry) and whether your mic lost audio while sending.

`-stats 5s` prints the same every 5 seconds, handy since typing while messages
arrive garbles the line.

Add `-audio` to talk as well. The mic stays open by default; with `-ptt-key`
it only sends while the key is held, even with the terminal in the background:

```bash
go run ./cmd/ares -room ABC123 -id 1 -name Alice -audio
go run ./cmd/ares -room ABC123 -id 2 -name Bob -audio -ptt-key ctrl+shift+f9
```

The key needs a modifier (`ctrl`, `shift`, `alt`, `super`) plus a letter, digit,
`f1`-`f12` or `space`. Push-to-talk needs Windows or an X11 session; Wayland
doesn't let apps grab global keys. Testing on one machine, use headphones, or
the two clients will feed back into each other.

For video, `-video` opens a window with the other peer's camera, and `/camera`
(or the C key in the window) turns yours on and off at any time; `-camera`
starts with it on. The camera is only open while on. If another app is
using it, Ares says so and keeps trying: the camera comes on by itself once
the other app lets go, and `/camera` again cancels. `/stats` reports the
video too. Two clients on one machine can't share a camera: only one gets it.

If the video stutters or the connection struggles, lower what you send with
`-quality` or, during the call, `/quality` or the keys 1-4 in the window:
`high` (640 wide, 30 fps, 1 Mbps, the default), `medium` (480, 24 fps,
500 kbps), `low` (320, 15 fps, 250 kbps) or `minimum` (160, 10 fps,
100 kbps). The Portuguese names `alta`, `media`, `baixa` and `minima` work
too. This lowers your upload and the other side's download; it does not yet
ask the other side to lower theirs.

```bash
go run ./cmd/ares -room ABC123 -id 1 -name Alice -audio -video -camera
```

To reach someone on another network, expose the signaling server with a tunnel
and point both clients at it:

```bash
ngrok http 8080
go run ./cmd/ares -signal wss://<your-tunnel>/ws -room ABC123 -id 1 -name Alice
```

Note the `wss://` scheme and the `/ws` path.

## Building

The client builds to a single binary, so the other side needs no Go
installation. `make` checks the toolchain first and says what to install if
something is missing:

```bash
make            # bin/ares and bin/signal for this machine
make windows    # bin/ares.exe and bin/signal.exe (needs: sudo apt install mingw-w64)
make all        # both
make test       # go vet plus every test with the race detector
make help       # list the targets
```

The Windows `.exe` only depends on DLLs that ship with Windows. Building it
from Linux takes a few workarounds for the WebRTC audio processing code, which
expects Microsoft's compiler; the `Makefile` explains each one.

## Known limitations

- **No TURN server.** Connections behind CGNAT or symmetric NAT on both ends
  will fail to establish. Roughly 10-20% of real-world pairs.
- **No peer authentication.** DTLS encrypts the connection but does not prove
  who is on the other end, so whoever controls the signaling server could
  intercept it.
- **No offline messages.** Both peers must be online at the same time. The app
  is session-based, like a phone call.
- **Old H.264 decoder.** The bundled openh264 is 2.1.1, which has a known
  decoder vulnerability (CVE-2025-27091). Only call people you trust until it
  is updated.
- **Video at a fixed bitrate** (about 1 Mbps) and not synced with the audio.

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
