package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseSourceIDsAcceptsConfiguredMaximum(t *testing.T) {
	values := make([]string, maximumVirtualSources)
	for index := range values {
		values[index] = fmt.Sprintf("virtual-%02d", index+1)
	}
	sources, err := parseSourceIDs(strings.Join(values, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != maximumVirtualSources {
		t.Fatalf("sources=%d want=%d", len(sources), maximumVirtualSources)
	}
}

func TestParseSourceIDsRejectsAboveConfiguredMaximum(t *testing.T) {
	values := make([]string, maximumVirtualSources+1)
	for index := range values {
		values[index] = fmt.Sprintf("virtual-%02d", index+1)
	}
	if _, err := parseSourceIDs(strings.Join(values, ",")); err == nil {
		t.Fatal("expected source count validation error")
	}
}

func TestBuildPlaybackProfilesSpreadsAcrossKeyframes(t *testing.T) {
	units := make([]h264AccessUnit, 10)
	for index := range units {
		units[index].keyframe = index%2 == 0
	}
	sources := []string{"virtual-01", "virtual-02", "virtual-03", "virtual-04", "virtual-05"}
	profiles := buildPlaybackProfiles(units, sources, true, 0)

	for index, sourceID := range sources {
		want := index * 2
		if profiles[sourceID].startIndex != want {
			t.Fatalf("%s startIndex=%d want=%d", sourceID, profiles[sourceID].startIndex, want)
		}
	}
}

func TestBuildPlaybackProfilesCanBoundSpreadWindow(t *testing.T) {
	units := make([]h264AccessUnit, 20)
	for index := range units {
		units[index].keyframe = true
	}
	sources := []string{"virtual-01", "virtual-02", "virtual-03", "virtual-04", "virtual-05"}
	profiles := buildPlaybackProfiles(units, sources, true, 80)
	want := []int{0, 3, 7, 11, 15}
	for index, sourceID := range sources {
		if profiles[sourceID].startIndex != want[index] {
			t.Fatalf("%s startIndex=%d want=%d", sourceID, profiles[sourceID].startIndex, want[index])
		}
	}
}

func TestBuildPlaybackProfilesDefaultsToFirstKeyframe(t *testing.T) {
	units := []h264AccessUnit{{keyframe: true}, {}, {keyframe: true}}
	profiles := buildPlaybackProfiles(units, []string{"virtual-01", "virtual-02"}, false, 0)
	if profiles["virtual-01"].startIndex != 0 || profiles["virtual-02"].startIndex != 0 {
		t.Fatalf("default playback profiles were not synchronized: %#v", profiles)
	}
}
