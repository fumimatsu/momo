package main

import (
	"github.com/pion/webrtc/v4"
	"testing"
)

func TestSpectatorPublisherIsReadOnlyEvenWithObserverCommandsEnabled(t *testing.T) {
	r := &relay{sourceKind: relaySourceKindVehicle, allowObserverCommand: true}
	for _, role := range []string{"observer", "pilot"} {
		if r.viewerCommandAllowed(&viewer{role: role, clientKind: "spectator-publisher"}) {
			t.Fatal("spectator publisher accepted a control command")
		}
	}
	if !r.viewerCommandAllowed(&viewer{role: "pilot", clientKind: "web-pilot"}) {
		t.Fatal("Pilot behavior changed")
	}
}

func TestSpectatorAudioUsesDedicatedBoundedQueue(t *testing.T) {
	publisher := &viewer{id: 1, role: "observer", clientKind: "spectator-publisher", audioWS: make(chan string, 8), telemetryWS: make(chan string, 1)}
	publisher.audioSubscribed.Store(true)
	observer := &viewer{id: 2, role: "observer", clientKind: "web-observer", telemetryWS: make(chan string, 1)}
	r := &relay{viewers: map[uint64]*viewer{1: publisher, 2: observer}}
	for i := 0; i < 30; i++ {
		r.broadcastTelemetry(webrtc.DataChannelMessage{IsString: true, Data: []byte("AUD:1,12345678,1,8,ima,AAAA")})
	}
	if len(publisher.audioWS) == 0 || len(publisher.audioWS) > 8 {
		t.Fatal("audio queue is not bounded")
	}
	if len(publisher.telemetryWS) != 0 || len(observer.telemetryWS) != 0 {
		t.Fatal("audio leaked into telemetry or Team Observer")
	}
}
