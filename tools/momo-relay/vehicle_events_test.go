package main

import (
	"encoding/json"
	"testing"
)

func TestVehicleEventStoreBoundsHistoryAndResetsPerRace(t *testing.T) {
	store := newVehicleEventStore()
	store.reset("rr_1")
	for index := 0; index < vehicleEventHistoryLimit+3; index++ {
		event := vehicleImpactEvent{
			Type:      "vehicle_event",
			Version:   1,
			EventID:   "event-" + string(rune('A'+index)),
			RaceRunID: "rr_1",
			CarID:     "CP-1",
		}
		if !store.add(event) {
			t.Fatalf("event %d was not added", index)
		}
		if store.add(event) {
			t.Fatalf("duplicate event %d was added", index)
		}
	}
	snapshot := store.snapshot()
	if snapshot.RaceRunID != "rr_1" || len(snapshot.Events) != vehicleEventHistoryLimit {
		t.Fatalf("bounded snapshot = %#v", snapshot)
	}
	if snapshot.Events[0].EventID != "event-D" {
		t.Fatalf("oldest retained event = %q, want event-D", snapshot.Events[0].EventID)
	}
	if !store.reset("rr_2") {
		t.Fatal("new race did not reset history")
	}
	if got := store.snapshot(); got.RaceRunID != "rr_2" || len(got.Events) != 0 {
		t.Fatalf("reset snapshot = %#v", got)
	}
}

func TestVehicleEventMessagesUseVersionedLiveAndSnapshotShapes(t *testing.T) {
	event := vehicleImpactEvent{
		Type:          "vehicle_event",
		Version:       1,
		EventID:       "CP-1:boot-a:7",
		RaceRunID:     "rr_1",
		CarID:         "CP-1",
		ImpactClass:   "strong",
		DamageApplied: true,
		Damage:        12,
		HPBefore:      100,
		HPAfter:       88,
	}
	message, err := marshalVehicleEvent(event)
	if err != nil {
		t.Fatalf("marshal live event: %v", err)
	}
	var live map[string]any
	if err := json.Unmarshal([]byte(message), &live); err != nil {
		t.Fatalf("decode live event: %v", err)
	}
	if live["type"] != "vehicle_event" || live["version"] != float64(1) {
		t.Fatalf("live event envelope = %#v", live)
	}

	store := newVehicleEventStore()
	store.reset("rr_1")
	store.add(event)
	snapshotMessage, err := marshalVehicleEvent(store.snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var snapshot vehicleEventSnapshot
	if err := json.Unmarshal([]byte(snapshotMessage), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.Type != "vehicle_event_snapshot" || len(snapshot.Events) != 1 {
		t.Fatalf("snapshot envelope = %#v", snapshot)
	}
}
