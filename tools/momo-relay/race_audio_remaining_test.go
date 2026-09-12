package main

import (
	"encoding/json"
	"testing"
	"time"
)

func remainingAudioState(elapsed int64, patch map[string]any) string {
	state := map[string]any{"type": "race_state", "version": 2, "raceRunId": "run", "sequence": 1,
		"serverTimeMs": 1000000 + elapsed, "phase": "green", "allTimeMode": "elapsed", "viewerCarId": "CP-1",
		"raceInfo":  map[string]any{"sessionType": "practice", "timeLimitMs": 180000},
		"standings": []any{map[string]any{"carId": "CP-1", "status": "racing", "allTimeMs": elapsed}}}
	for key, value := range patch {
		state[key] = value
	}
	data, _ := json.Marshal(state)
	return string(data)
}

func remainingEvents(events []raceAudioEvent) []raceAudioEvent {
	result := []raceAudioEvent{}
	for _, event := range events {
		if event.Kind == "time_remaining" {
			result = append(result, event)
		}
	}
	return result
}

func TestRaceAudioRemainingMinutes(t *testing.T) {
	for _, mode := range []string{"practice", "qualify"} {
		t.Run(mode, func(t *testing.T) {
			detector := raceAudioDetector{}
			patch := map[string]any{"raceInfo": map[string]any{"sessionType": mode, "timeLimitMs": 180000}}
			detector.observe(remainingAudioState(59000, patch), "CP-1")
			events := remainingEvents(detector.observe(remainingAudioState(60000, patch), "CP-1"))
			if len(events) != 1 || events[0].JapaneseText != "残り2分。" || events[0].EnglishText != "2 minutes remaining." {
				t.Fatalf("events: %+v", events)
			}
			if len(remainingEvents(detector.observe(remainingAudioState(61000, patch), "CP-1"))) != 0 {
				t.Fatal("duplicate")
			}
			detector.observe(remainingAudioState(119000, patch), "CP-1")
			events = remainingEvents(detector.observe(remainingAudioState(120000, patch), "CP-1"))
			if len(events) != 1 || events[0].JapaneseText != "残り1分。" {
				t.Fatalf("events: %+v", events)
			}
		})
	}
}

func TestRaceAudioRemainingGuards(t *testing.T) {
	for name, patch := range map[string]map[string]any{
		"stopped": {"phase": "paused"}, "finished": {"phase": "finished"}, "safety": {"flag": "yellow"},
		"missing clock": {"standings": []any{map[string]any{"carId": "CP-1", "status": "racing"}}},
		"new run":       {"raceRunId": "other"}, "missing run": {"raceRunId": ""}, "missing sequence": {"sequence": nil},
		"missing timestamp": {"serverTimeMs": nil}, "clock mode": {"allTimeMode": "remaining"},
		"no limit":       {"raceInfo": map[string]any{"sessionType": "practice"}},
		"race unchanged": {"raceInfo": map[string]any{"sessionType": "race", "timeLimitMs": 180000}},
	} {
		t.Run(name, func(t *testing.T) {
			detector := raceAudioDetector{}
			detector.observe(remainingAudioState(59000, nil), "CP-1")
			if got := remainingEvents(detector.observe(remainingAudioState(60000, patch), "CP-1")); len(got) != 0 {
				t.Fatalf("unexpected: %+v", got)
			}
			if got := remainingEvents(detector.observe(remainingAudioState(61000, nil), "CP-1")); len(got) != 0 {
				t.Fatalf("catch-up: %+v", got)
			}
		})
	}
	for _, times := range [][]int64{{61000, 62000}, {58000, 63000}, {179000, 180000}} {
		detector := raceAudioDetector{}
		for _, elapsed := range times {
			if len(remainingEvents(detector.observe(remainingAudioState(elapsed, nil), "CP-1"))) != 0 {
				t.Fatal(times)
			}
		}
	}
}

func TestRaceAudioRemainingQueueValidity(t *testing.T) {
	for name, patch := range map[string]map[string]any{"stop": {"phase": "paused"}, "new run": {"raceRunId": "other"}, "red": {"flag": "red"}, "missing self": {"standings": []any{}}} {
		t.Run(name, func(t *testing.T) {
			detector := raceAudioDetector{}
			detector.observe(remainingAudioState(59000, nil), "CP-1")
			event := remainingEvents(detector.observe(remainingAudioState(60000, nil), "CP-1"))[0]
			if !detector.remainingEventCurrent(event, time.Now()) {
				t.Fatal("fresh event rejected")
			}
			if detector.remainingEventCurrent(event, time.Now().Add(11*time.Second)) {
				t.Fatal("stale event accepted")
			}
			detector.observe(remainingAudioState(61000, patch), "CP-1")
			if detector.remainingEventCurrent(event, time.Now()) {
				t.Fatal("invalidated event accepted")
			}
			if raceAudioMetadataForEvent("prompt", "ja-JP", event, 0, "").RemainingTime == nil {
				t.Fatal("missing browser validity")
			}
		})
	}
	if !raceAudioBrowserLocalEvent("time_remaining") {
		t.Fatal("missing browser Kokoro routing")
	}
}
