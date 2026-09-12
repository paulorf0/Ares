# Ares

A peer-to-peer chat application built with WebRTC in pure Go, using
[pion/webrtc](https://github.com/pion/webrtc).

Text messaging first, then audio/video, then screen sharing with audio.
Scoped to two participants.

Media and messages travel **directly between peers**, no server relays your
conversation. A small signaling server is used only to introduce the two peers
to each other, and can be shut down once the connection is established.

## Status

Very early. There is no working application yet, the project is in its
foundational stage.

This is a learning project: the goal is to understand the WebRTC protocol from
the ground up, which is why it uses pion directly rather than embedding a
browser.

## Design

Architecture decisions, open questions, and the roadmap live in
[DECISIONS.md](DECISIONS.md).

## Contributing

Contributions are welcome. Issues, ideas, and pull requests are all appreciated, especially from anyone who has fought with NAT traversal before.

Since the project is still taking shape, opening an issue to discuss a change
before writing code will save you time.
