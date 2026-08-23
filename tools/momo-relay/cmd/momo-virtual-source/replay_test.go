package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildLoopScheduleAppliesRateAndStartOffset(t *testing.T) {
	events := []timedReplayMessage{
		{offset: 500 * time.Millisecond, data: "first"},
		{offset: 1500 * time.Millisecond, data: "second"},
		{offset: 2500 * time.Millisecond, data: "third"},
	}
	schedule := buildLoopSchedule(events, 2, 500*time.Millisecond, 2*time.Second)
	if len(schedule) != 3 {
		t.Fatalf("schedule length=%d want=3", len(schedule))
	}
	wantOffsets := []time.Duration{250 * time.Millisecond, 750 * time.Millisecond, 1750 * time.Millisecond}
	wantData := []string{"second", "third", "first"}
	for index := range schedule {
		if schedule[index].offset != wantOffsets[index] || schedule[index].data != wantData[index] {
			t.Fatalf("schedule[%d]=%s/%q want=%s/%q", index, schedule[index].offset, schedule[index].data, wantOffsets[index], wantData[index])
		}
	}
}

func TestNormalizeReplayTelemetryScheduleMakesLoopsMonotonic(t *testing.T) {
	schedule := []timedReplayMessage{
		{offset: 20 * time.Millisecond, data: `TEL:{"v":2,"k":"s","src":"imu0","boot":"deadbeef","seq":50,"t_us":50000,"m":{"a":[1,2,3],"y":0.1},"q":{"p":33333,"f":["flu_axes"]}}`},
		{offset: 40 * time.Millisecond, data: `TEL:{"v":2,"k":"s","src":"imu0","boot":"deadbeef","seq":10,"t_us":10000,"m":{"a":[4,5,6],"y":0.2},"q":{"p":33333,"f":["flu_axes"]}}`},
	}
	normalized, err := normalizeReplayTelemetrySchedule("virtual-01", schedule)
	if err != nil {
		t.Fatal(err)
	}
	first := decodeReplayPayload(t, normalized[0].data)
	second := decodeReplayPayload(t, normalized[1].data)
	alternate := decodeReplayPayload(t, normalized[0].alternateData)
	if first["seq"] != float64(1) || second["seq"] != float64(2) {
		t.Fatalf("normalized sequences=%v/%v want=1/2", first["seq"], second["seq"])
	}
	if first["t_us"] != float64(20000) || second["t_us"] != float64(40000) {
		t.Fatalf("normalized timestamps=%v/%v want=20000/40000", first["t_us"], second["t_us"])
	}
	if first["boot"] == alternate["boot"] || len(first["boot"].(string)) != 8 || len(alternate["boot"].(string)) != 8 {
		t.Fatalf("loop boot IDs=%v/%v want distinct 8-character IDs", first["boot"], alternate["boot"])
	}
	if second["m"].(map[string]any)["a"].([]any)[0] != float64(4) {
		t.Fatal("motion payload was not preserved")
	}
}

func decodeReplayPayload(t *testing.T, message string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "TEL:")), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestKeyframeAtOrAfterWrapsToNextAvailableKeyframe(t *testing.T) {
	units := []h264AccessUnit{{keyframe: true}, {}, {}, {keyframe: true}, {}}
	if got := keyframeAtOrAfter(units, 2); got != 3 {
		t.Fatalf("keyframe=%d want=3", got)
	}
	if got := keyframeAtOrAfter(units, 4); got != 0 {
		t.Fatalf("wrapped keyframe=%d want=0", got)
	}
}
