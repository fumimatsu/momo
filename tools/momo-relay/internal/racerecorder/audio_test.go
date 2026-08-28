package racerecorder

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAudioWriterDecodesFramesAndFillsShortSequenceGap(t *testing.T) {
	directory := t.TempDir()
	startedAt := time.Unix(100, 0)
	writer, err := newAudioWriter(directory, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, audioPacketBytes)
	encoded := base64.StdEncoding.EncodeToString(packet)
	for _, sequence := range []uint64{1, 3} {
		message := []byte(fmt.Sprintf("AUD:1,deadbeef,%d,8,ima,%s", sequence, encoded))
		if err := writer.Write(message, startedAt.Add(time.Duration(sequence)*20*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Frames != 2 || stats.GapFrames != 1 || stats.InvalidFrames != 0 {
		t.Fatalf("audio stats = %#v", stats)
	}
	wav, err := os.ReadFile(filepath.Join(directory, "m5-audio.wav"))
	if err != nil {
		t.Fatal(err)
	}
	wantDataBytes := uint32(3 * audioFrameSamples * 2)
	if string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || binary.LittleEndian.Uint32(wav[40:44]) != wantDataBytes {
		t.Fatalf("invalid WAV header or data size: bytes=%d header=%v", len(wav), wav[:44])
	}
	if len(wav) != 44+int(wantDataBytes) {
		t.Fatalf("WAV length = %d, want %d", len(wav), 44+wantDataBytes)
	}
}

func TestParseAudioFrameRejectsUnsupportedPayload(t *testing.T) {
	if _, err := parseAudioFrame([]byte("AUD:1,deadbeef,1,8,pcm,AAAA")); err == nil {
		t.Fatal("unsupported audio payload was accepted")
	}
}
