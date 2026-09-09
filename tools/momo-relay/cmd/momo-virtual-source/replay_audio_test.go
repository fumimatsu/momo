package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAudioReplayLoopIdentity(t *testing.T) {
	raw := "AUD:1,12345678,20,8,ima," + base64.StdEncoding.EncodeToString(make([]byte, 84))
	a, b, err := replayAudio(raw, [2]string{"11111111", "22222222"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "AUD:1,11111111,7,") || !strings.HasPrefix(b, "AUD:1,22222222,7,") {
		t.Fatal("loop audio identity not rewritten")
	}
	for _, bad := range []string{raw + ",extra", strings.Replace(raw, "8,ima", "48,pcm", 1), "AUD:1,12345678,1,8,ima,AAAA"} {
		if _, _, err := replayAudio(bad, [2]string{}, 0); err == nil {
			t.Fatal("accepted invalid audio")
		}
	}
}
