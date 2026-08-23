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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	virtualSourcePrefix   = "/ws/"
	maximumVirtualSources = 64
)

var virtualH264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	RTCPFeedback: []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	},
}

type signalMessage struct {
	Type  string                   `json:"type"`
	SDP   string                   `json:"sdp,omitempty"`
	ICE   *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error string                   `json:"error,omitempty"`
}

type virtualSourceServer struct {
	api           *webrtc.API
	accessUnits   []h264AccessUnit
	frameDuration time.Duration
	allowed       map[string]struct{}
	playback      map[string]playbackProfile

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

type playbackProfile struct {
	startIndex int
}

type playbackProfileStatus struct {
	SourceID      string `json:"sourceId"`
	StartFrame    int    `json:"startFrame"`
	StartOffsetMS int64  `json:"startOffsetMs"`
}

func main() {
	var listen string
	var input string
	var sourceList string
	var fps int
	var spreadStarts bool
	var spreadStartMaxPercent int
	flag.StringVar(&listen, "listen", "127.0.0.1:18880", "HTTP and WebSocket listen address")
	flag.StringVar(&input, "input", "", "H.264 Annex-B input with AUD NAL units")
	flag.StringVar(&sourceList, "sources", "virtual-01,virtual-02,virtual-03,virtual-04,virtual-05", "comma-separated source IDs")
	flag.IntVar(&fps, "fps", 30, "playback frame rate")
	flag.BoolVar(&spreadStarts, "spread-starts", false, "spread source start positions across input keyframes")
	flag.IntVar(&spreadStartMaxPercent, "spread-start-max-percent", 0, "optional inclusive maximum input position for spread starts (1-100; 0 uses the full legacy spread)")
	flag.Parse()

	if input == "" || fps < 1 || fps > 120 || spreadStartMaxPercent < 0 || spreadStartMaxPercent > 100 {
		log.Fatal("-input is required, -fps must be between 1 and 120, and -spread-start-max-percent must be 0 to 100")
	}
	data, err := os.ReadFile(input)
	if err != nil {
		log.Fatalf("read H.264 input: %v", err)
	}
	accessUnits, err := splitH264AccessUnits(data)
	if err != nil {
		log.Fatalf("parse H.264 input: %v", err)
	}
	sourceIDs, err := parseSourceIDs(sourceList)
	if err != nil {
		log.Fatal(err)
	}
	api, err := newVirtualSourceAPI()
	if err != nil {
		log.Fatal(err)
	}
	server := &virtualSourceServer{
		api:           api,
		accessUnits:   accessUnits,
		frameDuration: time.Second / time.Duration(fps),
		allowed:       make(map[string]struct{}, len(sourceIDs)),
		playback:      buildPlaybackProfiles(accessUnits, sourceIDs, spreadStarts, spreadStartMaxPercent),
		active:        make(map[string]context.CancelFunc),
	}
	for _, sourceID := range sourceIDs {
		server.allowed[sourceID] = struct{}{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.serveHealth)
	mux.HandleFunc(virtualSourcePrefix, server.serveWebSocket)
	httpServer := &http.Server{Addr: listen, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("virtual Momo source listening on http://%s for %s (%d frames at %d fps, spread starts=%t, spread max=%d%%)", listen, strings.Join(sourceIDs, ", "), len(accessUnits), fps, spreadStarts, spreadStartMaxPercent)
	for _, sourceID := range sourceIDs {
		profile := server.playback[sourceID]
		log.Printf("source %q playback starts at frame %d (%s)", sourceID, profile.startIndex, time.Duration(profile.startIndex)*server.frameDuration)
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func parseSourceIDs(value string) ([]string, error) {
	seen := make(map[string]struct{})
	var result []string
	for _, raw := range strings.Split(value, ",") {
		sourceID := strings.TrimSpace(raw)
		if sourceID == "" || strings.ContainsAny(sourceID, "/?#\\") {
			return nil, fmt.Errorf("invalid source ID: %q", raw)
		}
		if _, exists := seen[sourceID]; exists {
			return nil, fmt.Errorf("duplicate source ID: %s", sourceID)
		}
		seen[sourceID] = struct{}{}
		result = append(result, sourceID)
	}
	if len(result) == 0 || len(result) > maximumVirtualSources {
		return nil, fmt.Errorf("sources must contain 1 to %d IDs", maximumVirtualSources)
	}
	sort.Strings(result)
	return result, nil
}

func buildPlaybackProfiles(units []h264AccessUnit, sourceIDs []string, spread bool, maximumPercent int) map[string]playbackProfile {
	profiles := make(map[string]playbackProfile, len(sourceIDs))
	if len(sourceIDs) == 0 {
		return profiles
	}
	keyframes := make([]int, 0)
	for index, unit := range units {
		if unit.keyframe {
			keyframes = append(keyframes, index)
		}
	}
	if len(keyframes) == 0 {
		keyframes = append(keyframes, 0)
	}
	for index, sourceID := range sourceIDs {
		startIndex := keyframes[0]
		if spread {
			keyframeIndex := index * len(keyframes) / len(sourceIDs)
			if maximumPercent > 0 && len(sourceIDs) > 1 {
				maximumKeyframeIndex := (len(keyframes) - 1) * maximumPercent / 100
				keyframeIndex = index * maximumKeyframeIndex / (len(sourceIDs) - 1)
			}
			if keyframeIndex >= len(keyframes) {
				keyframeIndex = len(keyframes) - 1
			}
			startIndex = keyframes[keyframeIndex]
		}
		profiles[sourceID] = playbackProfile{startIndex: startIndex}
	}
	return profiles
}

func newVirtualSourceAPI() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: virtualH264Codec,
		PayloadType:        102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register H.264 codec: %w", err)
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	), nil
}

func (server *virtualSourceServer) serveHealth(w http.ResponseWriter, _ *http.Request) {
	server.mu.Lock()
	active := make([]string, 0, len(server.active))
	for sourceID := range server.active {
		active = append(active, sourceID)
	}
	server.mu.Unlock()
	sort.Strings(active)
	profiles := make([]playbackProfileStatus, 0, len(server.allowed))
	for sourceID := range server.allowed {
		profile := server.playback[sourceID]
		profiles = append(profiles, playbackProfileStatus{
			SourceID:      sourceID,
			StartFrame:    profile.startIndex,
			StartOffsetMS: (time.Duration(profile.startIndex) * server.frameDuration).Milliseconds(),
		})
	}
	sort.Slice(profiles, func(left, right int) bool { return profiles[left].SourceID < profiles[right].SourceID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"active":   active,
		"sources":  len(server.allowed),
		"playback": profiles,
	})
}

func (server *virtualSourceServer) serveWebSocket(w http.ResponseWriter, request *http.Request) {
	sourceID := strings.TrimPrefix(request.URL.Path, virtualSourcePrefix)
	if _, allowed := server.allowed[sourceID]; !allowed || sourceID == "" {
		http.Error(w, "unknown virtual source", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	server.mu.Lock()
	if _, active := server.active[sourceID]; active {
		server.mu.Unlock()
		cancel()
		http.Error(w, "virtual source already connected", http.StatusConflict)
		return
	}
	server.active[sourceID] = cancel
	server.mu.Unlock()
	defer func() {
		cancel()
		server.mu.Lock()
		delete(server.active, sourceID)
		server.mu.Unlock()
	}()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	connection, err := upgrader.Upgrade(w, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	if err := server.runPeer(ctx, sourceID, connection); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("source %q disconnected: %v", sourceID, err)
	}
}

func (server *virtualSourceServer) runPeer(ctx context.Context, sourceID string, connection *websocket.Conn) error {
	peer, err := server.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	defer peer.Close()
	track, err := webrtc.NewTrackLocalStaticSample(virtualH264Codec, "video", sourceID)
	if err != nil {
		return fmt.Errorf("create H.264 track: %w", err)
	}
	sender, err := peer.AddTrack(track)
	if err != nil {
		return fmt.Errorf("add H.264 track: %w", err)
	}

	var writeMu sync.Mutex
	send := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return connection.WriteJSON(message)
	}
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		value := candidate.ToJSON()
		if err := send(signalMessage{Type: "candidate", ICE: &value}); err != nil {
			log.Printf("source %q send ICE candidate: %v", sourceID, err)
		}
	})
	peer.OnDataChannel(func(channel *webrtc.DataChannel) {
		log.Printf("source %q DataChannel %q opened by Relay", sourceID, channel.Label())
	})

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("source %q peer state: %s", sourceID, state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			go server.play(ctx, sourceID, track, sender)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			closeDone()
		}
	})

	remoteDescriptionSet := false
	var pending []webrtc.ICECandidateInit
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		default:
		}
		_, data, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		switch message.Type {
		case "offer":
			if remoteDescriptionSet {
				return fmt.Errorf("renegotiation is not supported")
			}
			if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set offer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pending {
				if err := peer.AddICECandidate(candidate); err != nil {
					return fmt.Errorf("apply pending ICE candidate: %w", err)
				}
			}
			pending = nil
			answer, err := peer.CreateAnswer(nil)
			if err != nil {
				return fmt.Errorf("create answer: %w", err)
			}
			if err := peer.SetLocalDescription(answer); err != nil {
				return fmt.Errorf("set answer: %w", err)
			}
			if err := send(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return fmt.Errorf("send answer: %w", err)
			}
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pending = append(pending, *message.ICE)
				continue
			}
			if err := peer.AddICECandidate(*message.ICE); err != nil {
				return fmt.Errorf("add ICE candidate: %w", err)
			}
		case "close", "bye":
			return nil
		}
	}
}

func (server *virtualSourceServer) play(
	ctx context.Context,
	sourceID string,
	track *webrtc.TrackLocalStaticSample,
	sender *webrtc.RTPSender,
) {
	keyframeRequests := make(chan struct{}, 1)
	go func() {
		for {
			packets, _, err := sender.ReadRTCP()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("source %q read RTCP: %v", sourceID, err)
				}
				return
			}
			for _, packet := range packets {
				if _, ok := packet.(*rtcp.PictureLossIndication); ok {
					select {
					case keyframeRequests <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	ticker := time.NewTicker(server.frameDuration)
	defer ticker.Stop()
	index := server.playback[sourceID].startIndex
	for {
		select {
		case <-ctx.Done():
			return
		case <-keyframeRequests:
			index = nextKeyframe(server.accessUnits, index)
		case <-ticker.C:
			unit := server.accessUnits[index]
			if err := track.WriteSample(media.Sample{Data: unit.data, Duration: server.frameDuration}); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					log.Printf("source %q write H.264 sample: %v", sourceID, err)
				}
				return
			}
			index = (index + 1) % len(server.accessUnits)
		}
	}
}

func nextKeyframe(units []h264AccessUnit, current int) int {
	for offset := 0; offset < len(units); offset++ {
		index := (current + offset) % len(units)
		if units[index].keyframe {
			return index
		}
	}
	return 0
}
