package main

import (
	"fmt"
	"log/slog"

	"github.com/pion/webrtc/v4"
)

func main() {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	pA, err := webrtc.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer pA.Close()

	pB, err := webrtc.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer pB.Close()

	pA.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}

		if err := pB.AddICECandidate(i.ToJSON()); err != nil {
			slog.Error("[ADD CANDIDATE PB]: %w", err)
			return
		}
	})

	pB.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}

		if err := pA.AddICECandidate(i.ToJSON()); err != nil {
			slog.Error("[ADD CANDIDATE PA]: %w", err)
			return
		}
	})

	dA, err := pA.CreateDataChannel("ares", nil)
	if err != nil {
		// slog.Error("[Crate Data Channel]: %w", err)
		panic(err)
	}

	dA.OnOpen(func() {
		fmt.Println("A: canal aberto, enviando")
		dA.SendText(`Testando o envio de mensagem`)
	})

	pB.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Println("B recebeu:", string(msg.Data))
		})
	})

	offer, err := pA.CreateOffer(nil)
	if err != nil {
		panic(err)
	}
	if err := pA.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	// Em produção o offer viaja pelo WebSocket de sinalização (D8).
	if err := pB.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	answer, err := pB.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}
	if err := pB.SetLocalDescription(answer); err != nil {
		panic(err)
	}
	if err := pA.SetRemoteDescription(answer); err != nil {
		panic(err)
	}

	select {}
}
