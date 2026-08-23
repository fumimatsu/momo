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

var benchmarkRaceStateMessageSink string

func BenchmarkRaceStateSourceFanout(b *testing.B) {
	for _, sourceCount := range []int{1, 4, 20, 32, 64} {
		b.Run(fmt.Sprintf("%d-sources", sourceCount), func(b *testing.B) {
			state := benchmarkRaceStatePayload(b, sourceCount)
			carIDs := make([]string, sourceCount)
			for index := range carIDs {
				carIDs[index] = fmt.Sprintf("CAR-%d", index+1)
			}
			b.SetBytes(int64(len(state) * sourceCount))
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				messages, err := raceMessagesForCars(state, carIDs)
				if err != nil {
					b.Fatal(err)
				}
				if len(messages) > 0 {
					benchmarkRaceStateMessageSink = messages[len(messages)-1]
				}
			}
		})
	}
}

func benchmarkRaceStatePayload(tb testing.TB, carCount int) []byte {
	tb.Helper()
	standings := make([]map[string]any, 0, carCount)
	for index := 0; index < carCount; index++ {
		carNumber := index + 1
		standing := map[string]any{
			"carId":            fmt.Sprintf("CAR-%d", carNumber),
			"driver":           fmt.Sprintf("Pilot %02d", carNumber),
			"position":         carNumber,
			"status":           "racing",
			"lap":              20,
			"allTimeMs":        240_000 + index,
			"currentLapMs":     1_000 + index,
			"lapTimeMs":        12_000 + index,
			"bestLapMs":        11_900 + index,
			"sectorCount":      3,
			"currentSector":    index%3 + 1,
			"lastMarkerIndex":  index % 3,
			"lastMarkerRaceMs": 239_000 + index,
			"sectorTimes": []map[string]any{
				{"sector": 1, "sampleLap": 20, "lastMs": 4_000 + index, "bestMs": 3_950 + index},
				{"sector": 2, "sampleLap": 20, "lastMs": 3_900 + index, "bestMs": 3_850 + index},
				{"sector": 3, "sampleLap": 20, "lastMs": 4_100 + index, "bestMs": 4_050 + index},
			},
		}
		if index > 0 {
			standing["intervalToAheadMs"] = 100 + index*20
		}
		standings = append(standings, standing)
	}

	lapHistory := make([]map[string]any, 0, 64)
	for lap := 20; lap >= 1 && len(lapHistory) < 64; lap-- {
		for index := 0; index < carCount && len(lapHistory) < 64; index++ {
			lapHistory = append(lapHistory, map[string]any{
				"carId":             fmt.Sprintf("CAR-%d", index+1),
				"lap":               lap,
				"completedAtRaceMs": lap*12_000 + index,
				"lapTimeMs":         12_000 + index + lap,
				"sectorTimes": []map[string]any{
					{"sector": 1, "timeMs": 4_000 + index},
					{"sector": 2, "timeMs": 3_900 + index},
					{"sector": 3, "timeMs": 4_100 + index + lap},
				},
			})
		}
	}

	state, err := json.Marshal(map[string]any{
		"type":         "race_state",
		"version":      2,
		"raceId":       "race-scale",
		"raceRunId":    "rr_scale",
		"phase":        "green",
		"flag":         "none",
		"sequence":     20,
		"allTimeMode":  "elapsed",
		"serverTimeMs": 1_787_490_000_000,
		"raceInfo": map[string]any{
			"sessionType": "race",
			"totalLaps":   100,
			"sectorCount": 3,
		},
		"standings":  standings,
		"lapHistory": lapHistory,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return state
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
