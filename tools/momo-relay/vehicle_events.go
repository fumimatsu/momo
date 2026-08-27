package main

import (
	"encoding/json"
	"sync"
)

const (
	vehicleEventSchemaVersion = 2
	vehicleEventHistoryLimit  = 32
	vehicleEventQueueLimit    = 64
)

type vehicleImpactEvent struct {
	Type                    string     `json:"type"`
	Version                 int        `json:"version"`
	EventID                 string     `json:"eventId"`
	RaceRunID               string     `json:"raceRunId"`
	CarID                   string     `json:"carId"`
	ImpactClass             string     `json:"impactClass"`
	ImpactKind              string     `json:"impactKind"`
	ClassificationAlgorithm string     `json:"classificationAlgorithm"`
	WindowComplete          bool       `json:"windowComplete"`
	MagnitudeMPS2           float64    `json:"magnitudeMps2"`
	JerkMPS3                float64    `json:"jerkMps3"`
	Axis                    [3]float64 `json:"axis"`
	DamageApplied           bool       `json:"damageApplied"`
	Damage                  float64    `json:"damage"`
	SuppressionReason       string     `json:"suppressionReason,omitempty"`
	HPBefore                float64    `json:"hpBefore"`
	HPAfter                 float64    `json:"hpAfter"`
	ServerTimeMS            int64      `json:"serverTimeMs"`
}

type vehicleEventSnapshot struct {
	Type      string               `json:"type"`
	Version   int                  `json:"version"`
	RaceRunID string               `json:"raceRunId"`
	Events    []vehicleImpactEvent `json:"events"`
}

type vehicleEventStore struct {
	mu        sync.Mutex
	raceRunID string
	events    []vehicleImpactEvent
	eventIDs  map[string]struct{}
}

func newVehicleEventStore() *vehicleEventStore {
	return &vehicleEventStore{eventIDs: make(map[string]struct{})}
}

func (store *vehicleEventStore) reset(raceRunID string) bool {
	if store == nil {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.raceRunID == raceRunID {
		return false
	}
	store.raceRunID = raceRunID
	store.events = nil
	store.eventIDs = make(map[string]struct{})
	return true
}

func (store *vehicleEventStore) add(event vehicleImpactEvent) bool {
	if store == nil || event.EventID == "" {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.eventIDs[event.EventID]; exists {
		return false
	}
	if store.raceRunID != event.RaceRunID {
		store.raceRunID = event.RaceRunID
		store.events = nil
		store.eventIDs = make(map[string]struct{})
	}
	store.events = append(store.events, event)
	store.eventIDs[event.EventID] = struct{}{}
	if len(store.events) > vehicleEventHistoryLimit {
		store.events = append([]vehicleImpactEvent(nil), store.events[len(store.events)-vehicleEventHistoryLimit:]...)
	}
	return true
}

func (store *vehicleEventStore) snapshot() vehicleEventSnapshot {
	if store == nil {
		return vehicleEventSnapshot{Type: "vehicle_event_snapshot", Version: vehicleEventSchemaVersion, Events: []vehicleImpactEvent{}}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	events := append([]vehicleImpactEvent(nil), store.events...)
	return vehicleEventSnapshot{
		Type:      "vehicle_event_snapshot",
		Version:   vehicleEventSchemaVersion,
		RaceRunID: store.raceRunID,
		Events:    events,
	}
}

func marshalVehicleEvent(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
