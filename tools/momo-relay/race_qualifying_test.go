package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestQualifyingProjectionPreservesAuthorityResult(t *testing.T) {
	state := []byte(`{"type":"race_state","version":2,"phase":"finished","allTimeMode":"elapsed","raceInfo":{"sessionType":"qualify","timeLimitMs":180000},"standings":[{"carId":"CP-2","position":1,"status":"finished","lap":1,"bestLapMs":8000,"allTimeMs":180000,"endReason":"time_limit"},{"carId":"CP-1","position":2,"status":"finished","lap":10,"bestLapMs":9000,"allTimeMs":180000,"bestLapGapToAheadMs":1000,"bestLapGapToLeaderMs":1000,"endReason":"time_limit"}],"lapHistory":[]}`)
	var original map[string]any
	if err := json.Unmarshal(state, &original); err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{"CP-1", "CP-2", "observer"} {
		t.Run(audience, func(t *testing.T) {
			var message string
			var err error
			if audience == "observer" {
				message, err = raceObserverMessage(state)
			} else {
				message, err = raceMessageForCar(state, audience)
			}
			if err != nil {
				t.Fatal(err)
			}
			var projected map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "RACE:")), &projected); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"raceInfo", "standings", "allTimeMode"} {
				if !reflect.DeepEqual(projected[field], original[field]) {
					t.Fatalf("%s changed: %v", field, projected[field])
				}
			}
		})
	}
}
