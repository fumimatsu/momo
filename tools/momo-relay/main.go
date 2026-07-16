package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

//go:embed web
var webAssets embed.FS

const (
	commandLabel   = "momo-command"
	telemetryLabel = "momo-telemetry"
	upstreamLabel  = "serial"
)

var h264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

type signalMessage struct {
	Type  string                   `json:"type"`
	SDP   string                   `json:"sdp,omitempty"`
	ICE   *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error string                   `json:"error,omitempty"`
}

type viewer struct {
	id        uint64
	role      string
	pc        *webrtc.PeerConnection
	telemetry atomic.Pointer[webrtc.DataChannel]
	command   atomic.Pointer[webrtc.DataChannel]
}

type relay struct {
	name                 string
	upstreamURL          string
	allowObserverCommand bool

	videoTrack *webrtc.TrackLocalStaticRTP
	api        *webrtc.API

	viewersMu sync.RWMutex
	viewers   map[uint64]*viewer
	nextID    atomic.Uint64
	pilotID   uint64

	upstreamMu   sync.RWMutex
	upstreamPC   *webrtc.PeerConnection
	upstreamDC   *webrtc.DataChannel
	upstreamSSRC atomic.Uint32
}

type relayServer struct {
	sources map[string]*relay
}

type sourceFlag []string

func (values *sourceFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *sourceFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func newRelay(name string, upstreamURL string, allowObserverCommand bool) (*relay, error) {
	api, err := newH264API()
	if err != nil {
		return nil, err
	}
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(h264Codec, "video", "momo")
	if err != nil {
		return nil, fmt.Errorf("create local H264 track: %w", err)
	}
	return &relay{
		name:                 name,
		upstreamURL:          upstreamURL,
		allowObserverCommand: allowObserverCommand,
		videoTrack:           videoTrack,
		api:                  api,
		viewers:              make(map[uint64]*viewer),
	}, nil
}

func newH264API() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    h264Codec.MimeType,
			ClockRate:   h264Codec.ClockRate,
			SDPFmtpLine: h264Codec.SDPFmtpLine,
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register H264 codec: %w", err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)), nil
}

func (r *relay) start(ctx context.Context) {
	go func() {
		for {
			if err := r.connectUpstream(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("upstream disconnected: %v; retrying in 3 seconds", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

func (r *relay) connectUpstream(ctx context.Context) error {
	log.Printf("source %q: connecting upstream Momo: %s", r.name, r.upstreamURL)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, r.upstreamURL, nil)
	if err != nil {
		return fmt.Errorf("connect upstream signaling: %w", err)
	}
	defer ws.Close()

	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("create upstream peer connection: %w", err)
	}
	defer pc.Close()

	var writeMu sync.Mutex
	sendSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if err := sendSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
			log.Printf("send upstream ICE candidate: %v", err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("source %q: upstream peer connection state: %s", r.name, state.String())
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
			log.Printf("ignore unsupported upstream video codec: %s", track.Codec().MimeType)
			return
		}
		r.upstreamSSRC.Store(uint32(track.SSRC()))
		log.Printf("source %q: receiving upstream H264 track: SSRC=%d codec=%s", r.name, track.SSRC(), track.Codec().SDPFmtpLine)
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("upstream H264 RTP ended: %v", err)
				return
			}
			if err := r.videoTrack.WriteRTP(packet); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				log.Printf("fan out upstream RTP: %v", err)
			}
		}
	})

	upstreamDC, err := pc.CreateDataChannel(upstreamLabel, nil)
	if err != nil {
		return fmt.Errorf("create upstream data channel: %w", err)
	}
	upstreamDC.OnOpen(func() {
		r.upstreamMu.Lock()
		r.upstreamPC = pc
		r.upstreamDC = upstreamDC
		r.upstreamMu.Unlock()
		log.Printf("source %q: upstream DataChannel %q opened", r.name, upstreamLabel)
	})
	upstreamDC.OnMessage(func(message webrtc.DataChannelMessage) {
		r.broadcastTelemetry(message)
	})
	upstreamDC.OnClose(func() {
		r.clearUpstream(pc)
		log.Printf("source %q: upstream DataChannel %q closed", r.name, upstreamLabel)
	})

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	if err != nil {
		return fmt.Errorf("add upstream recvonly video transceiver: %w", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create upstream offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set upstream local description: %w", err)
	}
	if err := sendSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		return fmt.Errorf("send upstream offer: %w", err)
	}

	var pendingCandidates []webrtc.ICECandidateInit
	remoteDescriptionSet := false
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			r.clearUpstream(pc)
			return fmt.Errorf("read upstream signaling: %w", err)
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			log.Printf("ignore malformed upstream signaling: %v", err)
			continue
		}
		switch message.Type {
		case "answer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set upstream answer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("apply pending upstream ICE candidate: %v", err)
				}
			}
			pendingCandidates = nil
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := pc.AddICECandidate(*message.ICE); err != nil {
				log.Printf("apply upstream ICE candidate: %v", err)
			}
		case "close", "bye":
			r.clearUpstream(pc)
			return errors.New("upstream closed signaling session")
		}
	}
}

func (r *relay) clearUpstream(pc *webrtc.PeerConnection) {
	r.upstreamMu.Lock()
	defer r.upstreamMu.Unlock()
	if r.upstreamPC == pc {
		r.upstreamPC = nil
		r.upstreamDC = nil
	}
}

func (r *relay) broadcastTelemetry(message webrtc.DataChannelMessage) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if channel := client.telemetry.Load(); channel != nil {
			if err := sendDataChannel(channel, message); err != nil {
				log.Printf("send telemetry to viewer %d: %v", client.id, err)
			}
		}
	}
}

func (r *relay) broadcastCommand(message webrtc.DataChannelMessage) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if channel := client.command.Load(); channel != nil {
			if err := sendDataChannel(channel, message); err != nil {
				log.Printf("send command audit to viewer %d: %v", client.id, err)
			}
		}
	}
}

func sendDataChannel(channel *webrtc.DataChannel, message webrtc.DataChannelMessage) error {
	if message.IsString {
		return channel.SendText(string(message.Data))
	}
	return channel.Send(message.Data)
}

func (r *relay) handleCommand(client *viewer, message webrtc.DataChannelMessage) {
	if client.role != "pilot" && !r.allowObserverCommand {
		log.Printf("drop command from observer viewer %d", client.id)
		return
	}
	r.upstreamMu.RLock()
	upstream := r.upstreamDC
	r.upstreamMu.RUnlock()
	if upstream == nil {
		log.Printf("drop command from viewer %d: upstream DataChannel is unavailable", client.id)
		return
	}
	if err := sendDataChannel(upstream, message); err != nil {
		log.Printf("forward command from viewer %d to Momo: %v", client.id, err)
		return
	}
	// コマンドは全員に同じ DataChannel で返す。クライアント側は受信時にのみ
	// 表示するため、この監査メッセージが Momo に再送されることはない。
	r.broadcastCommand(message)
}

func (r *relay) addViewer(client *viewer) {
	r.viewersMu.Lock()
	r.viewers[client.id] = client
	r.viewersMu.Unlock()
}

func (r *relay) reservePilot(id uint64) bool {
	r.viewersMu.Lock()
	defer r.viewersMu.Unlock()
	if r.pilotID != 0 {
		return false
	}
	r.pilotID = id
	return true
}

func (r *relay) removeViewer(id uint64) {
	r.viewersMu.Lock()
	delete(r.viewers, id)
	if r.pilotID == id {
		r.pilotID = 0
	}
	r.viewersMu.Unlock()
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
}

func (server *relayServer) serveViewerWS(w http.ResponseWriter, req *http.Request) {
	device := req.URL.Query().Get("device")
	if device == "" && len(server.sources) == 1 {
		for device = range server.sources {
		}
	}
	source, ok := server.sources[device]
	if !ok {
		if device == "" {
			http.Error(w, "device is required when multiple Momo sources are configured", http.StatusBadRequest)
			return
		}
		http.Error(w, "unknown device: "+device, http.StatusNotFound)
		return
	}
	source.serveViewerWS(w, req)
}

func (r *relay) serveViewerWS(w http.ResponseWriter, req *http.Request) {
	role := req.URL.Query().Get("role")
	if role != "pilot" {
		role = "observer"
	}
	client := &viewer{id: r.nextID.Add(1), role: role}
	if role == "pilot" && !r.reservePilot(client.id) {
		http.Error(w, "a pilot viewer is already connected", http.StatusConflict)
		return
	}
	defer r.removeViewer(client.id)
	ws, err := wsUpgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("upgrade viewer signaling: %v", err)
		return
	}
	defer ws.Close()

	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = ws.WriteJSON(signalMessage{Type: "error", Error: err.Error()})
		return
	}
	defer pc.Close()

	client.pc = pc
	var writeMu sync.Mutex
	sendSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if err := sendSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
			log.Printf("send viewer %d ICE candidate: %v", client.id, err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("viewer %d (%s) peer connection state: %s", client.id, client.role, state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			r.removeViewer(client.id)
		}
	})
	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		switch channel.Label() {
		case telemetryLabel:
			channel.OnOpen(func() {
				client.telemetry.Store(channel)
				log.Printf("viewer %d telemetry channel opened", client.id)
			})
			channel.OnClose(func() { client.telemetry.CompareAndSwap(channel, nil) })
		case commandLabel:
			channel.OnOpen(func() {
				client.command.Store(channel)
				log.Printf("viewer %d command channel opened", client.id)
			})
			channel.OnMessage(func(message webrtc.DataChannelMessage) {
				r.handleCommand(client, message)
			})
			channel.OnClose(func() { client.command.CompareAndSwap(channel, nil) })
		default:
			log.Printf("viewer %d opened unsupported DataChannel %q", client.id, channel.Label())
		}
	})

	_, err = pc.AddTrack(r.videoTrack)
	if err != nil {
		_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
		return
	}

	remoteDescriptionSet := false
	var pendingCandidates []webrtc.ICECandidateInit
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			r.removeViewer(client.id)
			return
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			_ = sendSignal(signalMessage{Type: "error", Error: "invalid signaling JSON"})
			continue
		}
		switch message.Type {
		case "offer":
			if remoteDescriptionSet {
				_ = sendSignal(signalMessage{Type: "error", Error: "renegotiation is not supported"})
				continue
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
				continue
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("apply pending viewer %d ICE candidate: %v", client.id, err)
				}
			}
			pendingCandidates = nil
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
				continue
			}
			r.addViewer(client)
			if err := sendSignal(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return
			}
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := pc.AddICECandidate(*message.ICE); err != nil {
				log.Printf("apply viewer %d ICE candidate: %v", client.id, err)
			}
		case "close", "bye":
			r.removeViewer(client.id)
			return
		}
	}
}

func parseSource(value string) (string, string, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("source must be DEVICE=WS_URL: %q", value)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func main() {
	var upstream string
	var listen string
	var allowObserverCommand bool
	var sources sourceFlag
	flag.StringVar(&upstream, "upstream", "", "Momo P2P WebSocket URL, for example ws://192.168.11.3:8080/ws")
	flag.Var(&sources, "source", "Momo source as DEVICE=WS_URL; can be repeated")
	flag.StringVar(&listen, "listen", ":8090", "HTTP and WebSocket listen address")
	flag.BoolVar(&allowObserverCommand, "allow-observer-command", false, "allow observer viewers to send commands to Momo")
	flag.Parse()
	if upstream != "" {
		sources = append(sources, "default="+upstream)
	}
	if len(sources) == 0 {
		log.Fatal("-upstream or at least one -source is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverRelay := &relayServer{sources: make(map[string]*relay, len(sources))}
	for _, sourceValue := range sources {
		name, sourceURL, err := parseSource(sourceValue)
		if err != nil {
			log.Fatal(err)
		}
		if _, exists := serverRelay.sources[name]; exists {
			log.Fatalf("duplicate source name: %q", name)
		}
		relay, err := newRelay(name, sourceURL, allowObserverCommand)
		if err != nil {
			log.Fatal(err)
		}
		serverRelay.sources[name] = relay
		relay.start(ctx)
	}

	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	fileServer := http.FileServer(http.FS(webRoot))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, req)
	}))
	mux.HandleFunc("/pilot", func(w http.ResponseWriter, req *http.Request) {
		target := "/pilot.html"
		if req.URL.RawQuery != "" {
			target += "?" + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/ws", serverRelay.serveViewerWS)
	server := &http.Server{Addr: listen, Handler: mux}
	log.Printf("Momo relay is listening on http://%s/ for sources: %s", listen, strings.Join(sources, ", "))
	log.Fatal(server.ListenAndServe())
}
