package main

import "testing"

func TestCourseProgressTrackerRecordsUniqueAcceptedMarkers(t *testing.T) {
	tracker := &courseProgressTracker{}
	markerOne := 1
	markerOneRaceMS := int64(30000)
	standing := &raceStateStanding{
		CarID: "CP-1", Lap: 2, CurrentSector: 2, SectorCount: 3,
		LastMarkerIndex: &markerOne, LastMarkerRaceMS: &markerOneRaceMS,
	}

	snapshot, event := tracker.observe("rr_123", "CP-1", 10, standing)
	if event == nil || event.EventID != "rr_123:CP-1:course_marker:2:1" || event.MarkerRaceMS != 30000 {
		t.Fatalf("first marker event = %#v", event)
	}
	if snapshot.Lap != 2 || snapshot.LastMarkerIndex == nil || *snapshot.LastMarkerIndex != 1 {
		t.Fatalf("first marker snapshot = %#v", snapshot)
	}
	sameSequenceMarker := 2
	standing.LastMarkerIndex = &sameSequenceMarker
	snapshot, event = tracker.observe("rr_123", "CP-1", 10, standing)
	if event != nil || snapshot.LastMarkerIndex == nil || *snapshot.LastMarkerIndex != 1 {
		t.Fatalf("same sequence changed progress: snapshot=%#v event=%#v", snapshot, event)
	}
	standing.LastMarkerIndex = &markerOne

	correctedRaceMS := int64(29950)
	standing.LastMarkerRaceMS = &correctedRaceMS
	snapshot, event = tracker.observe("rr_123", "CP-1", 11, standing)
	if event != nil {
		t.Fatalf("timing correction emitted duplicate event = %#v", event)
	}
	if snapshot.LastMarkerRaceMS == nil || *snapshot.LastMarkerRaceMS != 29950 {
		t.Fatalf("corrected marker snapshot = %#v", snapshot)
	}

	markerTwo := 2
	markerTwoRaceMS := int64(34500)
	standing.CurrentSector = 3
	standing.LastMarkerIndex = &markerTwo
	standing.LastMarkerRaceMS = &markerTwoRaceMS
	snapshot, event = tracker.observe("rr_123", "CP-1", 12, standing)
	if event == nil || event.MarkerIndex != 2 || event.CurrentSector != 3 {
		t.Fatalf("next marker event = %#v", event)
	}

	standing.LastMarkerIndex = &markerOne
	standing.LastMarkerRaceMS = &markerOneRaceMS
	snapshot, event = tracker.observe("rr_123", "CP-1", 11, standing)
	if event != nil || snapshot.LastMarkerIndex == nil || *snapshot.LastMarkerIndex != 2 {
		t.Fatalf("stale sequence changed progress: snapshot=%#v event=%#v", snapshot, event)
	}
	snapshot, event = tracker.observe("rr_123", "CP-1", 13, standing)
	if event != nil || snapshot.LastMarkerIndex == nil || *snapshot.LastMarkerIndex != 2 {
		t.Fatalf("already-seen marker regressed progress: snapshot=%#v event=%#v", snapshot, event)
	}

	startMarker := 0
	startMarkerRaceMS := int64(50000)
	standing.Lap = 3
	standing.CurrentSector = 1
	standing.LastMarkerIndex = &startMarker
	standing.LastMarkerRaceMS = &startMarkerRaceMS
	_, event = tracker.observe("rr_123", "CP-1", 14, standing)
	if event == nil || event.EventID != "rr_123:CP-1:course_marker:3:0" {
		t.Fatalf("next lap marker event = %#v", event)
	}
}

func TestCourseProgressTrackerRejectsIncompleteMarkerState(t *testing.T) {
	tracker := &courseProgressTracker{}
	marker := 3
	markerRaceMS := int64(1000)
	standing := &raceStateStanding{
		CarID: "CP-1", Lap: 1, CurrentSector: 1, SectorCount: 3,
		LastMarkerIndex: &marker, LastMarkerRaceMS: &markerRaceMS,
	}

	snapshot, event := tracker.observe("rr_123", "CP-1", 1, standing)
	if event != nil || snapshot.LastMarkerIndex != nil {
		t.Fatalf("out-of-range marker was accepted: snapshot=%#v event=%#v", snapshot, event)
	}
}
