package main

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

func replayAudio(raw string, boots [2]string, sequence int) (string, string, error) {
	p := strings.Split(raw, ",")
	if len(p) != 6 || p[0] != "AUD:1" || p[3] != "8" || p[4] != "ima" || len(p[1]) != 8 {
		return "", "", fmt.Errorf("unsupported AUD frame")
	}
	if _, e := strconv.ParseUint(p[1], 16, 32); e != nil {
		return "", "", e
	}
	if _, e := strconv.ParseUint(p[2], 10, 64); e != nil {
		return "", "", e
	}
	b, e := base64.StdEncoding.DecodeString(p[5])
	if e != nil || len(b) != 84 || b[2] > 88 || b[3] != 0 {
		return "", "", fmt.Errorf("invalid IMA frame")
	}
	return fmt.Sprintf("AUD:1,%s,%d,8,ima,%s", boots[0], sequence, p[5]), fmt.Sprintf("AUD:1,%s,%d,8,ima,%s", boots[1], sequence, p[5]), nil
}
