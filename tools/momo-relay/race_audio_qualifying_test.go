package main

import (
	"context"
	"strings"
	"testing"
)

func qualifyingAudioState(run, phase string, position, best int, gap any, history []raceAudioLapHistory) string {
	values := map[string]any{"bestLapGapToAheadMs": gap}
	if best > 0 {
		values["bestLapMs"] = best
	}
	return raceAudioScenarioState(run, phase, "qualify", 0, position, len(history), history, values)
}

func TestQualifyingAudioTracksBestTargetsAndCombinesLap(t *testing.T) {
	detector := raceAudioDetector{}
	detector.observe(qualifyingAudioState("q1", "green", 3, 0, nil, nil), "CP-1")
	history := []raceAudioLapHistory{{CarID: "CP-1", Lap: 1, LapTimeMS: 6350, Achievement: "personal_best"}}
	first := qualifyingAudioState("q1", "green", 3, 6350, 150, history)
	events := detector.observe(first, "CP-1")
	if len(events) != 1 || events[0].Kind != "qualifying_update" ||
		events[0].JapaneseText != "1周目、6.350。自己ベスト更新。予選3位。2位のベストとの差、0.150秒。" ||
		!strings.Contains(events[0].EnglishText, "Best lap gap to P 2, 0 point one five zero seconds.") {
		t.Fatalf("first measured lap: %+v", events)
	}
	if got := detector.observe(first, "CP-1"); len(got) != 0 {
		t.Fatalf("repeated sample: %+v", got)
	}
	// An upper-ranked driver's improvement changes the target without changing rank.
	events = detector.observe(qualifyingAudioState("q1", "green", 3, 6350, 175, history), "CP-1")
	if len(events) != 1 || events[0].JapaneseText != "予選3位。2位のベストとの差、0.175秒。" {
		t.Fatalf("new target: %+v", events)
	}
	// Losing a rank reports the updated best-lap target, never physical proximity.
	events = detector.observe(qualifyingAudioState("q1", "green", 4, 6350, 50, history), "CP-1")
	if len(events) != 1 || events[0].JapaneseText != "予選4位。3位のベストとの差、0.050秒。" {
		t.Fatalf("rank lost: %+v", events)
	}
	history = append(history, raceAudioLapHistory{CarID: "CP-1", Lap: 2, LapTimeMS: 6450})
	events = detector.observe(qualifyingAudioState("q1", "green", 4, 6350, 50, history), "CP-1")
	if len(events) != 1 || events[0].Kind != "lap_complete" || strings.Contains(events[0].JapaneseText, "予選") {
		t.Fatalf("unchanged best keeps ordinary lap speech: %+v", events)
	}
	history = append(history, raceAudioLapHistory{CarID: "CP-1", Lap: 3, LapTimeMS: 6000, Achievement: "overall_best"})
	events = detector.observe(qualifyingAudioState("q1", "green", 1, 6000, nil, history), "CP-1")
	if len(events) != 1 || !strings.HasSuffix(events[0].JapaneseText, "全体ベスト更新。予選トップ。") {
		t.Fatalf("leader: %+v", events)
	}
	if !raceAudioBrowserLocalEvent(events[0].Kind) {
		t.Fatal("qualifying update must use the existing Browser Kokoro path")
	}
}

func TestQualifyingAudioNeverInventsAnUnmeasuredOrMissingGap(t *testing.T) {
	for _, tc := range []struct {
		best int
		gap  any
		want string
	}{
		{0, 0, ""}, {6000, nil, "予選2位。"}, {6000, -1, "予選2位。"},
		{6000, 6000, "予選2位。"}, {6000, 0, "予選2位。1位と同タイム。"},
		{6000, 1, "予選2位。1位のベストとの差、0.001秒。"},
	} {
		detector := raceAudioDetector{}
		detector.observe(qualifyingAudioState("q", "green", 2, 0, nil, nil), "CP-1")
		events := detector.observe(qualifyingAudioState("q", "green", 2, tc.best, tc.gap, nil), "CP-1")
		if tc.want == "" {
			if len(events) != 0 {
				t.Fatalf("unmeasured: %+v", events)
			}
		} else if len(events) != 1 || events[0].JapaneseText != tc.want {
			t.Fatalf("gap %v: %+v; want %s", tc.gap, events, tc.want)
		}
	}
}

func TestQualifyingAudioSeedsSilentlyAndDoesNotReplaySuppressedUpdates(t *testing.T) {
	detector := raceAudioDetector{}
	state := qualifyingAudioState("q", "green", 2, 6400, 200, nil)
	if events := detector.observe(state, "CP-1"); len(events) != 0 {
		t.Fatal(events)
	}
	for _, suppression := range []string{"yellow", "red", "wrong_way", "paused"} {
		state = qualifyingAudioState("q", "green", 2, 6200, 100, nil)
		if suppression == "paused" {
			state = qualifyingAudioState("q", "paused", 2, 6200, 100, nil)
		} else if suppression == "wrong_way" {
			state = raceAudioStateWithSafety(state, "green", "wrong_way")
		} else {
			state = raceAudioStateWithSafety(state, suppression, "normal")
		}
		for _, event := range detector.observe(state, "CP-1") {
			if event.Kind == "qualifying_update" {
				t.Fatal(event)
			}
		}
		state = qualifyingAudioState("q", "green", 2, 6200, 100, nil)
		if events := detector.observe(state, "CP-1"); len(events) != 0 {
			t.Fatal(events)
		}
		// Re-establish a distinct target before the next suppression.
		detector.observe(qualifyingAudioState("q", "green", 2, 6400, 200, nil), "CP-1")
	}
	for _, event := range detector.observe(qualifyingAudioState("q", "finished", 3, 6400, 100, nil), "CP-1") {
		if event.Kind == "qualifying_update" {
			t.Fatal(event)
		}
	}
	if events := detector.observe(qualifyingAudioState("next", "green", 2, 6100, 1, nil), "CP-1"); len(events) != 0 {
		t.Fatalf("new run replays old state: %+v", events)
	}
	for _, mode := range []string{"race", "practice", "qualifying", ""} {
		detector := raceAudioDetector{}
		detector.observe(raceAudioScenarioState("q", "green", mode, 10, 2, 0, nil, nil), "CP-1")
		for _, event := range detector.observe(raceAudioScenarioState("q", "green", mode, 10, 2, 0, nil,
			map[string]any{"bestLapMs": 6200, "bestLapGapToAheadMs": 200}), "CP-1") {
			if event.Kind == "qualifying_update" {
				t.Fatalf("mode %s: %+v", mode, event)
			}
		}
	}
}

func TestQualifyingAudioQueueReplacesPendingTargetAndPreservesSafety(t *testing.T) {
	queue := newRaceAudioJobQueue()
	old := raceAudioJob{event: raceAudioEvent{EventID: "old", Kind: "qualifying_update", Priority: 50}}
	newer := raceAudioJob{event: raceAudioEvent{EventID: "new", Kind: "qualifying_update", Priority: 50}}
	queue.enqueue(old)
	accepted, dropped := queue.enqueue(newer)
	if !accepted || len(dropped) != 1 || dropped[0].event.EventID != "old" {
		t.Fatal(accepted, dropped)
	}
	if job, _ := queue.dequeue(context.Background()); job.event.EventID != "new" {
		t.Fatal(job)
	}
	queue.enqueue(old)
	queue.enqueue(raceAudioJob{event: raceAudioEvent{Kind: "red_flag", Priority: 120}})
	if accepted, _ := queue.enqueue(newer); accepted {
		t.Fatal("queued qualifying behind safety")
	}
}
