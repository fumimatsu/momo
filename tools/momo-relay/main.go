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
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

//go:embed web
var webAssets embed.FS

const (
	commandLabel   = "momo-command"
	telemetryLabel = "momo-telemetry"
	raceLabel      = "momo-race"
	upstreamLabel  = "serial"

	defaultRTPStallTimeout      = 5 * time.Second
	defaultUpstreamStartTimeout = 20 * time.Second
	defaultPilotCommandTimeout  = 250 * time.Millisecond
	keyframeRecoveryGrace       = 2 * time.Second
	defaultVideoTimestampStep   = uint32(90000 / 50)
	operationsPollWindow        = time.Second
)

var h264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

type signalMessage struct {
	Type        string                   `json:"type"`
	SDP         string                   `json:"sdp,omitempty"`
	ICE         *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error       string                   `json:"error,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
	RoomID      string                   `json:"roomId,omitempty"`
	ClientID    string                   `json:"clientId,omitempty"`
	Key         string                   `json:"key,omitempty"`
	IsExistUser bool                     `json:"isExistUser,omitempty"`
	ICEServers  []webrtc.ICEServer       `json:"iceServers,omitempty"`
}

type viewer struct {
	id                  uint64
	role                string
	pc                  *webrtc.PeerConnection
	state               atomic.Int32
	telemetry           atomic.Pointer[webrtc.DataChannel]
	command             atomic.Pointer[webrtc.DataChannel]
	race                atomic.Pointer[webrtc.DataChannel]
	lastCommandUnixNano atomic.Int64
}

type sourceLifecycle int32

const (
	sourceWaiting sourceLifecycle = iota
	sourceConnecting
	sourceConnected
	sourceRetryWait
	sourceRecovering
)

func (state sourceLifecycle) String() string {
	switch state {
	case sourceWaiting:
		return "waiting"
	case sourceConnecting:
		return "connecting"
	case sourceConnected:
		return "connected"
	case sourceRetryWait:
		return "retry_wait"
	case sourceRecovering:
		return "recovering"
	default:
		return "waiting"
	}
}

type sourceVideoHealth int32

const (
	videoNotStarted sourceVideoHealth = iota
	videoReceiving
	videoStalled
)

func (health sourceVideoHealth) String() string {
	switch health {
	case videoReceiving:
		return "receiving"
	case videoStalled:
		return "stalled"
	default:
		return "not_started"
	}
}

type viewerConnectionState int32

const (
	viewerNegotiating viewerConnectionState = iota
	viewerConnected
)

type frameRateWindow struct {
	mu          sync.Mutex
	ingress     []time.Time
	relayWrites []time.Time
}

func (window *frameRateWindow) recordIngress(now time.Time) {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.ingress = append(window.ingress, now)
	window.ingress = pruneFrameTimes(window.ingress, now)
}

func (window *frameRateWindow) recordRelayWrite(now time.Time) {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.relayWrites = append(window.relayWrites, now)
	window.relayWrites = pruneFrameTimes(window.relayWrites, now)
}

func (window *frameRateWindow) snapshot(now time.Time) (float64, float64) {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.ingress = pruneFrameTimes(window.ingress, now)
	window.relayWrites = pruneFrameTimes(window.relayWrites, now)
	return float64(len(window.ingress)), float64(len(window.relayWrites))
}

func pruneFrameTimes(samples []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-operationsPollWindow)
	first := 0
	for first < len(samples) && !samples[first].After(cutoff) {
		first++
	}
	return samples[first:]
}

type relay struct {
	name                 string
	upstreamURL          string
	raceCarID            string
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

	rtpStallTimeout        time.Duration
	upstreamStartTimeout   time.Duration
	upstreamGeneration     atomic.Uint64
	pilotCommandTimeout    time.Duration
	lastVideoFrameUnixNano atomic.Int64
	lastRTPTimestamp       atomic.Uint32
	lifecycle              atomic.Int32
	videoHealth            atomic.Int32
	upstreamPeerState      atomic.Value
	lastErrorCode          atomic.Value
	connectionAttempts     atomic.Uint64
	pliNewTrack            atomic.Uint64
	pliViewerConnect       atomic.Uint64
	pliWatchdog            atomic.Uint64
	rtpStalls              atomic.Uint64
	frameRate              frameRateWindow

	rtpRewriteMu          sync.Mutex
	rtpRewriteInitialized bool
	rtpRewriteGeneration  uint64
	rtpSequenceOffset     uint16
	rtpTimestampOffset    uint32
	lastOutputSequence    uint16
	lastOutputTimestamp   uint32
	lastInputTimestamp    uint32
	lastTimestampStep     uint32

	raceStateMu sync.RWMutex
	raceState   string
}

type relayServer struct {
	sources     map[string]*relay
	sourceOrder []string
}

type raceStateEnvelope struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
}

type sourceFlag []string

func (values *sourceFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *sourceFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type operationsAccessPolicy struct {
	networks []*net.IPNet
}

func parseOperationsAccessPolicy(values []string) (operationsAccessPolicy, error) {
	if len(values) == 0 {
		values = []string{"127.0.0.1/32", "::1/128"}
	}
	policy := operationsAccessPolicy{networks: make([]*net.IPNet, 0, len(values))}
	for _, value := range values {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return operationsAccessPolicy{}, fmt.Errorf("invalid operations allow CIDR %q: %w", value, err)
		}
		policy.networks = append(policy.networks, network)
	}
	return policy, nil
}

func (policy operationsAccessPolicy) allows(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range policy.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (policy operationsAccessPolicy) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !policy.allows(req.RemoteAddr) {
			http.Error(w, "operations access denied", http.StatusForbidden)
			return
		}
		next(w, req)
	}
}

type operationsStatus struct {
	Version    int                     `json:"version"`
	ServerTime time.Time               `json:"serverTime"`
	Sources    []sourceOperationsState `json:"sources"`
}

type sourceOperationsState struct {
	ID          string                    `json:"id"`
	RaceCarID   string                    `json:"raceCarId,omitempty"`
	State       string                    `json:"state"`
	Lifecycle   string                    `json:"lifecycle"`
	VideoHealth string                    `json:"videoHealth"`
	Upstream    upstreamOperationsState   `json:"upstream"`
	Downstream  downstreamOperationsState `json:"downstream"`
	Recovery    recoveryOperationsState   `json:"recovery"`
}

type upstreamOperationsState struct {
	PeerState               string  `json:"peerState"`
	SerialOpen              bool    `json:"serialOpen"`
	LastRtpAgeMs            *int64  `json:"lastRtpAgeMs"`
	IngressAccessUnitFPS    float64 `json:"ingressAccessUnitFps"`
	RelayWriteAccessUnitFPS float64 `json:"relayWriteAccessUnitFps"`
	Generation              uint64  `json:"generation"`
	StallTimeoutMs          int64   `json:"stallTimeoutMs"`
	StartTimeoutMs          int64   `json:"startTimeoutMs"`
}

type downstreamOperationsState struct {
	PilotLeaseReserved bool `json:"pilotLeaseReserved"`
	NegotiatingPeers   int  `json:"negotiatingPeers"`
	ConnectedPilots    int  `json:"connectedPilots"`
	ConnectedObservers int  `json:"connectedObservers"`
	TelemetryOpen      int  `json:"telemetryChannelsOpen"`
	RaceOpen           int  `json:"raceChannelsOpen"`
}

type pliRequestCounts struct {
	NewTrack      uint64 `json:"newTrack"`
	ViewerConnect uint64 `json:"viewerConnect"`
	Watchdog      uint64 `json:"watchdog"`
}

type recoveryOperationsState struct {
	PLIRequests   pliRequestCounts `json:"pliRequests"`
	RTPStalls     uint64           `json:"rtpStalls"`
	RetryAttempts uint64           `json:"retryAttempts"`
	LastErrorCode *string          `json:"lastErrorCode"`
}

func newRelay(name string, upstreamURL string, raceCarID string, allowObserverCommand bool,
	rtpStallTimeout time.Duration, upstreamStartTimeout time.Duration) (*relay, error) {
	api, err := newH264API()
	if err != nil {
		return nil, err
	}
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(h264Codec, "video", "momo")
	if err != nil {
		return nil, fmt.Errorf("create local H264 track: %w", err)
	}
	relay := &relay{
		name:                 name,
		upstreamURL:          upstreamURL,
		raceCarID:            raceCarID,
		allowObserverCommand: allowObserverCommand,
		videoTrack:           videoTrack,
		api:                  api,
		viewers:              make(map[uint64]*viewer),
		rtpStallTimeout:      rtpStallTimeout,
		upstreamStartTimeout: upstreamStartTimeout,
		pilotCommandTimeout:  defaultPilotCommandTimeout,
	}
	relay.lifecycle.Store(int32(sourceWaiting))
	relay.videoHealth.Store(int32(videoNotStarted))
	relay.upstreamPeerState.Store("new")
	relay.lastErrorCode.Store("")
	return relay, nil
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
	go r.watchPilotCommands(ctx)
	go func() {
		for {
			r.lifecycle.Store(int32(sourceConnecting))
			r.videoHealth.Store(int32(videoNotStarted))
			r.connectionAttempts.Add(1)
			err := r.connectUpstream(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				if sourceLifecycle(r.lifecycle.Load()) != sourceRecovering {
					r.lifecycle.Store(int32(sourceRetryWait))
				}
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

func (r *relay) watchPilotCommands(ctx context.Context) {
	interval := r.pilotCommandTimeout / 2
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.viewersMu.RLock()
			pilotID := r.pilotID
			pilot := r.viewers[pilotID]
			r.viewersMu.RUnlock()
			if pilot == nil || pilot.command.Load() == nil {
				continue
			}
			lastCommand := pilot.lastCommandUnixNano.Load()
			if lastCommand == 0 || now.Sub(time.Unix(0, lastCommand)) >= r.pilotCommandTimeout {
				r.sendNeutralToUpstream("pilot command timeout")
			}
		}
	}
}

func (r *relay) connectUpstream(ctx context.Context) error {
	log.Printf("source %q: connecting upstream Momo: %s", r.name, r.upstreamURL)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, r.upstreamURL, nil)
	if err != nil {
		r.setLastErrorCode("upstream_signaling_failed")
		return fmt.Errorf("connect upstream signaling: %w", err)
	}
	defer ws.Close()

	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("create upstream peer connection: %w", err)
	}
	defer pc.Close()
	generation := r.upstreamGeneration.Add(1)
	r.lastVideoFrameUnixNano.Store(0)
	r.lastRTPTimestamp.Store(0)
	r.upstreamSSRC.Store(0)
	r.upstreamPeerState.Store("new")

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
		if r.upstreamGeneration.Load() != generation {
			return
		}
		r.upstreamPeerState.Store(state.String())
		if state == webrtc.PeerConnectionStateConnected {
			r.lifecycle.Store(int32(sourceConnected))
		}
		if state == webrtc.PeerConnectionStateFailed {
			r.setLastErrorCode("upstream_peer_failed")
			r.lifecycle.Store(int32(sourceRetryWait))
			_ = ws.Close()
		}
		log.Printf("source %q: upstream peer connection state: %s", r.name, state.String())
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if r.upstreamGeneration.Load() != generation {
			return
		}
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
			log.Printf("ignore unsupported upstream video codec: %s", track.Codec().MimeType)
			return
		}
		r.upstreamSSRC.Store(uint32(track.SSRC()))
		log.Printf("source %q: receiving upstream H264 track: SSRC=%d codec=%s", r.name, track.SSRC(), track.Codec().SDPFmtpLine)
		// Momo の再起動後は既存 Viewer の接続が維持されるため、
		// Viewer 接続時だけの PLI では復号器が差分フレームを受け続ける。
		// 新しい上流トラックを受けた時点でも IDR を要求する。
		r.requestKeyframe("new_track")
		go func() {
			for _, delay := range []time.Duration{time.Second, 3 * time.Second} {
				time.Sleep(delay)
				r.requestKeyframe("new_track")
			}
		}()
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("upstream H264 RTP ended: %v", err)
				return
			}
			if r.upstreamGeneration.Load() == generation {
				previousTimestamp := r.lastRTPTimestamp.Swap(packet.Timestamp)
				if r.lastVideoFrameUnixNano.Load() == 0 ||
					previousTimestamp != packet.Timestamp {
					r.lastVideoFrameUnixNano.Store(time.Now().UnixNano())
					r.videoHealth.Store(int32(videoReceiving))
					r.setLastErrorCode("")
				}
			}
			if packet.Marker {
				r.frameRate.recordIngress(time.Now())
			}
			if !r.rewriteRTPHeader(generation, &packet.Header) {
				return
			}
			if err := r.videoTrack.WriteRTP(packet); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					r.setLastErrorCode("relay_write_failed")
					log.Printf("fan out upstream RTP: %v", err)
				}
			} else if packet.Marker {
				r.frameRate.recordRelayWrite(time.Now())
			}
		}
	})

	upstreamDC, err := pc.CreateDataChannel(upstreamLabel, nil)
	if err != nil {
		r.setLastErrorCode("upstream_data_channel_failed")
		return fmt.Errorf("create upstream data channel: %w", err)
	}
	upstreamDC.OnOpen(func() {
		if r.upstreamGeneration.Load() != generation {
			return
		}
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
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("add upstream recvonly video transceiver: %w", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("create upstream offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("set upstream local description: %w", err)
	}
	if err := sendSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		r.setLastErrorCode("upstream_signaling_failed")
		return fmt.Errorf("send upstream offer: %w", err)
	}
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go r.watchUpstreamRTP(ctx, generation, pc, ws, watchdogDone)

	var pendingCandidates []webrtc.ICECandidateInit
	remoteDescriptionSet := false
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			r.clearUpstream(pc)
			if sourceLifecycle(r.lifecycle.Load()) != sourceRecovering {
				r.setLastErrorCode("upstream_signaling_failed")
			}
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
			r.setLastErrorCode("upstream_signaling_closed")
			return errors.New("upstream closed signaling session")
		}
	}
}

// TrackLocalStaticRTP は出力 SSRC だけを Viewer ごとに置き換え、上流の
// sequence number と timestamp はそのまま送る。Momo を再起動するとこれらが
// 先頭へ戻るため、接続済み Viewer の jitter buffer は再接続後の RTP を古い
// パケットとして捨てる。source 世代をまたぐ時だけ offset を更新し、下流で
// sequence number と timestamp が連続するようにする。
func (r *relay) rewriteRTPHeader(generation uint64, header *rtp.Header) bool {
	r.rtpRewriteMu.Lock()
	defer r.rtpRewriteMu.Unlock()

	if r.upstreamGeneration.Load() != generation {
		return false
	}

	if !r.rtpRewriteInitialized {
		r.rtpRewriteInitialized = true
		r.rtpRewriteGeneration = generation
		r.lastInputTimestamp = header.Timestamp
		r.lastTimestampStep = defaultVideoTimestampStep
	} else if r.rtpRewriteGeneration != generation {
		r.rtpSequenceOffset = r.lastOutputSequence + 1 - header.SequenceNumber
		r.rtpTimestampOffset = r.lastOutputTimestamp + r.lastTimestampStep - header.Timestamp
		r.rtpRewriteGeneration = generation
		r.lastInputTimestamp = header.Timestamp
	} else if header.Timestamp != r.lastInputTimestamp {
		r.lastTimestampStep = header.Timestamp - r.lastInputTimestamp
		r.lastInputTimestamp = header.Timestamp
	}

	header.SequenceNumber += r.rtpSequenceOffset
	header.Timestamp += r.rtpTimestampOffset
	r.lastOutputSequence = header.SequenceNumber
	r.lastOutputTimestamp = header.Timestamp
	return true
}

// WebRTC の PeerConnection は ICE が connected のままでも、RTP の受信だけが
// 無期限に止まることがある。停止後も古い RTP の再送が届く場合があるため、
// パケット到着ではなく RTP timestamp が進んだ最終時刻を監視する。止まった
// source の WebSocket と PeerConnection を閉じ、既存の再接続ループへ制御を戻す。
func (r *relay) watchUpstreamRTP(ctx context.Context, generation uint64,
	pc *webrtc.PeerConnection, ws *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	startedAt := time.Now()
	var keyframeRequestedAt time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case now := <-ticker.C:
			if r.upstreamGeneration.Load() != generation {
				return
			}

			lastVideoFrame := r.lastVideoFrameUnixNano.Load()
			stallTimeout := r.upstreamStartTimeout
			stalledFor := now.Sub(startedAt)
			if lastVideoFrame != 0 {
				stallTimeout = r.rtpStallTimeout
				stalledFor = now.Sub(time.Unix(0, lastVideoFrame))
			}
			if stalledFor < stallTimeout {
				keyframeRequestedAt = time.Time{}
				continue
			}

			if keyframeRequestedAt.IsZero() {
				keyframeRequestedAt = now
				r.videoHealth.Store(int32(videoStalled))
				r.rtpStalls.Add(1)
				log.Printf("source %q: no upstream video frames for %s; request keyframe before reconnect",
					r.name, stalledFor.Round(time.Millisecond))
				r.requestKeyframe("watchdog")
				continue
			}
			if now.Sub(keyframeRequestedAt) < keyframeRecoveryGrace {
				continue
			}

			log.Printf("source %q: upstream video frames remained stalled for %s; reconnecting source",
				r.name, stalledFor.Round(time.Millisecond))
			r.setLastErrorCode("upstream_rtp_stalled")
			r.lifecycle.Store(int32(sourceRecovering))
			r.clearUpstream(pc)
			_ = pc.Close()
			_ = ws.Close()
			return
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

func (r *relay) setLastErrorCode(code string) {
	r.lastErrorCode.Store(code)
}

func (r *relay) statusSnapshot(now time.Time) sourceOperationsState {
	lifecycle := sourceLifecycle(r.lifecycle.Load())
	videoHealth := sourceVideoHealth(r.videoHealth.Load())
	peerState, _ := r.upstreamPeerState.Load().(string)
	lastError, _ := r.lastErrorCode.Load().(string)
	lastFrame := r.lastVideoFrameUnixNano.Load()
	var lastRtpAgeMs *int64
	if lastFrame != 0 {
		age := now.Sub(time.Unix(0, lastFrame)).Milliseconds()
		if age < 0 {
			age = 0
		}
		lastRtpAgeMs = &age
	}
	ingressFPS, relayWriteFPS := r.frameRate.snapshot(now)

	r.upstreamMu.RLock()
	serialOpen := r.upstreamDC != nil
	r.upstreamMu.RUnlock()

	attempts := r.connectionAttempts.Load()
	retries := uint64(0)
	if attempts > 0 {
		retries = attempts - 1
	}
	var lastErrorCode *string
	if lastError != "" {
		lastErrorCode = &lastError
	}

	return sourceOperationsState{
		ID:          r.name,
		RaceCarID:   r.raceCarID,
		State:       displaySourceState(lifecycle, videoHealth),
		Lifecycle:   lifecycle.String(),
		VideoHealth: videoHealth.String(),
		Upstream: upstreamOperationsState{
			PeerState:               peerState,
			SerialOpen:              serialOpen,
			LastRtpAgeMs:            lastRtpAgeMs,
			IngressAccessUnitFPS:    ingressFPS,
			RelayWriteAccessUnitFPS: relayWriteFPS,
			Generation:              r.upstreamGeneration.Load(),
			StallTimeoutMs:          r.rtpStallTimeout.Milliseconds(),
			StartTimeoutMs:          r.upstreamStartTimeout.Milliseconds(),
		},
		Downstream: r.downstreamStatusSnapshot(),
		Recovery: recoveryOperationsState{
			PLIRequests: pliRequestCounts{
				NewTrack:      r.pliNewTrack.Load(),
				ViewerConnect: r.pliViewerConnect.Load(),
				Watchdog:      r.pliWatchdog.Load(),
			},
			RTPStalls:     r.rtpStalls.Load(),
			RetryAttempts: retries,
			LastErrorCode: lastErrorCode,
		},
	}
}

func (r *relay) downstreamStatusSnapshot() downstreamOperationsState {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	state := downstreamOperationsState{PilotLeaseReserved: r.pilotID != 0}
	for _, client := range r.viewers {
		if viewerConnectionState(client.state.Load()) == viewerConnected {
			if client.role == "pilot" {
				state.ConnectedPilots++
			} else {
				state.ConnectedObservers++
			}
		} else {
			state.NegotiatingPeers++
		}
		if client.telemetry.Load() != nil {
			state.TelemetryOpen++
		}
		if client.race.Load() != nil {
			state.RaceOpen++
		}
	}
	return state
}

func displaySourceState(lifecycle sourceLifecycle, videoHealth sourceVideoHealth) string {
	switch lifecycle {
	case sourceRecovering:
		return "RECOVERING"
	case sourceRetryWait:
		return "DISCONNECTED"
	case sourceWaiting:
		return "WAITING"
	case sourceConnected:
		switch videoHealth {
		case videoStalled:
			return "STALE"
		case videoReceiving:
			return "STREAMING"
		default:
			return "CONNECTING"
		}
	default:
		return "CONNECTING"
	}
}

func (server *relayServer) operationsStatusSnapshot(now time.Time) operationsStatus {
	sources := make([]sourceOperationsState, 0, len(server.sourceOrder))
	for _, sourceID := range server.sourceOrder {
		source, ok := server.sources[sourceID]
		if !ok {
			continue
		}
		sources = append(sources, source.statusSnapshot(now))
	}
	return operationsStatus{Version: 1, ServerTime: now.UTC(), Sources: sources}
}

func (server *relayServer) serveOperationsStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(server.operationsStatusSnapshot(time.Now()))
}

func operationsPageHandler(operationsHTML []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(operationsHTML)
	}
}

// 新しい Viewer は relay に蓄積されていない差分フレームから受信を始める。
// 上流 Momo に IDR を要求しないと、次の自然発生キーフレームまで映像を
// 復号できず、黒画面のままになる。
func (r *relay) requestKeyframe(reason string) {
	r.upstreamMu.RLock()
	pc := r.upstreamPC
	ssrc := r.upstreamSSRC.Load()
	r.upstreamMu.RUnlock()
	if pc == nil || ssrc == 0 {
		return
	}
	switch reason {
	case "new_track":
		r.pliNewTrack.Add(1)
	case "viewer_connect":
		r.pliViewerConnect.Add(1)
	case "watchdog":
		r.pliWatchdog.Add(1)
	}
	if err := pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: 1, MediaSSRC: ssrc},
	}); err != nil {
		log.Printf("source %q: request upstream keyframe: %v", r.name, err)
	} else {
		log.Printf("source %q: requested upstream keyframe for SSRC=%d", r.name, ssrc)
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

// Race Control の状態は操縦テレメトリーと分離した reliable DataChannel で配る。
// 順位やフラグは最新値を確実に渡す必要があり、低遅延・非信頼の telemetry channel
// に混在させると再送されず、Viewer が古い状態のまま残るためである。
func (r *relay) publishRaceState(message string) {
	r.raceStateMu.Lock()
	r.raceState = message
	r.raceStateMu.Unlock()
	r.broadcastRaceState(message)
}

func (r *relay) currentRaceState() string {
	r.raceStateMu.RLock()
	defer r.raceStateMu.RUnlock()
	return r.raceState
}

func (r *relay) broadcastRaceState(message string) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if channel := client.race.Load(); channel != nil {
			if err := channel.SendText(message); err != nil {
				log.Printf("send race state to viewer %d: %v", client.id, err)
			}
		}
	}
}

func (r *relay) sendCurrentRaceState(client *viewer, channel *webrtc.DataChannel) {
	message := r.currentRaceState()
	if message == "" {
		return
	}
	if err := channel.SendText(message); err != nil {
		log.Printf("send cached race state to viewer %d: %v", client.id, err)
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
	client.lastCommandUnixNano.Store(time.Now().UnixNano())
	// コマンドは全員に同じ DataChannel で返す。クライアント側は受信時にのみ
	// 表示するため、この監査メッセージが Momo に再送されることはない。
	r.broadcastCommand(message)
}

func (r *relay) sendNeutralToUpstream(reason string) {
	r.upstreamMu.RLock()
	upstream := r.upstreamDC
	r.upstreamMu.RUnlock()
	if upstream == nil {
		return
	}
	if err := upstream.SendText("S:1500,T:1500"); err != nil {
		log.Printf("source %q: send neutral after %s: %v", r.name, reason, err)
	}
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
	wasPilot := false
	r.viewersMu.Lock()
	delete(r.viewers, id)
	if r.pilotID == id {
		r.pilotID = 0
		wasPilot = true
	}
	r.viewersMu.Unlock()
	if wasPilot {
		r.sendNeutralToUpstream("pilot disconnect")
	}
}

// Ayame の room は source ごとに 1 つだけ割り当てる。ここでは映像の下流配信だけを
// 担当する。外部操縦は deadman / neutral failsafe が未実装のため、別段階で追加する。
func (r *relay) startAyamePilot(ctx context.Context, signalingURL string, roomID string, clientID string, key string) {
	go func() {
		for {
			err := r.connectAyamePilot(ctx, signalingURL, roomID, clientID, key)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("source %q: Ayame pilot disconnected: %v; retrying in 3 seconds", r.name, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

func (r *relay) connectAyamePilot(ctx context.Context, signalingURL string, roomID string, clientID string, key string) error {
	log.Printf("source %q: connecting Ayame external pilot room %q", r.name, roomID)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, signalingURL, nil)
	if err != nil {
		return fmt.Errorf("connect Ayame signaling: %w", err)
	}
	defer ws.Close()

	var writeMu sync.Mutex
	sendSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}
	if err := sendSignal(signalMessage{Type: "register", RoomID: roomID, ClientID: clientID, Key: key}); err != nil {
		return fmt.Errorf("register Ayame room: %w", err)
	}

	var client *viewer
	var pc *webrtc.PeerConnection
	var ayameICEServers []webrtc.ICEServer
	remoteDescriptionSet := false
	var pendingCandidates []webrtc.ICECandidateInit
	cleanup := func() {
		if pc != nil {
			_ = pc.Close()
		}
		if client != nil {
			r.removeViewer(client.id)
		}
	}
	defer cleanup()

	createPeer := func(iceServers []webrtc.ICEServer) error {
		if pc != nil {
			return nil
		}
		client = &viewer{id: r.nextID.Add(1), role: "pilot"}
		client.state.Store(int32(viewerNegotiating))
		if !r.reservePilot(client.id) {
			return errors.New("a local or external pilot is already connected")
		}
		var createErr error
		pc, createErr = r.api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
		if createErr != nil {
			r.removeViewer(client.id)
			return fmt.Errorf("create Ayame peer connection: %w", createErr)
		}
		pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			if candidate == nil {
				return
			}
			candidateJSON := candidate.ToJSON()
			if err := sendSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
				log.Printf("source %q: send Ayame ICE candidate: %v", r.name, err)
			}
		})
		pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			log.Printf("source %q: Ayame pilot peer connection state: %s", r.name, state.String())
			if state == webrtc.PeerConnectionStateConnected {
				client.state.Store(int32(viewerConnected))
				r.addViewer(client)
				r.requestKeyframe("viewer_connect")
			}
		})
		pc.OnDataChannel(func(channel *webrtc.DataChannel) {
			switch channel.Label() {
			case commandLabel:
				channel.OnOpen(func() {
					client.command.Store(channel)
					log.Printf("source %q: Ayame pilot command channel opened", r.name)
				})
				channel.OnMessage(func(message webrtc.DataChannelMessage) {
					r.handleCommand(client, message)
				})
				channel.OnClose(func() {
					client.command.CompareAndSwap(channel, nil)
					r.sendNeutralToUpstream("Ayame command channel closed")
				})
			case telemetryLabel:
				channel.OnOpen(func() { client.telemetry.Store(channel) })
				channel.OnClose(func() { client.telemetry.CompareAndSwap(channel, nil) })
			case raceLabel:
				channel.OnOpen(func() {
					client.race.Store(channel)
					r.sendCurrentRaceState(client, channel)
				})
				channel.OnClose(func() { client.race.CompareAndSwap(channel, nil) })
			default:
				log.Printf("source %q: Ayame pilot opened unsupported DataChannel %q", r.name, channel.Label())
			}
		})
		if _, createErr = pc.AddTrack(r.videoTrack); createErr != nil {
			_ = pc.Close()
			r.removeViewer(client.id)
			pc = nil
			return fmt.Errorf("add Ayame video track: %w", createErr)
		}
		return nil
	}

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return fmt.Errorf("decode Ayame signaling: %w", err)
		}
		switch message.Type {
		case "accept":
			ayameICEServers = message.ICEServers
			if message.IsExistUser {
				if err := createPeer(ayameICEServers); err != nil {
					return err
				}
				offer, err := pc.CreateOffer(nil)
				if err != nil {
					return fmt.Errorf("create Ayame offer: %w", err)
				}
				if err := pc.SetLocalDescription(offer); err != nil {
					return fmt.Errorf("set Ayame offer: %w", err)
				}
				if err := sendSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
					return fmt.Errorf("send Ayame offer: %w", err)
				}
			}
		case "offer":
			if err := createPeer(ayameICEServers); err != nil {
				return err
			}
			if remoteDescriptionSet {
				return errors.New("Ayame renegotiation is not supported")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set Ayame offer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("source %q: apply pending Ayame ICE candidate: %v", r.name, err)
				}
			}
			pendingCandidates = nil
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return fmt.Errorf("create Ayame answer: %w", err)
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				return fmt.Errorf("set Ayame answer: %w", err)
			}
			if err := sendSignal(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return fmt.Errorf("send Ayame answer: %w", err)
			}
		case "answer":
			if pc == nil {
				return errors.New("received Ayame answer before offer")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set Ayame answer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("source %q: apply pending Ayame ICE candidate: %v", r.name, err)
				}
			}
			pendingCandidates = nil
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if pc == nil || !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := pc.AddICECandidate(*message.ICE); err != nil {
				log.Printf("source %q: apply Ayame ICE candidate: %v", r.name, err)
			}
		case "ping":
			if err := sendSignal(signalMessage{Type: "pong"}); err != nil {
				return fmt.Errorf("send Ayame pong: %w", err)
			}
		case "bye", "reject":
			return fmt.Errorf("Ayame %s: %s", message.Type, firstNonEmpty(message.Reason, message.Error))
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
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
	client.state.Store(int32(viewerNegotiating))
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
		if state == webrtc.PeerConnectionStateConnected {
			client.state.Store(int32(viewerConnected))
			r.requestKeyframe("viewer_connect")
			// 接続直後の IDR が欠けると、H.264 の復号器は次の IDR まで
			// 映像を出せない。LAN 内でも ICE/DTLS の確立直後はこの状態に
			// なり得るため、短時間だけ PLI を再送する。
			go func() {
				for _, delay := range []time.Duration{time.Second, 3 * time.Second} {
					time.Sleep(delay)
					r.requestKeyframe("viewer_connect")
				}
			}()
		}
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
		case raceLabel:
			channel.OnOpen(func() {
				client.race.Store(channel)
				log.Printf("viewer %d race channel opened", client.id)
				r.sendCurrentRaceState(client, channel)
			})
			channel.OnClose(func() { client.race.CompareAndSwap(channel, nil) })
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

func raceMessageForCar(state []byte, carID string) (string, error) {
	if strings.TrimSpace(carID) == "" {
		return "", errors.New("race car ID is empty")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(state, &payload); err != nil {
		return "", fmt.Errorf("decode race state: %w", err)
	}
	carIDJSON, err := json.Marshal(carID)
	if err != nil {
		return "", fmt.Errorf("encode race car ID: %w", err)
	}
	payload["viewerCarId"] = carIDJSON
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode race state: %w", err)
	}
	return "RACE:" + string(encoded), nil
}

func (server *relayServer) startRaceControl(ctx context.Context, raceURL string, viewerToken string) {
	if strings.TrimSpace(raceURL) == "" {
		return
	}
	go func() {
		for {
			if err := server.connectRaceControl(ctx, raceURL, viewerToken); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("Race Control disconnected: %v; retrying in 3 seconds", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

func (server *relayServer) connectRaceControl(ctx context.Context, raceURL string, viewerToken string) error {
	headers := http.Header{}
	if strings.TrimSpace(viewerToken) != "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(viewerToken))
	}
	log.Printf("connecting Race Control: %s", raceURL)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, raceURL, headers)
	if err != nil {
		return fmt.Errorf("connect Race Control WebSocket: %w", err)
	}
	defer ws.Close()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Race Control WebSocket: %w", err)
		}
		var envelope raceStateEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			log.Printf("ignore malformed Race Control message: %v", err)
			continue
		}
		if envelope.Type != "race_state" || envelope.Version != 2 {
			log.Printf("ignore unsupported Race Control message: type=%q version=%d", envelope.Type, envelope.Version)
			continue
		}
		for _, source := range server.sources {
			message, err := raceMessageForCar(data, source.raceCarID)
			if err != nil {
				log.Printf("source %q: ignore Race Control state: %v", source.name, err)
				continue
			}
			source.publishRaceState(message)
		}
	}
}

func main() {
	var upstream string
	var listen string
	var allowObserverCommand bool
	var rtpStallTimeout time.Duration
	var upstreamStartTimeout time.Duration
	var sources sourceFlag
	var raceCars sourceFlag
	var operationsAllowCIDRs sourceFlag
	var raceURL string
	var raceViewerToken string
	var ayameSignalingURL string
	var ayameClientIDPrefix string
	var ayameSignalingKey string
	var ayamePilotRooms sourceFlag
	flag.StringVar(&upstream, "upstream", "", "Momo P2P WebSocket URL, for example ws://192.168.11.3:8080/ws")
	flag.Var(&sources, "source", "Momo source as DEVICE=WS_URL; can be repeated")
	flag.Var(&raceCars, "race-car", "Race Control car mapping as DEVICE=CAR_ID; can be repeated")
	flag.Var(&operationsAllowCIDRs, "operations-allow-cidr", "CIDR allowed to read /operations.html and /api/v1/status; can be repeated (default: loopback only)")
	flag.StringVar(&listen, "listen", ":8090", "HTTP and WebSocket listen address")
	flag.StringVar(&raceURL, "race-url", "", "Race Control WebSocket URL for race_state v2 distribution")
	flag.StringVar(&raceViewerToken, "race-viewer-token", "", "Race Control Viewer Bearer token")
	flag.StringVar(&ayameSignalingURL, "ayame-signaling-url", "", "Ayame signaling WebSocket URL for external pilot distribution")
	flag.StringVar(&ayameClientIDPrefix, "ayame-client-id-prefix", "momo-relay", "Ayame client ID prefix; source name is appended")
	flag.StringVar(&ayameSignalingKey, "ayame-signaling-key", "", "Ayame signaling key for external pilot distribution")
	flag.Var(&ayamePilotRooms, "ayame-pilot-room", "Ayame external pilot room as DEVICE=ROOM_ID; can be repeated")
	flag.BoolVar(&allowObserverCommand, "allow-observer-command", false, "allow observer viewers to send commands to Momo")
	flag.DurationVar(&rtpStallTimeout, "rtp-stall-timeout", defaultRTPStallTimeout, "reconnect a source when received RTP stops for this duration")
	flag.DurationVar(&upstreamStartTimeout, "upstream-start-timeout", defaultUpstreamStartTimeout, "reconnect a source when no RTP arrives after connection")
	flag.Parse()
	if rtpStallTimeout <= 0 || upstreamStartTimeout <= 0 {
		log.Fatal("-rtp-stall-timeout and -upstream-start-timeout must be positive")
	}
	if upstream != "" {
		sources = append(sources, "default="+upstream)
	}
	if len(sources) == 0 {
		log.Fatal("-upstream or at least one -source is required")
	}
	operationsPolicy, err := parseOperationsAccessPolicy(operationsAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raceCarBySource := make(map[string]string, len(raceCars))
	for _, raceCarValue := range raceCars {
		name, carID, err := parseSource(raceCarValue)
		if err != nil {
			log.Fatalf("invalid -race-car: %v", err)
		}
		if _, exists := raceCarBySource[name]; exists {
			log.Fatalf("duplicate Race Control source mapping: %q", name)
		}
		raceCarBySource[name] = carID
	}
	ayameRoomBySource := make(map[string]string, len(ayamePilotRooms))
	for _, ayameRoomValue := range ayamePilotRooms {
		name, roomID, err := parseSource(ayameRoomValue)
		if err != nil {
			log.Fatalf("invalid -ayame-pilot-room: %v", err)
		}
		if _, exists := ayameRoomBySource[name]; exists {
			log.Fatalf("duplicate Ayame source mapping: %q", name)
		}
		ayameRoomBySource[name] = roomID
	}
	if len(ayameRoomBySource) > 0 && ayameSignalingURL == "" {
		log.Fatal("-ayame-signaling-url is required when -ayame-pilot-room is set")
	}
	serverRelay := &relayServer{
		sources:     make(map[string]*relay, len(sources)),
		sourceOrder: make([]string, 0, len(sources)),
	}
	for _, sourceValue := range sources {
		name, sourceURL, err := parseSource(sourceValue)
		if err != nil {
			log.Fatal(err)
		}
		if _, exists := serverRelay.sources[name]; exists {
			log.Fatalf("duplicate source name: %q", name)
		}
		raceCarID := raceCarBySource[name]
		if raceURL != "" && raceCarID == "" {
			log.Fatalf("Race Control is enabled but source %q has no -race-car mapping", name)
		}
		relay, err := newRelay(name, sourceURL, raceCarID, allowObserverCommand,
			rtpStallTimeout, upstreamStartTimeout)
		if err != nil {
			log.Fatal(err)
		}
		serverRelay.sources[name] = relay
		serverRelay.sourceOrder = append(serverRelay.sourceOrder, name)
		relay.start(ctx)
		if roomID := ayameRoomBySource[name]; roomID != "" {
			clientID := strings.TrimSuffix(ayameClientIDPrefix, "-") + "-" + name
			relay.startAyamePilot(ctx, ayameSignalingURL, roomID, clientID, ayameSignalingKey)
		}
	}
	for name := range ayameRoomBySource {
		if _, exists := serverRelay.sources[name]; !exists {
			log.Fatalf("Ayame source %q is not configured by -source", name)
		}
	}
	serverRelay.startRaceControl(ctx, raceURL, raceViewerToken)

	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatal(err)
	}
	operationsHTML, err := fs.ReadFile(webRoot, "operations.html")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", operationsPolicy.wrap(serverRelay.serveOperationsStatus))
	mux.HandleFunc("/operations.html", operationsPolicy.wrap(operationsPageHandler(operationsHTML)))
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
