package main

import (
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

func TestKeyframeAtOrAfterWrapsToNextAvailableKeyframe(t *testing.T) {
	units := []h264AccessUnit{{keyframe: true}, {}, {}, {keyframe: true}, {}}
	if got := keyframeAtOrAfter(units, 2); got != 3 {
		t.Fatalf("keyframe=%d want=3", got)
	}
	if got := keyframeAtOrAfter(units, 4); got != 0 {
		t.Fatalf("wrapped keyframe=%d want=0", got)
	}
}
