# Architecture Decisions - Ares

P2P chat over WebRTC. Open-source learning project.
Last updated: 2026-09-12

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
**video**.

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
the first build, ~12 MB more binary. Echo cancellation only works once received
audio is played back, because every played frame must go through the render
side.

---

## Open decisions

| # | Decision | When to decide |
|---|----------|----------------|
| A1 | Interface: TUI (bubbletea) or plain CLI | Before phase 1 becomes usable |
| A4 | Local message history (SQLite / file / none) | Late phase 1 |
| A5 | Database and persistent identity (see D2) | After phase 1 |
| A6 | Reconnection strategy (ICE Restart) | Once dropouts become annoying |

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
  first offer; remote RTP payloads reach `ReceiveAudio`).
- WebRTC APM on the capture side: noise suppression, gain, high-pass (D13).
- Opus decoding and playback, still undecided: `pion/opus` (pure Go, maturity
  unverified) or `hraban/opus` (needs a system libopus).
- APM render side and echo cancellation, once playback exists.
- Audio opt-in instead of inside `client.New`, so tests and text-only sessions
  do not open the microphone.
- Renegotiation over the DataChannel (D6), so a call can start after the
  connection is up.
- Decision point for revisiting D1 (capture and encoding in Go).

### Phase 3 - Screen sharing with audio
- A second video track on the same PeerConnection.
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
