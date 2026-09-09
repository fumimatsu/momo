package main

import (
	"net/url"
	"testing"
)

func TestRelayEndpointReadOnly(t *testing.T) {
	value, err := relayEndpoint("ws://127.0.0.1:8090/ws?device=virtual-01&role=pilot&client=web-pilot")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(value)
	if u.Query().Get("role") != "observer" || u.Query().Get("client") != "spectator-publisher" {
		t.Fatal("not a publisher observer")
	}
	for _, bad := range []string{"", "https://example/ws?device=x", "ws://host/ws", "ws://host/control?device=x", "ws://secret@host/ws?device=x"} {
		if _, err := relayEndpoint(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
