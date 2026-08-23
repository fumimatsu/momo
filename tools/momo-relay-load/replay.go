package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const recordedReplayDisplayGear = 5

type captureCommandRecord struct {
	Kind         string  `json:"kind"`
	RunElapsedMS float64 `json:"run_elapsed_ms"`
	Line         string  `json:"line"`
	Sent         bool    `json:"sent"`
}

type timedCommand struct {
	offset time.Duration
	line   string
}

type commandReplay struct {
	events   []timedCommand
	duration time.Duration
}

func loadCommandReplay(path string) (*commandReplay, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open command replay %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	events := make([]timedCommand, 0)
	for scanner.Scan() {
		var record captureCommandRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || record.Kind != "command" || !record.Sent || record.Line == "" || record.RunElapsedMS < 0 {
			continue
		}
		events = append(events, timedCommand{
			offset: time.Duration(record.RunElapsedMS * float64(time.Millisecond)),
			line:   commandWithDisplayGear(record.Line, recordedReplayDisplayGear),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan command replay %q: %w", path, err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("command replay %q has no sent command records", path)
	}
	sort.SliceStable(events, func(left, right int) bool { return events[left].offset < events[right].offset })
	duration := events[len(events)-1].offset + 20*time.Millisecond
	return &commandReplay{events: events, duration: duration}, nil
}

func commandWithDisplayGear(line string, gear int) string {
	body := strings.TrimRight(line, "\r\n")
	lineEnding := line[len(body):]
	return fmt.Sprintf("%s,G:%d%s", body, gear, lineEnding)
}

func (replay *commandReplay) schedule(startOffset time.Duration) []timedCommand {
	if replay == nil || len(replay.events) == 0 || replay.duration <= 0 {
		return nil
	}
	startOffset %= replay.duration
	if startOffset < 0 {
		startOffset += replay.duration
	}
	schedule := make([]timedCommand, 0, len(replay.events))
	for _, event := range replay.events {
		relative := event.offset - startOffset
		if relative < 0 {
			relative += replay.duration
		}
		schedule = append(schedule, timedCommand{offset: relative, line: event.line})
	}
	sort.SliceStable(schedule, func(left, right int) bool { return schedule[left].offset < schedule[right].offset })
	return schedule
}
