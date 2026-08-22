package main

import (
	"fmt"
)

type h264AccessUnit struct {
	data     []byte
	keyframe bool
}

func splitH264AccessUnits(data []byte) ([]h264AccessUnit, error) {
	nalus := splitAnnexBNALUs(data)
	if len(nalus) == 0 {
		return nil, fmt.Errorf("input does not contain Annex-B NAL units")
	}

	var units []h264AccessUnit
	var current []byte
	currentHasVCL := false
	currentKeyframe := false
	flush := func() {
		if !currentHasVCL {
			return
		}
		units = append(units, h264AccessUnit{
			data:     append([]byte(nil), current...),
			keyframe: currentKeyframe,
		})
		current = nil
		currentHasVCL = false
		currentKeyframe = false
	}

	for _, nalu := range nalus {
		naluType := nalu.payload[0] & 0x1f
		if naluType == 9 && currentHasVCL {
			flush()
		}
		current = append(current, nalu.annexB...)
		if naluType >= 1 && naluType <= 5 {
			currentHasVCL = true
			currentKeyframe = currentKeyframe || naluType == 5
		}
	}
	flush()

	if len(units) == 0 {
		return nil, fmt.Errorf("input does not contain H.264 video access units")
	}
	if !units[0].keyframe {
		return nil, fmt.Errorf("first H.264 access unit must be an IDR keyframe")
	}
	return units, nil
}

type annexBNALU struct {
	annexB  []byte
	payload []byte
}

func splitAnnexBNALUs(data []byte) []annexBNALU {
	starts := annexBStartCodes(data)
	if len(starts) == 0 {
		return nil
	}
	result := make([]annexBNALU, 0, len(starts))
	for index, start := range starts {
		end := len(data)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		prefixLength := 3
		if start+3 < len(data) && data[start+2] == 0 && data[start+3] == 1 {
			prefixLength = 4
		}
		payloadStart := start + prefixLength
		if payloadStart >= end {
			continue
		}
		payload := data[payloadStart:end]
		if len(payload) == 0 {
			continue
		}
		annexB := make([]byte, 4+len(payload))
		copy(annexB, []byte{0, 0, 0, 1})
		copy(annexB[4:], payload)
		result = append(result, annexBNALU{annexB: annexB, payload: payload})
	}
	return result
}

func annexBStartCodes(data []byte) []int {
	var starts []int
	for index := 0; index+3 <= len(data); {
		if index+4 <= len(data) && data[index] == 0 && data[index+1] == 0 && data[index+2] == 0 && data[index+3] == 1 {
			starts = append(starts, index)
			index += 4
			continue
		}
		if data[index] == 0 && data[index+1] == 0 && data[index+2] == 1 {
			starts = append(starts, index)
			index += 3
			continue
		}
		index++
	}
	return starts
}
