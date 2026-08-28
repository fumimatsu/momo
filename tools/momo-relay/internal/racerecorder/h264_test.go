package racerecorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestH264WriterWaitsForIDRAndRotatesOnlyAtIDR(t *testing.T) {
	directory := t.TempDir()
	startedAt := time.Unix(200, 0)
	writer := newH264Writer(directory, startedAt, time.Second)
	nonIDR := []byte{0, 0, 0, 1, 0x41, 1, 2}
	idr := []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x65, 2, 3}
	if err := writer.WriteSample(nonIDR, startedAt.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteSample(idr, startedAt.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteSample(nonIDR, startedAt.Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteSample(idr, startedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	stats, err := writer.Close(startedAt.Add(2500 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if stats.DroppedUntilFirstKeyframe != 1 || stats.Frames != 3 || len(stats.Segments) != 2 {
		t.Fatalf("video stats = %#v", stats)
	}
	for _, segment := range stats.Segments {
		if !segment.StartsWithIDR {
			t.Fatalf("segment does not start with IDR: %#v", segment)
		}
		if _, err := os.Stat(filepath.Join(directory, segment.File)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestH264KeyframeDetection(t *testing.T) {
	if !isH264Keyframe([]byte{0, 0, 1, 0x65, 0}) {
		t.Fatal("IDR was not detected")
	}
	if isH264Keyframe([]byte{0, 0, 0, 1, 0x41, 0}) {
		t.Fatal("non-IDR was classified as a keyframe")
	}
}
