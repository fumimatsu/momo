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
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var h264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

type config struct {
	listen          string
	fps             int
	packetsPerFrame int
	payloadBytes    int
	telemetryHz     int
}

type signalMessage struct {
	Type string                   `json:"type"`
	SDP  string                   `json:"sdp,omitempty"`
	ICE  *webrtc.ICECandidateInit `json:"ice,omitempty"`
}

type simulator struct {
	config           config
	sessionsMu       sync.Mutex
	sessions         map[*session]struct{}
	activeSessions   atomic.Int64
	totalSessions    atomic.Uint64
	framesSent       atomic.Uint64
	packetsSent      atomic.Uint64
	payloadBytesSent atomic.Uint64
	telemetrySent    atomic.Uint64
}

type session struct {
	path    string
	ws      *websocket.Conn
	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticRTP
	cancel  context.CancelFunc
	writeMu sync.Mutex
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:18080", "HTTP/WebSocket listen address")
	flag.IntVar(&cfg.fps, "fps", 30, "synthetic H264 frame rate")
	flag.IntVar(&cfg.packetsPerFrame, "packets-per-frame", 8, "RTP packets sent for each frame")
	flag.IntVar(&cfg.payloadBytes, "payload-bytes", 1200, "payload bytes in each RTP packet")
	flag.IntVar(&cfg.telemetryHz, "telemetry-hz", 15, "TEL state messages per second; 0 disables telemetry")
	flag.Parse()
	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}

	sim := &simulator{config: cfg, sessions: make(map[*session]struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", sim.serveHealth)
	mux.HandleFunc("/api/v1/status", sim.serveStatus)
	mux.HandleFunc("/api/v1/disconnect", sim.serveDisconnect)
	mux.HandleFunc("/ws", sim.serveWebSocket)
	mux.HandleFunc("/ws/", sim.serveWebSocket)
	server := &http.Server{Addr: cfg.listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	bitrateMbps := float64(cfg.fps*cfg.packetsPerFrame*cfg.payloadBytes*8) / 1_000_000
	log.Printf("Momo Relay simulator listening on %s (fps=%d packets/frame=%d payload=%d, %.2f Mbps/session)",
		cfg.listen, cfg.fps, cfg.packetsPerFrame, cfg.payloadBytes, bitrateMbps)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (sim *simulator) serveDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	source := r.URL.Query().Get("source")
	closed := 0
	sim.sessionsMu.Lock()
	matching := make([]*session, 0, len(sim.sessions))
	for current := range sim.sessions {
		if source != "" && current.path != "/ws/"+source {
			continue
		}
		matching = append(matching, current)
	}
	sim.sessionsMu.Unlock()
	for _, current := range matching {
		current.cancel()
		_ = current.pc.Close()
		_ = current.ws.Close()
		closed++
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"closed": closed, "source": source})
}

func validateConfig(cfg config) error {
	if cfg.listen == "" {
		return errors.New("-listen must not be empty")
	}
	if cfg.fps < 1 || cfg.fps > 120 {
		return errors.New("-fps must be in 1..120")
	}
	if cfg.packetsPerFrame < 1 || cfg.packetsPerFrame > 128 {
		return errors.New("-packets-per-frame must be in 1..128")
	}
	if cfg.payloadBytes < 64 || cfg.payloadBytes > 1400 {
		return errors.New("-payload-bytes must be in 64..1400")
	}
	if cfg.telemetryHz < 0 || cfg.telemetryHz > 120 {
		return errors.New("-telemetry-hz must be in 0..120")
	}
	return nil
}

func (sim *simulator) serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (sim *simulator) serveStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"activeSessions":   sim.activeSessions.Load(),
		"totalSessions":    sim.totalSessions.Load(),
		"framesSent":       sim.framesSent.Load(),
		"packetsSent":      sim.packetsSent.Load(),
		"payloadBytesSent": sim.payloadBytesSent.Load(),
		"telemetrySent":    sim.telemetrySent.Load(),
		"fps":              sim.config.fps,
		"packetsPerFrame":  sim.config.packetsPerFrame,
		"payloadBytes":     sim.config.payloadBytes,
	})
}

func (sim *simulator) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade %s: %v", r.URL.Path, err)
		return
	}
	defer ws.Close()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Printf("create peer for %s: %v", r.URL.Path, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	s := &session{path: r.URL.Path, ws: ws, pc: pc, cancel: cancel}
	sim.sessionsMu.Lock()
	sim.sessions[s] = struct{}{}
	sim.sessionsMu.Unlock()
	defer func() {
		cancel()
		_ = pc.Close()
		sim.sessionsMu.Lock()
		delete(sim.sessions, s)
		sim.sessionsMu.Unlock()
		sim.activeSessions.Add(-1)
	}()

	track, err := webrtc.NewTrackLocalStaticRTP(h264Codec, "video", "momo-relay-sim")
	if err != nil {
		log.Printf("create track for %s: %v", r.URL.Path, err)
		return
	}
	s.track = track
	sender, err := pc.AddTrack(track)
	if err != nil {
		log.Printf("add track for %s: %v", r.URL.Path, err)
		return
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buffer); err != nil {
				return
			}
		}
	}()

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if err := s.writeJSON(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
			log.Printf("send candidate for %s: %v", r.URL.Path, err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			go sim.writeVideo(ctx, track)
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			cancel()
		}
	})
	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		if channel.Label() != "serial" || sim.config.telemetryHz == 0 {
			return
		}
		channel.OnOpen(func() { go sim.writeTelemetry(ctx, channel) })
	})

	sim.activeSessions.Add(1)
	sim.totalSessions.Add(1)
	log.Printf("source connected: path=%s active=%d", r.URL.Path, sim.activeSessions.Load())
	for {
		var message signalMessage
		if err := ws.ReadJSON(&message); err != nil {
			return
		}
		switch message.Type {
		case "offer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				log.Printf("set offer for %s: %v", r.URL.Path, err)
				return
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Printf("create answer for %s: %v", r.URL.Path, err)
				return
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Printf("set answer for %s: %v", r.URL.Path, err)
				return
			}
			if err := s.writeJSON(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return
			}
		case "candidate":
			if message.ICE != nil {
				if err := pc.AddICECandidate(*message.ICE); err != nil {
					log.Printf("add candidate for %s: %v", r.URL.Path, err)
				}
			}
		case "close", "bye":
			return
		}
	}
}

func (s *session) writeJSON(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.ws.WriteJSON(value)
}

func (sim *simulator) writeVideo(ctx context.Context, track *webrtc.TrackLocalStaticRTP) {
	interval := time.Second / time.Duration(sim.config.fps)
	timestampStep := uint32(h264Codec.ClockRate / uint32(sim.config.fps))
	payload := make([]byte, sim.config.payloadBytes)
	payload[0] = 0x41
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var sequence uint16
	var timestamp uint32
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for packetIndex := 0; packetIndex < sim.config.packetsPerFrame; packetIndex++ {
				packet := &rtp.Packet{Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: sequence,
					Timestamp:      timestamp,
					SSRC:           1,
					Marker:         packetIndex == sim.config.packetsPerFrame-1,
				}, Payload: payload}
				if err := track.WriteRTP(packet); err != nil {
					return
				}
				sequence++
				sim.packetsSent.Add(1)
				sim.payloadBytesSent.Add(uint64(len(payload)))
			}
			timestamp += timestampStep
			sim.framesSent.Add(1)
		}
	}
}

func (sim *simulator) writeTelemetry(ctx context.Context, channel *webrtc.DataChannel) {
	ticker := time.NewTicker(time.Second / time.Duration(sim.config.telemetryHz))
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			sequence++
			payload := fmt.Sprintf(`TEL:{"v":3,"k":"s","boot":1,"seq":%d,"t":%d}`, sequence, now.UnixMilli())
			if err := channel.SendText(payload); err != nil {
				return
			}
			sim.telemetrySent.Add(1)
		}
	}
}
