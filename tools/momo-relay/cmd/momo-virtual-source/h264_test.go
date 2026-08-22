package main

import "testing"

func TestSplitH264AccessUnitsGroupsByAUD(t *testing.T) {
	data := []byte{
		0, 0, 0, 1, 0x67, 1,
		0, 0, 0, 1, 0x68, 2,
		0, 0, 1, 0x09, 0xf0,
		0, 0, 1, 0x65, 3,
		0, 0, 1, 0x09, 0xf0,
		0, 0, 1, 0x41, 4,
	}

	units, err := splitH264AccessUnits(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("access units = %d, want 2", len(units))
	}
	if !units[0].keyframe || units[1].keyframe {
		t.Fatalf("keyframe flags = %v, %v", units[0].keyframe, units[1].keyframe)
	}
}

func TestSplitH264AccessUnitsRejectsNonIDRStart(t *testing.T) {
	_, err := splitH264AccessUnits([]byte{
		0, 0, 1, 0x09, 0xf0,
		0, 0, 1, 0x41, 4,
	})
	if err == nil {
		t.Fatal("splitH264AccessUnits() accepted a non-IDR first frame")
	}
}
