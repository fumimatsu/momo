package main

import (
	"fmt"
	"strings"
	"sync"
)

// courseProgressTracker keeps the accepted Race Control marker position for one car.
// Race Control owns marker ordering and direction validation; Relay only normalizes the
// canonical state so high-rate drive samples do not need to repeat the full race_state.
type courseProgressTracker struct {
	mu                 sync.RWMutex
	current            courseProgressSnapshot
	hasSequence        bool
	seenMarkerEventIDs map[string]struct{}
}

type courseProgressSnapshot struct {
	RaceRunID        string
	RaceSequence     uint64
	Lap              int
	CurrentSector    int
	SectorCount      int
	LastMarkerIndex  *int
	LastMarkerRaceMS *int64
}

type courseMarkerLogSample struct {
	EventID       string `json:"eventId"`
	Lap           int    `json:"lap"`
	MarkerIndex   int    `json:"markerIndex"`
	MarkerRaceMS  int64  `json:"markerRaceMs"`
	CurrentSector int    `json:"currentSector,omitempty"`
	SectorCount   int    `json:"sectorCount"`
}

func (r *relay) observeCourseProgress(context telemetryRaceContext, standing *raceStateStanding) {
	if r == nil {
		return
	}
	_, event := r.courseProgress.observe(context.RaceRunID, r.raceCarID, context.Sequence, standing)
	if event != nil && r.recorder != nil {
		r.recorder.RecordCourseMarker(r.name, r.raceCarID, *event, context)
	}
}

func (tracker *courseProgressTracker) observe(runID string, carID string, sequence uint64, standing *raceStateStanding) (courseProgressSnapshot, *courseMarkerLogSample) {
	if tracker == nil {
		return courseProgressSnapshot{}, nil
	}
	runID = strings.TrimSpace(runID)
	carID = strings.TrimSpace(carID)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if tracker.current.RaceRunID != runID {
		tracker.current = courseProgressSnapshot{RaceRunID: runID}
		tracker.hasSequence = false
		tracker.seenMarkerEventIDs = make(map[string]struct{})
	}
	if tracker.hasSequence && sequence <= tracker.current.RaceSequence {
		return cloneCourseProgressSnapshot(tracker.current), nil
	}
	tracker.current.RaceSequence = sequence
	tracker.hasSequence = true
	if standing == nil {
		return cloneCourseProgressSnapshot(tracker.current), nil
	}

	nextLap := maxInt(0, standing.Lap)
	if nextLap < tracker.current.Lap {
		return cloneCourseProgressSnapshot(tracker.current), nil
	}
	if nextLap > tracker.current.Lap {
		tracker.current.LastMarkerIndex = nil
		tracker.current.LastMarkerRaceMS = nil
	}
	tracker.current.Lap = nextLap
	if standing.SectorCount > 0 {
		tracker.current.SectorCount = standing.SectorCount
	}
	if standing.CurrentSector > 0 && (tracker.current.SectorCount == 0 || standing.CurrentSector <= tracker.current.SectorCount) {
		tracker.current.CurrentSector = standing.CurrentSector
	}
	if standing.LastMarkerIndex == nil || standing.LastMarkerRaceMS == nil ||
		*standing.LastMarkerIndex < 0 || *standing.LastMarkerRaceMS < 0 || tracker.current.SectorCount <= 0 ||
		*standing.LastMarkerIndex >= tracker.current.SectorCount {
		return cloneCourseProgressSnapshot(tracker.current), nil
	}

	markerIndex := *standing.LastMarkerIndex
	markerRaceMS := *standing.LastMarkerRaceMS
	eventID := fmt.Sprintf("%s:%s:course_marker:%d:%d", runID, carID, tracker.current.Lap, markerIndex)
	if runID == "" || carID == "" {
		return cloneCourseProgressSnapshot(tracker.current), nil
	}
	if _, seen := tracker.seenMarkerEventIDs[eventID]; seen {
		if tracker.current.LastMarkerIndex != nil && *tracker.current.LastMarkerIndex == markerIndex {
			tracker.current.LastMarkerRaceMS = int64Pointer(markerRaceMS)
		}
		return cloneCourseProgressSnapshot(tracker.current), nil
	}
	if tracker.current.LastMarkerIndex != nil && markerIndex < *tracker.current.LastMarkerIndex {
		return cloneCourseProgressSnapshot(tracker.current), nil
	}
	tracker.current.LastMarkerIndex = intPointer(markerIndex)
	tracker.current.LastMarkerRaceMS = int64Pointer(markerRaceMS)
	tracker.seenMarkerEventIDs[eventID] = struct{}{}
	event := &courseMarkerLogSample{
		EventID:       eventID,
		Lap:           tracker.current.Lap,
		MarkerIndex:   markerIndex,
		MarkerRaceMS:  markerRaceMS,
		CurrentSector: tracker.current.CurrentSector,
		SectorCount:   tracker.current.SectorCount,
	}
	return cloneCourseProgressSnapshot(tracker.current), event
}

func (tracker *courseProgressTracker) snapshot() courseProgressSnapshot {
	if tracker == nil {
		return courseProgressSnapshot{}
	}
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	return cloneCourseProgressSnapshot(tracker.current)
}

func cloneCourseProgressSnapshot(snapshot courseProgressSnapshot) courseProgressSnapshot {
	clone := snapshot
	if snapshot.LastMarkerIndex != nil {
		clone.LastMarkerIndex = intPointer(*snapshot.LastMarkerIndex)
	}
	if snapshot.LastMarkerRaceMS != nil {
		clone.LastMarkerRaceMS = int64Pointer(*snapshot.LastMarkerRaceMS)
	}
	return clone
}

func intPointer(value int) *int {
	return &value
}

func int64Pointer(value int64) *int64 {
	return &value
}
