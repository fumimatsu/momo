package main

import (
	"fmt"
	"math"
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
	RouteProgress    *raceStateRouteProgress
}

type courseMarkerLogSample struct {
	EventID            string   `json:"eventId"`
	Lap                int      `json:"lap"`
	MarkerIndex        *int     `json:"markerIndex,omitempty"`
	MarkerRaceMS       *int64   `json:"markerRaceMs,omitempty"`
	RouteGateID        string   `json:"routeGateId,omitempty"`
	RouteGateIndex     *int     `json:"routeGateIndex,omitempty"`
	RouteGateCount     int      `json:"routeGateCount,omitempty"`
	CourseProgress     *float64 `json:"courseProgress,omitempty"`
	NextCourseProgress *float64 `json:"nextCourseProgress,omitempty"`
	RouteRaceMS        *int64   `json:"routeRaceMs,omitempty"`
	CurrentSector      int      `json:"currentSector,omitempty"`
	SectorCount        int      `json:"sectorCount"`
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
		tracker.current.RouteProgress = nil
	}
	tracker.current.Lap = nextLap
	if standing.SectorCount > 0 {
		tracker.current.SectorCount = standing.SectorCount
	}
	if standing.CurrentSector > 0 && (tracker.current.SectorCount == 0 || standing.CurrentSector <= tracker.current.SectorCount) {
		tracker.current.CurrentSector = standing.CurrentSector
	}
	tracker.observePublicMarker(standing)
	if validRaceStateRouteProgress(standing.RouteProgress) {
		progress := *standing.RouteProgress
		eventID := fmt.Sprintf("%s:%s:route_gate:%d:%d", runID, carID, tracker.current.Lap, progress.GateIndex)
		if runID == "" || carID == "" {
			return cloneCourseProgressSnapshot(tracker.current), nil
		}
		if _, seen := tracker.seenMarkerEventIDs[eventID]; seen {
			if tracker.current.RouteProgress != nil && tracker.current.RouteProgress.GateIndex == progress.GateIndex {
				tracker.current.RouteProgress = cloneRaceStateRouteProgress(&progress)
			}
			return cloneCourseProgressSnapshot(tracker.current), nil
		}
		if tracker.current.RouteProgress != nil && progress.GateIndex < tracker.current.RouteProgress.GateIndex {
			return cloneCourseProgressSnapshot(tracker.current), nil
		}
		tracker.current.RouteProgress = cloneRaceStateRouteProgress(&progress)
		tracker.seenMarkerEventIDs[eventID] = struct{}{}
		event := courseMarkerLogSample{
			EventID: eventID, Lap: tracker.current.Lap,
			MarkerIndex:  cloneIntPointer(tracker.current.LastMarkerIndex),
			MarkerRaceMS: cloneInt64Pointer(tracker.current.LastMarkerRaceMS),
			RouteGateID:  progress.GateID, RouteGateIndex: intPointer(progress.GateIndex),
			RouteGateCount: progress.GateCount, CourseProgress: float64Pointer(progress.CourseProgress),
			NextCourseProgress: float64Pointer(progress.NextCourseProgress), RouteRaceMS: int64Pointer(progress.RaceTimeMS),
			CurrentSector: tracker.current.CurrentSector, SectorCount: tracker.current.SectorCount,
		}
		return cloneCourseProgressSnapshot(tracker.current), &event
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
		MarkerIndex:   intPointer(markerIndex),
		MarkerRaceMS:  int64Pointer(markerRaceMS),
		CurrentSector: tracker.current.CurrentSector,
		SectorCount:   tracker.current.SectorCount,
	}
	return cloneCourseProgressSnapshot(tracker.current), event
}

func (tracker *courseProgressTracker) observePublicMarker(standing *raceStateStanding) {
	if standing.LastMarkerIndex == nil || standing.LastMarkerRaceMS == nil ||
		*standing.LastMarkerIndex < 0 || *standing.LastMarkerRaceMS < 0 || tracker.current.SectorCount <= 0 ||
		*standing.LastMarkerIndex >= tracker.current.SectorCount {
		return
	}
	markerIndex := *standing.LastMarkerIndex
	if tracker.current.LastMarkerIndex != nil && markerIndex < *tracker.current.LastMarkerIndex {
		return
	}
	tracker.current.LastMarkerIndex = intPointer(markerIndex)
	tracker.current.LastMarkerRaceMS = int64Pointer(*standing.LastMarkerRaceMS)
}

func validRaceStateRouteProgress(progress *raceStateRouteProgress) bool {
	return progress != nil && strings.TrimSpace(progress.GateID) != "" &&
		progress.GateCount > 0 && progress.GateCount <= 50 &&
		progress.GateIndex >= 0 && progress.GateIndex < progress.GateCount &&
		!math.IsNaN(progress.CourseProgress) && !math.IsInf(progress.CourseProgress, 0) &&
		!math.IsNaN(progress.NextCourseProgress) && !math.IsInf(progress.NextCourseProgress, 0) &&
		progress.CourseProgress >= 0 && progress.CourseProgress < 1 &&
		progress.NextCourseProgress > progress.CourseProgress && progress.NextCourseProgress <= 1 &&
		progress.RaceTimeMS >= 0
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
	clone.RouteProgress = cloneRaceStateRouteProgress(snapshot.RouteProgress)
	return clone
}

func cloneRaceStateRouteProgress(progress *raceStateRouteProgress) *raceStateRouteProgress {
	if progress == nil {
		return nil
	}
	clone := *progress
	return &clone
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return int64Pointer(*value)
}

func routeGateIndex(progress *raceStateRouteProgress) *int {
	if progress == nil {
		return nil
	}
	return intPointer(progress.GateIndex)
}

func routeRaceMS(progress *raceStateRouteProgress) *int64 {
	if progress == nil {
		return nil
	}
	return int64Pointer(progress.RaceTimeMS)
}

func intPointer(value int) *int {
	return &value
}

func int64Pointer(value int64) *int64 {
	return &value
}

func float64Pointer(value float64) *float64 {
	return &value
}
