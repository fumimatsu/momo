package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestOperationsSnapshotSupportsConfiguredScaleCeiling(t *testing.T) {
	server := newScaleTestServer(maximumConfiguredSources, 4)
	snapshot := server.operationsStatusSnapshot(time.Now())
	if len(snapshot.Sources) != maximumConfiguredSources {
		t.Fatalf("sources = %d, want %d", len(snapshot.Sources), maximumConfiguredSources)
	}
	clients := 0
	for _, source := range snapshot.Sources {
		clients += len(source.Downstream.Clients)
	}
	if clients != maximumConfiguredSources*4 {
		t.Fatalf("clients = %d, want %d", clients, maximumConfiguredSources*4)
	}
	if _, err := json.Marshal(snapshot); err != nil {
		t.Fatalf("marshal operations snapshot: %v", err)
	}
}

func BenchmarkOperationsStatusSnapshot(b *testing.B) {
	for _, sourceCount := range []int{4, 8, 12, 16, 32} {
		b.Run(fmt.Sprintf("%d-sources", sourceCount), func(b *testing.B) {
			server := newScaleTestServer(sourceCount, 4)
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				snapshot := server.operationsStatusSnapshot(now)
				if _, err := json.Marshal(snapshot); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func newScaleTestServer(sourceCount int, clientsPerSource int) *relayServer {
	server := &relayServer{
		sources:         make(map[string]*relay, sourceCount),
		sourceOrder:     make([]string, 0, sourceCount),
		raceSubscribers: make(map[uint64]*raceSubscriber),
	}
	now := time.Now()
	for sourceIndex := 0; sourceIndex < sourceCount; sourceIndex++ {
		id := fmt.Sprintf("11.%d", sourceIndex+3)
		source := newStatusTestRelay(id, fmt.Sprintf("CP-%d", sourceIndex+1))
		source.viewers = make(map[uint64]*viewer, clientsPerSource)
		for clientIndex := 0; clientIndex < clientsPerSource; clientIndex++ {
			client := &viewer{
				id:          uint64(clientIndex + 1),
				role:        "observer",
				clientKind:  "web-observer",
				remoteAddr:  fmt.Sprintf("192.168.%d.%d:50000", sourceIndex%250, clientIndex+10),
				telemetryWS: make(chan string, 1),
				eventsWS:    make(chan string, 1),
			}
			client.state.Store(int32(viewerConnected))
			client.lastTelemetrySentAt.Store(now.Add(-time.Duration(clientIndex) * time.Millisecond).UnixNano())
			source.viewers[client.id] = client
		}
		server.sources[id] = source
		server.sourceOrder = append(server.sourceOrder, id)
	}
	return server
}
