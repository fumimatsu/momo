package racerecorder

import "testing"

func TestValidateStartRequestRejectsDuplicateIdentity(t *testing.T) {
	request := validStartRequest()
	request.Sources = append(request.Sources, Source{SourceID: "11.3", VehicleID: "vehicle-2", CarID: "CAR-2"})
	if err := validateStartRequest(request, 64); err == nil {
		t.Fatal("duplicate sourceId was accepted")
	}
}

func TestValidateStartRequestRejectsUnsafeRunID(t *testing.T) {
	request := validStartRequest()
	request.RaceRunID = "../run"
	if err := validateStartRequest(request, 64); err == nil {
		t.Fatal("unsafe raceRunId was accepted")
	}
}

func TestValidateStartRequestAcceptsCoordinatorCommandID(t *testing.T) {
	request := validStartRequest()
	request.CommandID = "ops:formation-1.recording"
	if err := validateStartRequest(request, 64); err != nil {
		t.Fatalf("Coordinator commandId was rejected: %v", err)
	}
}

func validStartRequest() StartRequest {
	return StartRequest{
		SchemaVersion: SchemaVersion, CommandID: "command-1", RaceID: "race-1", RaceRunID: "run-1",
		Mode: ModeFullArchive, RequestedAtUnixMS: 1,
		Sources: []Source{{SourceID: "11.3", VehicleID: "vehicle-1", CarID: "CAR-1"}},
	}
}
