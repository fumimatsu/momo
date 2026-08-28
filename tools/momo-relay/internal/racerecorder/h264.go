package racerecorder

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type videoSegment struct {
	File          string `json:"file"`
	StartedAtMS   int64  `json:"startedAtMs"`
	DurationMS    int64  `json:"durationMs"`
	Frames        uint64 `json:"frames"`
	Bytes         int64  `json:"bytes"`
	StartsWithIDR bool   `json:"startsWithIdr"`
}

type videoStats struct {
	Frames                    uint64         `json:"frames"`
	Bytes                     int64          `json:"bytes"`
	PacketsLost               uint64         `json:"packetsLost"`
	DroppedUntilFirstKeyframe uint64         `json:"droppedUntilFirstKeyframe"`
	FirstVideoOffsetMS        int64          `json:"firstVideoOffsetMs,omitempty"`
	Segments                  []videoSegment `json:"segments"`
}

type h264Writer struct {
	mu              sync.Mutex
	directory       string
	startedAt       time.Time
	segmentDuration time.Duration
	file            *os.File
	current         videoSegment
	segmentStarted  time.Time
	segmentIndex    int
	stats           videoStats
	ready           chan struct{}
	readyOnce       sync.Once
}

func newH264Writer(directory string, startedAt time.Time, segmentDuration time.Duration) *h264Writer {
	return &h264Writer{
		directory: directory, startedAt: startedAt, segmentDuration: segmentDuration,
		ready: make(chan struct{}),
	}
}

func (writer *h264Writer) Ready() <-chan struct{} {
	return writer.ready
}

func (writer *h264Writer) WriteSample(data []byte, receivedAt time.Time) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	keyframe := isH264Keyframe(data)
	if writer.file == nil {
		if !keyframe {
			writer.stats.DroppedUntilFirstKeyframe++
			return nil
		}
		if err := writer.openSegment(receivedAt, true); err != nil {
			return err
		}
	} else if keyframe && writer.segmentDuration > 0 && receivedAt.Sub(writer.segmentStarted) >= writer.segmentDuration {
		if err := writer.closeSegment(receivedAt); err != nil {
			return err
		}
		if err := writer.openSegment(receivedAt, true); err != nil {
			return err
		}
	}
	written, err := writer.file.Write(data)
	if err != nil {
		return fmt.Errorf("write H.264 segment: %w", err)
	}
	writer.current.Frames++
	writer.current.Bytes += int64(written)
	writer.current.DurationMS = receivedAt.Sub(writer.segmentStarted).Milliseconds()
	writer.stats.Frames++
	writer.stats.Bytes += int64(written)
	writer.readyOnce.Do(func() { close(writer.ready) })
	return nil
}

func (writer *h264Writer) RecordPacketLoss(count uint16) {
	if count == 0 {
		return
	}
	writer.mu.Lock()
	writer.stats.PacketsLost += uint64(count)
	writer.mu.Unlock()
}

func (writer *h264Writer) openSegment(at time.Time, keyframe bool) error {
	writer.segmentIndex++
	name := fmt.Sprintf("video-%04d.h264", writer.segmentIndex)
	file, err := os.OpenFile(filepath.Join(writer.directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create H.264 segment %s: %w", name, err)
	}
	writer.file = file
	writer.segmentStarted = at
	writer.current = videoSegment{
		File: name, StartedAtMS: at.Sub(writer.startedAt).Milliseconds(), StartsWithIDR: keyframe,
	}
	if writer.stats.Frames == 0 {
		writer.stats.FirstVideoOffsetMS = writer.current.StartedAtMS
	}
	return nil
}

func (writer *h264Writer) closeSegment(at time.Time) error {
	if writer.file == nil {
		return nil
	}
	writer.current.DurationMS = at.Sub(writer.segmentStarted).Milliseconds()
	closeErr := writer.file.Close()
	writer.file = nil
	writer.stats.Segments = append(writer.stats.Segments, writer.current)
	writer.current = videoSegment{}
	if closeErr != nil {
		return fmt.Errorf("close H.264 segment: %w", closeErr)
	}
	return nil
}

func (writer *h264Writer) Close(at time.Time) (videoStats, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	err := writer.closeSegment(at)
	writer.stats.Segments = append([]videoSegment(nil), writer.stats.Segments...)
	return writer.stats, err
}

func isH264Keyframe(data []byte) bool {
	for index := 0; index+4 < len(data); {
		start, prefix := nextAnnexBStart(data, index)
		if start < 0 {
			return false
		}
		payload := start + prefix
		if payload < len(data) && data[payload]&0x1f == 5 {
			return true
		}
		index = payload + 1
	}
	return false
}

func nextAnnexBStart(data []byte, offset int) (int, int) {
	for index := offset; index+3 < len(data); index++ {
		if data[index] != 0 || data[index+1] != 0 {
			continue
		}
		if data[index+2] == 1 {
			return index, 3
		}
		if data[index+2] == 0 && data[index+3] == 1 {
			return index, 4
		}
	}
	return -1, 0
}
