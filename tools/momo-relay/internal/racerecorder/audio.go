package racerecorder

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	audioSampleRate   = 8000
	audioFrameSamples = 160
	audioPacketBytes  = 84
	maximumAudioGap   = 250
)

var imaIndexTable = [...]int{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}
var imaStepTable = [...]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45, 50,
	55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230, 253, 279,
	307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963, 1060, 1166, 1282,
	1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871,
	5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899, 15289, 16818,
	18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

type audioFrame struct {
	bootID   string
	sequence uint64
	samples  [audioFrameSamples]int16
}

type audioEvent struct {
	ReceivedOffsetMS int64  `json:"receivedOffsetMs"`
	BootID           string `json:"bootId"`
	Sequence         uint64 `json:"sequence"`
	GapFrames        uint64 `json:"gapFrames,omitempty"`
}

type audioStats struct {
	Frames             uint64 `json:"frames"`
	GapFrames          uint64 `json:"gapFrames"`
	InvalidFrames      uint64 `json:"invalidFrames"`
	Resets             uint64 `json:"resets"`
	FirstAudioOffsetMS int64  `json:"firstAudioOffsetMs,omitempty"`
	WAVBytes           int64  `json:"wavBytes"`
}

type audioWriter struct {
	mu          sync.Mutex
	startedAt   time.Time
	wav         *os.File
	events      *os.File
	eventWriter *bufio.Writer
	dataBytes   uint32
	bootID      string
	lastSeq     uint64
	haveSeq     bool
	stats       audioStats
}

func newAudioWriter(directory string, startedAt time.Time) (*audioWriter, error) {
	wav, err := os.OpenFile(directory+string(os.PathSeparator)+"m5-audio.wav", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create M5 audio WAV: %w", err)
	}
	if _, err := wav.Write(make([]byte, 44)); err != nil {
		_ = wav.Close()
		return nil, fmt.Errorf("reserve M5 audio WAV header: %w", err)
	}
	events, err := os.OpenFile(directory+string(os.PathSeparator)+"m5-audio-events.ndjson", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		_ = wav.Close()
		return nil, fmt.Errorf("create M5 audio event sidecar: %w", err)
	}
	return &audioWriter{startedAt: startedAt, wav: wav, events: events, eventWriter: bufio.NewWriterSize(events, 32*1024)}, nil
}

func parseAudioFrame(message []byte) (audioFrame, error) {
	parts := strings.Split(strings.TrimSpace(string(message)), ",")
	if len(parts) != 6 || parts[0] != "AUD:1" || parts[3] != "8" || parts[4] != "ima" {
		return audioFrame{}, fmt.Errorf("unsupported AUD frame")
	}
	if len(parts[1]) != 8 {
		return audioFrame{}, fmt.Errorf("invalid AUD boot ID")
	}
	if _, err := strconv.ParseUint(parts[1], 16, 32); err != nil {
		return audioFrame{}, fmt.Errorf("invalid AUD boot ID: %w", err)
	}
	sequence, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return audioFrame{}, fmt.Errorf("invalid AUD sequence: %w", err)
	}
	packet, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil || len(packet) != audioPacketBytes || packet[2] > 88 {
		return audioFrame{}, fmt.Errorf("invalid AUD payload")
	}
	frame := audioFrame{bootID: strings.ToLower(parts[1]), sequence: sequence}
	predictor := int(int16(binary.LittleEndian.Uint16(packet[:2])))
	stepIndex := int(packet[2])
	frame.samples[0] = int16(predictor)
	sampleIndex := 1
	decodeNibble := func(nibble byte) int16 {
		step := imaStepTable[stepIndex]
		difference := step >> 3
		if nibble&4 != 0 {
			difference += step
		}
		if nibble&2 != 0 {
			difference += step >> 1
		}
		if nibble&1 != 0 {
			difference += step >> 2
		}
		if nibble&8 != 0 {
			predictor -= difference
		} else {
			predictor += difference
		}
		predictor = clampInt(predictor, -32768, 32767)
		stepIndex = clampInt(stepIndex+imaIndexTable[nibble&0x0f], 0, 88)
		return int16(predictor)
	}
	for packetIndex := 4; packetIndex < len(packet) && sampleIndex < len(frame.samples); packetIndex++ {
		frame.samples[sampleIndex] = decodeNibble(packet[packetIndex] & 0x0f)
		sampleIndex++
		if sampleIndex < len(frame.samples) {
			frame.samples[sampleIndex] = decodeNibble(packet[packetIndex] >> 4)
			sampleIndex++
		}
	}
	if sampleIndex != len(frame.samples) {
		return audioFrame{}, fmt.Errorf("AUD payload produced %d samples", sampleIndex)
	}
	return frame, nil
}

func (writer *audioWriter) Write(message []byte, receivedAt time.Time) error {
	frame, err := parseAudioFrame(message)
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err != nil {
		writer.stats.InvalidFrames++
		return err
	}
	gap := uint64(0)
	if !writer.haveSeq || writer.bootID != frame.bootID || frame.sequence <= writer.lastSeq {
		if writer.haveSeq {
			writer.stats.Resets++
		}
		writer.bootID = frame.bootID
		writer.haveSeq = false
	} else if frame.sequence > writer.lastSeq+1 {
		gap = frame.sequence - writer.lastSeq - 1
		writer.stats.GapFrames += gap
		if gap <= maximumAudioGap {
			if err := writer.writeSilence(gap); err != nil {
				return err
			}
		}
	}
	if writer.stats.Frames == 0 {
		writer.stats.FirstAudioOffsetMS = receivedAt.Sub(writer.startedAt).Milliseconds()
	}
	buffer := make([]byte, len(frame.samples)*2)
	for index, sample := range frame.samples {
		binary.LittleEndian.PutUint16(buffer[index*2:], uint16(sample))
	}
	if _, err := writer.wav.Write(buffer); err != nil {
		return fmt.Errorf("write M5 audio WAV: %w", err)
	}
	writer.dataBytes += uint32(len(buffer))
	writer.stats.Frames++
	writer.bootID = frame.bootID
	writer.lastSeq = frame.sequence
	writer.haveSeq = true
	encoded, _ := json.Marshal(audioEvent{
		ReceivedOffsetMS: receivedAt.Sub(writer.startedAt).Milliseconds(), BootID: frame.bootID,
		Sequence: frame.sequence, GapFrames: gap,
	})
	if _, err := writer.eventWriter.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write M5 audio event sidecar: %w", err)
	}
	return nil
}

func (writer *audioWriter) writeSilence(frames uint64) error {
	if frames == 0 {
		return nil
	}
	bytes := make([]byte, int(frames)*audioFrameSamples*2)
	if _, err := writer.wav.Write(bytes); err != nil {
		return fmt.Errorf("write M5 audio gap: %w", err)
	}
	writer.dataBytes += uint32(len(bytes))
	return nil
}

func (writer *audioWriter) Close() (audioStats, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	var firstErr error
	if err := writer.eventWriter.Flush(); err != nil {
		firstErr = err
	}
	if err := writer.events.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if _, err := writer.wav.Seek(0, 0); err == nil {
		header := wavHeader(writer.dataBytes)
		if _, err := writer.wav.Write(header); err != nil && firstErr == nil {
			firstErr = err
		}
	} else if firstErr == nil {
		firstErr = err
	}
	if err := writer.wav.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	writer.stats.WAVBytes = int64(writer.dataBytes) + 44
	return writer.stats, firstErr
}

func wavHeader(dataBytes uint32) []byte {
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+dataBytes)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], audioSampleRate)
	binary.LittleEndian.PutUint32(header[28:32], audioSampleRate*2)
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataBytes)
	return header
}

func clampInt(value int, minimum int, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
