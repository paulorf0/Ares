# Architecture Decisions - Ares

P2P chat over WebRTC. Open-source learning project.
Last updated: 2026-09-27

---

## Context

An application for direct communication between **two people** (scope fixed at
2 peers). Planned evolution: text → audio/video → screen sharing with audio.

Core premise: data and media traffic is **peer-to-peer**, with no intermediary
server. An external server exists only for the initial signaling.

---

## Settled decisions

### D1 - Pure Go with pion/webrtc (no browser)

The client is a Go binary speaking WebRTC directly through `pion/webrtc/v4`.
The Wails/Electron model with a browser frontend is ruled out for now.

**Rationale:** the primary goal is learning the protocol. A browser would make
camera and screen capture trivial (`getUserMedia`/`getDisplayMedia`), but it
hides the very layer we want to study.

**Accepted cost:** in phases 2 and 3, Go has no native device capture or
encoders. Audio already needs CGO: miniaudio for capture, libopus for encoding
and the WebRTC APM for processing (D13), so every build needs a C/C++ compiler
and cross-compiling needs a cross toolchain. Revisit specifically when reaching
**video**. Revisited for video: D1 stays (D16).

### D2 - Identity: ephemeral room code

Peer A generates a short code (e.g. `X7K-9P2`) and sends it over an external
channel (WhatsApp, etc.); peer B types it in. The server persists nothing - no
accounts, no database, no presence.

**Planned evolution:** add a database and persistent identity to remove the
manual code exchange. The ephemeral room model is the step before that, not a
dead end.

### D3 - Perfect Negotiation Pattern

Each peer is assigned a role at creation: **polite** or **impolite**.
On an offer collision (both renegotiating at once), the polite peer rolls back
and yields; the impolite peer ignores the incoming offer and keeps its own.

**Rationale:** in phase 2, either side can start a video call. Without the
pattern, a renegotiation collision breaks the connection.

**Immediate implication:** the `polite bool` flag must exist from phase 1, even
if unused. Assigning the role is the signaling server's responsibility (e.g.
whoever creates the room is impolite, whoever joins is polite).

### D4 - DataChannel protocol: typed envelope

Every message travels inside a JSON envelope with a discriminator field:

```json
{ "type": "chat", "payload": { ... } }
```

Types expected over the life of the project:
`chat`, `typing`, `call-request`, `sdp`, `ice`, `file-chunk`.

**Rationale:** free now, and it avoids a painful refactor once files, control
signals, and renegotiation show up.

### D5 - A single DataChannel

One channel, `ordered` + `reliable` (the default behavior).

**Rationale:** simplicity. Splitting into multiple channels (`chat`, `files`,
`control`) is only justified once file transfer enters the picture - at which
point a large file must not block the chat.

### D6 - Renegotiation over the DataChannel itself

Once the P2P connection is established, further SDP negotiation (adding video,
adding screen share, ICE restart) travels **over the DataChannel**, not the
server.

**Consequence:** the signaling server is only needed for the first few seconds
of the connection's life. It can then be shut down without affecting the
session - which matches the chosen infrastructure model (see D8).

### D7 - No peer authentication (for now)

DTLS-SRTP provides encryption but not authenticity: whoever controls the
signaling server could, in principle, MITM the connection by swapping SDPs.

**Consciously accepted** at this stage. It must be listed in the README as a
known limitation.

**Planned evolution:** a public key embedded in the invite code itself, making
MITM infeasible. (Considered and not chosen: SAS - a Short Authentication
String compared out loud between users.)

### D8 - Infrastructure: ngrok on demand

- **Development:** signaling server on `localhost`, both peers on the same
  machine or LAN.
- **Real use:** `ngrok http 8080` creates a temporary public URL at the moment
  of use, torn down afterwards.

**Implication:** the server address must **never** be hardcoded. It has to be a
command-line flag or an environment variable.

### D9 - Public STUN, no TURN

`stun:stun.l.google.com:19302` and similar public servers. No TURN configured.

**Accepted cost:** connections behind symmetric NAT / CGNAT will fail
(estimate: 10-20% of cases). Add TURN only once there is a real connectivity
failure to diagnose. `pion/turn/v5` is already in `go.mod` should a local TURN
be needed for testing.

### D10 - Liveness: absolute deadline, not ping/pong

Every peer gets a single read deadline when it connects, never refreshed. When
it expires the read fails, the room slot is released and the room is dropped if
empty. Default: 5 minutes.

**Rationale:** a peer whose network dies sends no close frame, and TCP takes
around two hours to notice. The slot would stay occupied, and since a room holds
exactly two peers, the user would reconnect with the same code and be told the
room is full by their own ghost.

Ping/pong was considered and not chosen: it exists to keep connections of
indeterminate length alive, while by D6 signaling only has to survive the
handshake. An absolute deadline buys the same slot release with no extra
protocol.

**Accepted cost:** detection is slow. If a peer dies mid-handshake, the other one
waits for the deadline instead of being told right away. Revisit if that wait
becomes annoying in practice.

### D11 - Repo layout: library packages plus thin commands

```
client/       WebRTC peer: signaling handshake, data channel
server/       signaling hub: rooms, roles, blind relay
messages/     wire format shared by both
microphone/   capture driver for mediadevices (D13)
voice/        WebRTC APM: noise, gain, echo (D13)
speaker/      reorders, decodes and plays received audio (D14)
rtpstream/    keeps RTP numbering continuous across track swaps (D15)
h264/         H.264 decoder over the bundled openh264 (D16)
video/        reorders, decodes and hands out received video (D16)
viewer/       video window, used only by cmd/ares (D17)
framing/      downscales and paces camera frames to the quality level (D18)
hotkey/       global push-to-talk key, used only by cmd/ares (D15)
cmd/ares/     client binary
cmd/signal/   server binary
tests/        every test, external to the packages it exercises
```

**Rationale:** the client used to live in `server/`, which made the signaling
package depend on the whole pion stack for code that was not its own. Separate
packages keep each import list honest.

The commands hold no logic: they parse flags and call the library. That is what
keeps the signaling address out of the code (D8) and makes both sides testable
without binding a fixed port.

**Closes A3.**

### D12 - License: MPL-2.0

The Mozilla Public License 2.0, with the full text in `LICENSE`.

**Rationale:** file-level copyleft. Changes to files that are part of Ares have
to stay open, which keeps improvements flowing back, while the code can still be
used next to software under other licenses. MIT was considered too permissive
for the first half, AGPL too restrictive for the second.

**Closes A2.**

### D13 - Audio pipeline: own microphone driver plus WebRTC APM

```
mic (malgo) -> microphone/ driver -> APM capture -> Opus -> RTP
RTP -> Opus decode -> APM render -> speaker
```

- **Capture:** `microphone/` replaces mediadevices' microphone driver, which
  drops devices whose native format is not F32/S16 (PipeWire exposes S32).
  Every device advertises F32/S16 at 48 kHz and miniaudio converts. Works the
  same on WASAPI (Windows) and PulseAudio/PipeWire/ALSA (Linux).
- **Processing:** the WebRTC Audio Processing Module, through
  `github.com/livekit/livekit-cli/v2/pkg/apm` (Apache-2.0, bundles the C++
  sources, no system library). Echo cancellation, noise suppression, automatic
  gain and high-pass filter, in 10 ms frames of 48 kHz int16.

**Rationale:** raw laptop microphones are noisy and often over-gained, and a
laptop speaker feeds the remote voice back into the microphone. Discord works
on the same hardware because it runs this processing; Ares has to as well.
RNNoise removed more steady noise in a test but does neither echo nor gain.

**Accepted cost:** cgo with a C++ compiler (MinGW-w64 on Windows), ~45 s for
the first build, ~12 MB more binary. Every frame the speaker plays goes through
the render side, on the audio thread.

### D14 - Opus decoding with pion/opus, audio as an option of `New`

Received audio is decoded with `github.com/pion/opus` v0.1.0 (pure Go, SILK,
CELT and hybrid, int16 at 48 kHz) and played through malgo, in a `speaker/`
package that mirrors `microphone/`.

The jitter buffer is adaptive, in the spirit of WebRTC's NetEQ:
- packets are stored and only decoded when their turn to play comes, so a late
  packet still plays if it beats its turn; one that never does plays as silence;
- the target delay is the recent spread of arrival delays (97th percentile of
  the last ~2 s) plus 20 ms, between 40 and 300 ms. It rises at once and falls
  at 20 ms/s at most;
- the level is steered through silence only: silent 10 ms frames are skipped
  when the buffer holds more than target plus max(40 ms, jitter), and played
  twice when it holds less than the target, at most once per packet received
  (a stream that stopped must not stretch forever). Speech is only cut past
  500 ms.

`/stats` stops at the end of the call with a final report. Mic drops only
count when the reader was late and came back; a reader that stopped (muted,
call over) leaves drops that are expected.

pion's `samplebuilder` was dropped: it holds each packet until the next one
arrives, which cut word endings before every push-to-talk pause. Loss and
jitter are measured by the buffer itself rather than read from pion's stats,
because pion counts by sequence number, and push-to-talk restarts it at random
on every talk spurt. `/stats` in the terminal shows these numbers.

Audio is turned on with `client.New(..., WithAudio())`. It has to be decided
before the first offer: with no renegotiation yet (D6), a track added later
would never reach the other peer.

**Rationale:** no cgo and no system library, same organization as the rest of
the stack. `hraban/opus` needs libopus installed, which is painful on Windows.

**Accepted cost:** v0.1.0 has no packet loss concealment, so a lost packet
plays as 20 ms of silence (a click). `DecodePLC` exists upstream but is not
released yet; the silence is kept in one place so the swap is local.

### D15 - Voice modes: open mic or push-to-talk

Two modes: the mic always transmits, or it transmits only while a key is held.
The client exposes the mode and a talking flag; reading the key is done by a
`hotkey/` package used only by `cmd/ares`, through `golang.design/x/hotkey`
(global key, works with the terminal unfocused). The key is set by flag.

Muting uses `RTPSender.ReplaceTrack(nil)`: the track is unbound, mediadevices
stops its encoder goroutine, and the APM and Opus stop running. The mic device
stays open, so talking again is instant.

Every bind of a mediadevices track starts a packetizer with random sequence
number and timestamp. The receiver's SRTP expects the sequence to go on where
it stopped and silently drops the rest, so about half the unmutes never
reached the peer (pion/webrtc#2623). `rtpstream.Wrap` sits between the track
and the sender and shifts each bind to continue the previous one: sequence by
one packet, timestamp by the time the mic was out. The speaker reads a
timestamp jump with no missing packets after running dry as a new talk spurt,
not as an underrun.

**Rationale:** an open mic costs a few % of a core for the whole session;
push-to-talk costs nothing while silent, and it is what gaming voice apps use.
Keeping the keyboard out of the client keeps X11 out of the library and tests.

The key is grabbed before joining the room, so a taken key fails without
touching the other peer, and push-to-talk is set with `WithVoiceMode` at
creation, so nothing is sent before the first key press.

Measured on a Ryzen 5 7535HS: a call with an open mic costs ~10% of one core
per peer, about half capture and half playback. A silent push-to-talk peer
drops the capture half.

**Accepted cost:** global keys work on Windows and Linux X11, not on Wayland,
where push-to-talk fails with a message instead of silently opening the mic.
The key must include a modifier, and it is grabbed from the rest of the system
while Ares runs. Desktops take common combinations (XFCE owns Ctrl+F1..F12),
so the suggested default is `ctrl+shift+f9`.

### D16 - Video: D1 stays, H.264 through the bundled openh264

The D1 revisit for video came out in favour of staying with Go. Capture is
mediadevices' camera driver (v4l2 in pure Go on Linux, DirectShow through cgo
on Windows). Encoding is mediadevices' openh264. Decoding goes through a small
cgo wrapper of our own (`h264/`) over the same static library, which already
ships the decoder even though mediadevices only uses the encoder.

**Rationale:** no new system dependency. Measured on this machine with the
bundled library:
- 640x480 at 1 Mbps: encode ~3.4 ms, decode ~0.7 ms per frame;
- 1280x720 at 2.5 Mbps: encode ~9 ms, decode ~2 ms;
- camera to encode to decode at 720p: ~37% of one core.
`make windows` links the camera driver and both codec halves with no DLL
beyond what Windows ships (ole32, oleaut32, quartz).

mediadevices gives the encoder the frame rate it measures on the first frame,
which is 0, and openh264 then ignores the bitrate target: the first call
between Linux and Windows sent 4-8 Mbps instead of 1 and choked on its own
queues (RTT up to 2.7 s, audio running dry). The client's encoder builder
fills in the camera's 30 fps; a test holds the camera to about 1 Mbps.

The alternatives were weaker. VP8/VP9 needs a system libvpx on both OSes, and
the pure-Go `x/image/vp8` only decodes key frames. A webview reverses D1.
FFmpeg is a far heavier dependency for no gain between two peers.

**Accepted cost:**
- The bundled openh264 is 2.1.1, whose decoder has CVE-2025-27091 (heap
  overflow, fixed in 2.6.0). The fix is a build change only: a fork of
  mediadevices with newer `.a` files through `replace`, plus the headers in
  `h264/include/`. Revisit before calls with people outside the test circle.
- No audio/video sync yet: audio waits in its jitter buffer, video is shown as
  soon as it decodes.
- Fixed bitrate until adaptive bitrate lands.

### D17 - Camera on and off mid-call without renegotiation

Both peers add a video transceiver before the first offer, in every call. The
camera is opened only when turned on, so the device stays free otherwise.
Turning it on or off is `ReplaceTrack` on the live sender, with the track
wrapped by `rtpstream.Wrap` like the mic (D15). A `video` envelope over the
DataChannel (D4) tells the other side, since RTP just stops and would leave
the last frame frozen on screen.

The first delivery shows received video in a small Ebitengine window, in a
`viewer/` package used only by `cmd/ares`.

**Rationale:** the frontend needs camera on/off at any point in the call.
Tested with two pion peers: no `OnNegotiationNeeded`, signaling stays stable,
nothing is sent while off, and the remote `OnTrack` fires ~50 ms after turning
it on. Renegotiation (D6) is left for phase 3, where a second video track is
new media. Ebitengine is the shortest path to drawing frames. It keeps the
full UI toolkit (A1) open, and it stays out of the client library and tests.

`SetCamera(true)` returns at once and starts the camera in the background.
The Windows driver opens a camera held by another app without an error and
then never delivers a frame. mediadevices reads the first frame while binding
the track, inside pion, so such a camera froze the handshake or
`ReplaceTrack`, and with them the chat. The client therefore holds a camera
back until it delivers a picture (4 s), closes it otherwise, and opens it
again every second until it works or is cancelled. A camera freed by the
other app comes on by itself. `OnCameraStatus` reports sending, off, or
waiting and why.

**Accepted cost:** every call negotiates H.264, even voice-only ones; a
camera that is off costs no bandwidth. A camera that stops delivering in the
middle of a call (pulled out) still hangs `SetCamera(false)`: mediadevices'
unbind waits for the encoder goroutine, stuck reading the camera, and closing
the camera first races inside mediadevices. Fixing it needs a source wrapper
that can hand the encoder a last frame on demand. On Linux, Ebitengine needs cgo and the
X11/GL development headers.

### D18 - Manual camera quality

Each peer picks how much of its own camera it sends, at any time:

| Level | Width | fps | Bitrate |
|---|---|---|---|
| high (default) | 640 | 30 | 1000 kbps |
| medium | 480 | 24 | 500 kbps |
| low | 320 | 15 | 250 kbps |
| minimum | 160 | 10 | 100 kbps |

The height follows the camera's aspect ratio, and a camera smaller than the
level is never enlarged. `framing/` shapes the frames before the encoder:
nearest-neighbour downscale from any YCbCr to 4:2:0, and frame dropping on a
due time that moves one interval per frame, so 30 to 24 fps drops one frame
in five. Changing the level rebinds the camera track, which builds a new
encoder with the level's bitrate and frame rate, while the camera itself
stays open.

**Rationale:** the first real call showed a home connection and a slow
machine struggling with video. The level is the one knob that eases the
network and the CPU on both sides at once. mediadevices' `video.Scale` fixes
its size at creation and goes through `At`/`Set` per pixel, which its own
comments call ten times costlier for YCbCr; `video.Throttle` runs on a fixed
ticker. A rebind is needed because openh264 takes bitrate and frame rate only
when the encoder is created, and the frame rate sets each frame's share of
the bits: told the wrong one, the low level sent half its target. It is
cheap: no reopening the camera, no renegotiation, and the numbering stays
continuous through `rtpstream`.

**Accepted cost:** a level change freezes the picture for a moment. A level
only lowers what the peer sends; asking the other side to lower its video,
which is what helps a peer with a weak download, is open (A7).

---

## Open decisions

| # | Decision | When to decide |
|---|----------|----------------|
| A1 | Interface: a graphical UI is planned (which toolkit is open). The terminal client mixes incoming messages into the line being typed. Video uses an Ebitengine window for now (D17) | Before calls are shared beyond testing |
| A4 | Local message history (SQLite / file / none) | Late phase 1 |
| A5 | Database and persistent identity (see D2) | After phase 1 |
| A6 | Reconnection strategy (ICE Restart) | Once dropouts become annoying |
| A7 | The other peer adapting too: manual camera quality only lowers what each peer sends, so someone on a weak connection can cut their upload but not their download, which is the other peer's camera. Asking the other side for a lower level needs a `video_quality` envelope over the DataChannel (D4) and a policy for applying it | Right after manual camera quality ships |

### Limitation inherent to the model

**There are no offline messages.** In pure P2P, if the other peer is not
connected, the message does not arrive. The app is session-based, like a phone
call. Supporting store-and-forward would require a stateful server, which
contradicts the project's premise.

---

## Roadmap

### Phase 1 - Text
1. Signaling server: WebSocket in Go, rooms keyed by a short code.
2. Replace the example's base64 copy-paste with real signaling.
3. **Trickle ICE**: send each candidate from `OnICECandidate` as it appears,
   instead of waiting on `GatheringCompletePromise`.
4. Handle `OnConnectionStateChange` to detect dropouts.
5. Message envelope (D4) and the polite/impolite role (D3).

### Phase 2 - Audio and video
- ~~Microphone capture and Opus encoding~~ (done, D13).
- ~~`AddTrack` / `OnTrack`~~ (done: a sendrecv audio track added before the
  first offer).
- ~~WebRTC APM: noise suppression, gain, high-pass, echo cancellation~~ (done, D13).
- ~~Opus decoding and playback~~ (done, D14).
- ~~Audio opt-in with `WithAudio`~~ (done, D14).
- ~~Open mic or push-to-talk~~ (done, D15).
- ~~A voice call between two machines~~ (Linux and Windows, through ngrok).
- ~~Adaptive jitter buffer and `/stats`~~ (done, D14).
- Packet loss concealment once `pion/opus` releases `DecodePLC`.
- Opus in-band FEC and DTX, if `/stats` shows frequent real loss.
- ~~Decision point for revisiting D1 (capture and encoding in Go)~~ (done:
  D1 stays, D16).
- ~~Camera video (D16, D17)~~ (done locally: `h264/` decoder, `video/` receive
  stream, reserved video transceiver, `SetCamera` with the `video` envelope,
  PLI, `viewer/` window in `cmd/ares`, video in `/stats`).
- ~~Camera video between Linux and Windows~~ (through ngrok; the DirectShow
  camera works).
- Confirm on Windows that a camera busy in another app no longer hangs the
  call and comes on by itself once freed (D17).
- ~~Manual camera quality~~ (done, D18: `SetVideoQuality`, `-quality`,
  `/quality`, keys 1-4 in the window).
- Asking the other peer to lower its quality (A7).
- After that: adaptive bitrate (TWCC/GCC plus the encoder's `SetBitRate`),
  openh264 2.6 or later (D16), 720p, audio/video sync.

### Phase 3 - Screen sharing with audio
- A second video track on the same PeerConnection, which needs renegotiation
  over the DataChannel (D6).
- System audio on Linux through the PulseAudio/PipeWire monitor -
  historically the most tedious part.

---

## Reference notes

**What STUN does:** the peer sends a Binding Request to the STUN server, which
replies with the public IP:port it saw the packet arrive from. This produces an
ICE candidate of type `srflx`. The side effect matters as much as the reply:
the outgoing packet **forces the NAT to create the mapping**, punching the hole
the other peer will come in through (UDP hole punching).

**`NewPeerConnection` connects nothing.** It only creates the object and stores
the configuration. Candidate gathering and ICE begin at `SetLocalDescription`.

**STUN stays in use after connecting** - but between the peers, in the
*connectivity checks* that elect the best candidate pair. The external server
is out of the picture by then; DataChannel traffic never passes through it.

**The three layers:**

| Layer | P2P? | Cost |
|---|---|---|
| Signaling (SDP + ICE candidates) | No | ngrok on demand |
| NAT traversal (STUN/TURN) | No | public STUN, free |
| Data and media (DataChannel / RTP) | **Yes** | Zero |
