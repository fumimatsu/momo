package main

import (
	"testing"
	"time"
)

func TestCommandReplayScheduleRotatesWithoutChangingCommand(t *testing.T) {
	replay := &commandReplay{
		duration: 3 * time.Second,
		events: []timedCommand{
			{offset: 500 * time.Millisecond, line: "one"},
			{offset: 1500 * time.Millisecond, line: "two"},
			{offset: 2500 * time.Millisecond, line: "three"},
		},
	}
	schedule := replay.schedule(time.Second)
	wantOffsets := []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 2500 * time.Millisecond}
	wantLines := []string{"two", "three", "one"}
	for index := range schedule {
		if schedule[index].offset != wantOffsets[index] || schedule[index].line != wantLines[index] {
			t.Fatalf("schedule[%d]=%s/%q want=%s/%q", index, schedule[index].offset, schedule[index].line, wantOffsets[index], wantLines[index])
		}
	}
}
