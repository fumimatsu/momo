package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

type signalMessage struct {
	Type string                   `json:"type"`
	Data string                   `json:"data,omitempty"`
	Error string                   `json:"error,omitempty"`
	SDP  string                   `json:"sdp,omitempty"`
	ICE  *webrtc.ICECandidateInit `json:"ice,omitempty"`
}

type loadClient struct {
	id              string
	role            string
	connected       atomic.Bool
	commandOpen     atomic.Bool
	driveOpen       atomic.Bool
	commandsSent    atomic.Uint64
	rtpPackets      atomic.Uint64
	rtpFrames       atomic.Uint64
	telemetry       atomic.Uint64
	lastRTPUnixNano atomic.Int64
}

type loadRunner struct {
	clients []*loadClient
}

func main() {
	var relayURL string
	var sourceCount int
	var observersPerSource int
	var pilotSource string
	var listen string
	flag.StringVar(&relayURL, "relay-url", "http://127.0.0.1:18090", "Relay HTTP base URL")
	flag.IntVar(&sourceCount, "source-count", 4, "simulated source count")
	flag.IntVar(&observersPerSource, "observers-per-source", 1, "Observer PeerConnections per source")
	flag.StringVar(&pilotSource, "pilot-source", "", "optional source ID that receives one Pilot client")
	flag.StringVar(&listen, "listen", "127.0.0.1:18100", "status HTTP listen address")
	flag.Parse()
	if sourceCount < 1 || sourceCount > 32 || observersPerSource < 0 || observersPerSource > 8 || (observersPerSource == 0 && pilotSource == "") {
		log.Fatal("-source-count must be in 1..32; observers must be in 0..8 and a zero value requires -pilot-source")
	}
	parsed, err := url.Parse(relayURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		log.Fatal("-relay-url must be an absolute http:// or https:// URL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := &loadRunner{}
	for sourceIndex := 1; sourceIndex <= sourceCount; sourceIndex++ {
		for observerIndex := 1; observerIndex <= observersPerSource; observerIndex++ {
			client := &loadClient{id: fmt.Sprintf("sim-%02d/observer-%d", sourceIndex, observerIndex), role: "observer"}
			runner.clients = append(runner.clients, client)
			device := fmt.Sprintf("sim-%02d", sourceIndex)
			go client.run(ctx, parsed, device)
		}
	}
	if pilotSource != "" {
		client := &loadClient{id: pilotSource + "/pilot", role: "pilot"}
		runner.clients = append(runner.clients, client)
		go client.run(ctx, parsed, pilotSource)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/api/v1/status", runner.serveStatus)
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("Relay Observer load listening on %s with %d clients", listen, len(runner.clients))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (runner *loadRunner) serveStatus(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	clients := make([]map[string]any, 0, len(runner.clients))
	connected := 0
	var packets uint64
	var frames uint64
	var telemetry uint64
	for _, client := range runner.clients {
		if client.connected.Load() {
			connected++
		}
		lastRTP := client.lastRTPUnixNano.Load()
		packets += client.rtpPackets.Load()
		frames += client.rtpFrames.Load()
		telemetry += client.telemetry.Load()
		clients = append(clients, map[string]any{
			"id": client.id, "role": client.role, "connected": client.connected.Load(),
			"commandOpen": client.commandOpen.Load(), "driveOpen": client.driveOpen.Load(),
			"commandsSent": client.commandsSent.Load(),
			"rtpPackets":   client.rtpPackets.Load(), "rtpFrames": client.rtpFrames.Load(),
			"telemetry": client.telemetry.Load(),
			"lastRtpAgeMs": func() any {
				if lastRTP == 0 {
					return nil
				}
				return now.Sub(time.Unix(0, lastRTP)).Milliseconds()
			}(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"clientCount": len(runner.clients), "connectedCount": connected,
		"rtpPackets": packets, "rtpFrames": frames, "telemetry": telemetry,
		"clients": clients,
	})
}

func (client *loadClient) run(ctx context.Context, relayURL *url.URL, device string) {
	for ctx.Err() == nil {
		if err := client.connect(ctx, relayURL, device); err != nil && ctx.Err() == nil {
			log.Printf("client %s: %v; retrying", client.id, err)
		}
		client.connected.Store(false)
		client.commandOpen.Store(false)
		client.driveOpen.Store(false)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (client *loadClient) connect(ctx context.Context, relayURL *url.URL, device string) error {
	wsURL := *relayURL
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = "/ws"
	query := wsURL.Query()
	query.Set("device", device)
	query.Set("role", client.role)
	query.Set("client", "web-"+client.role)
	wsURL.RawQuery = query.Encode()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	defer ws.Close()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("create peer: %w", err)
	}
	defer pc.Close()
	var writeMu sync.Mutex
	writeSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		value := candidate.ToJSON()
		_ = writeSignal(signalMessage{Type: "candidate", ICE: &value})
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		client.connected.Store(state == webrtc.PeerConnectionStateConnected)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			_ = ws.Close()
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				client.rtpPackets.Add(1)
				if packet.Marker {
					client.rtpFrames.Add(1)
				}
				client.lastRTPUnixNano.Store(time.Now().UnixNano())
			}
		}()
	})
	if client.role == "pilot" {
		command, err := pc.CreateDataChannel("momo-command", &webrtc.DataChannelInit{Ordered: boolPointer(false), MaxRetransmits: uint16Pointer(0)})
		if err != nil {
			return fmt.Errorf("create command channel: %w", err)
		}
		drive, err := pc.CreateDataChannel("momo-drive", &webrtc.DataChannelInit{Ordered: boolPointer(true)})
		if err != nil {
			return fmt.Errorf("create drive channel: %w", err)
		}
		command.OnOpen(func() {
			client.commandOpen.Store(true)
			go client.writeCommands(ctx, command)
		})
		command.OnClose(func() { client.commandOpen.Store(false) })
		drive.OnOpen(func() {
			client.driveOpen.Store(true)
			_ = drive.SendText("DRIVE:1")
			_ = drive.SendText("GEAR:1")
		})
		drive.OnClose(func() { client.driveOpen.Store(false) })
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("add video transceiver: %w", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set offer: %w", err)
	}
	if err := writeSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		return err
	}

	remoteSet := false
	pending := []webrtc.ICECandidateInit{}
	for {
		var message signalMessage
		if err := ws.ReadJSON(&message); err != nil {
			return err
		}
		switch message.Type {
		case "answer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				return err
			}
			remoteSet = true
			for _, candidate := range pending {
				_ = pc.AddICECandidate(candidate)
			}
			pending = nil
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteSet {
				pending = append(pending, *message.ICE)
				continue
			}
			_ = pc.AddICECandidate(*message.ICE)
		case "telemetry":
			client.telemetry.Add(1)
		case "error":
			return fmt.Errorf("Relay signaling error: %s", message.Error)
		}
	}
}

func (client *loadClient) writeCommands(ctx context.Context, channel *webrtc.DataChannel) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := channel.SendText("S:1500,T:1000\n"); err != nil {
				return
			}
			client.commandsSent.Add(1)
		}
	}
}

func boolPointer(value bool) *bool { return &value }

func uint16Pointer(value uint16) *uint16 { return &value }
