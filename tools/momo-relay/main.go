package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
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
	driveLabel     = "momo-drive"
	eventsLabel    = "momo-events"
	upstreamLabel  = "serial"

	defaultRTPStallTimeout       = 5 * time.Second
	defaultUpstreamStartTimeout  = 20 * time.Second
	defaultPilotCommandTimeout   = 250 * time.Millisecond
	commandDropLogInterval       = time.Second
	telemetryDeliveryLogInterval = 5 * time.Second
	telemetryDataHighWatermark   = uint64(64 * 1024)
	gameplayWebSocketQueueSize   = 128
	observerTelemetryInterval    = time.Second / 15
	maxTelemetryStateSources     = 16
	fuelCommandCapability        = "fuel_command_v1"
	raceStreamPingInterval       = 5 * time.Second
	raceStreamPongWait           = 15 * time.Second
	raceStreamWriteTimeout       = 5 * time.Second
	keyframeRecoveryGrace        = 2 * time.Second
	defaultVideoTimestampStep    = uint32(90000 / 50)
	operationsPollWindow         = time.Second
)

var h264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	RTCPFeedback: []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	},
}

type signalMessage struct {
	Type        string                   `json:"type"`
	Data        string                   `json:"data,omitempty"`
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
	id                   uint64
	role                 string
	clientKind           string
	remoteAddr           string
	pc                   *webrtc.PeerConnection
	state                atomic.Int32
	telemetry            atomic.Pointer[webrtc.DataChannel]
	command              atomic.Pointer[webrtc.DataChannel]
	race                 atomic.Pointer[webrtc.DataChannel]
	raceAudio            atomic.Pointer[webrtc.DataChannel]
	drive                atomic.Pointer[webrtc.DataChannel]
	events               atomic.Pointer[webrtc.DataChannel]
	lastCommandUnixNano  atomic.Int64
	lastCommandDropLog   atomic.Int64
	lastTelemetryLog     atomic.Int64
	lastTelemetrySentAt  atomic.Int64
	telemetryMessages    atomic.Uint64
	telemetryBytes       atomic.Uint64
	telemetrySendErrors  atomic.Uint64
	telemetryDropped     atomic.Uint64
	telemetryThrottled   atomic.Uint64
	telemetryWS          chan string
	telemetryStateWS     *sourceLatestTelemetryQueue
	gameplayWS           chan string
	commandWS            chan string
	raceWS               chan string
	audioWS              chan string
	eventsWS             chan string
	audioSubscribed      atomic.Bool
	telemetrySendMu      sync.Mutex
	observerStateMu      sync.Mutex
	observerStateAt      map[string]int64
	raceSendMu           sync.Mutex
	eventsSendMu         sync.Mutex
	raceAudioSendMu      sync.Mutex
	raceAudioCalloutMu   sync.Mutex
	raceAudioCalloutAt   time.Time
	raceAudioCalloutSeen map[string]time.Time
	raceAudioLanguage    atomic.Value
	raceAudioMode        atomic.Value
	raceAudioTrack       *webrtc.TrackLocalStaticSample
	raceAudioQueueMu     sync.Mutex
	raceAudioQueue       chan raceAudioClip
	raceAudioStop        chan struct{}
	raceAudioStopOnce    sync.Once
}

// sourceLatestTelemetryQueue keeps one latest state per telemetry source.
// A single latest-wins slot is insufficient once imu0 and esc0 have different
// production rates because the faster source can permanently replace the slower one.
type sourceLatestTelemetryQueue struct {
	mu     sync.Mutex
	latest map[string]string
	order  []string
	ready  chan struct{}
}

func newSourceLatestTelemetryQueue() *sourceLatestTelemetryQueue {
	return &sourceLatestTelemetryQueue{
		latest: make(map[string]string),
		ready:  make(chan struct{}, 1),
	}
}

func (queue *sourceLatestTelemetryQueue) Ready() <-chan struct{} {
	if queue == nil {
		return nil
	}
	return queue.ready
}

func (queue *sourceLatestTelemetryQueue) Enqueue(source string, payload string) {
	if queue == nil {
		return
	}
	queue.mu.Lock()
	if _, exists := queue.latest[source]; !exists {
		if len(queue.latest) >= maxTelemetryStateSources {
			source = "_overflow"
		}
		if _, exists := queue.latest[source]; !exists {
			queue.order = append(queue.order, source)
		}
	}
	queue.latest[source] = payload
	queue.mu.Unlock()
	select {
	case queue.ready <- struct{}{}:
	default:
	}
}

func (queue *sourceLatestTelemetryQueue) Dequeue() (string, bool) {
	if queue == nil {
		return "", false
	}
	queue.mu.Lock()
	if len(queue.order) == 0 {
		queue.mu.Unlock()
		return "", false
	}
	source := queue.order[0]
	queue.order = queue.order[1:]
	payload := queue.latest[source]
	delete(queue.latest, source)
	hasMore := len(queue.order) > 0
	queue.mu.Unlock()
	if hasMore {
		select {
		case queue.ready <- struct{}{}:
		default:
		}
	}
	return payload, true
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

func (state viewerConnectionState) String() string {
	if state == viewerConnected {
		return "connected"
	}
	return "negotiating"
}

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
	sourceKind           string
	displayName          string
	raceCarID            string
	allowObserverCommand bool
	recorder             *telemetryRecorder

	videoTrack *webrtc.TrackLocalStaticRTP
	api        *webrtc.API

	viewersMu      sync.RWMutex
	viewers        map[uint64]*viewer
	nextID         atomic.Uint64
	activeSessions atomic.Int32
	pilotID        uint64

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
	pliViewerFeedback      atomic.Uint64
	pliWatchdog            atomic.Uint64
	nackViewerFeedback     atomic.Uint64
	rtpStalls              atomic.Uint64
	telemetryTextTEL       atomic.Uint64
	telemetryBinaryTEL     atomic.Uint64
	telemetryBinaryAudio   atomic.Uint64
	telemetryOther         atomic.Uint64
	fuelCommandGeneration  atomic.Uint64
	frameRate              frameRateWindow
	vehicleHealth          *vehicleHealth
	pitPresence            *pitPresenceState
	vehicleEvents          *vehicleEventStore
	eventDispatch          chan string
	raceAudio              *raceAudioSource

	rtpRewriteMu          sync.Mutex
	rtpRewriteInitialized bool
	rtpRewriteGeneration  uint64
	rtpSequenceOffset     uint16
	rtpTimestampOffset    uint32
	lastOutputSequence    uint16
	lastOutputTimestamp   uint32
	lastInputTimestamp    uint32
	lastTimestampStep     uint32

	raceStateMu    sync.RWMutex
	raceState      string
	courseProgress courseProgressTracker
	boostRegen     boostRegenProbe

	driveLoggingEnabled atomic.Bool
	driveOwnerID        atomic.Uint64
	driveGear           atomic.Int32
	driveInputLogMu     sync.Mutex
	lastDriveInputLogAt time.Time
	driveStateMu        sync.RWMutex
	driveRevision       uint64
	driveSessionID      string
	driveChangedAt      time.Time
	driveReason         string
}

type relayServer struct {
	sourcesMu               sync.RWMutex
	sourceMutationMu        sync.Mutex
	sources                 map[string]*relay
	sourceOrder             []string
	managedSources          map[string]*managedRelaySource
	sourceRuntime           relaySourceRuntime
	dynamicSourceRegistry   *dynamicSourceRegistry
	recorder                *telemetryRecorder
	raceMu                  sync.RWMutex
	raceContext             relayRaceContext
	raceStreamMu            sync.RWMutex
	raceState               string
	raceSubscribers         map[uint64]*raceSubscriber
	nextRaceSubscriberID    atomic.Uint64
	raceAudioAnnouncementMu sync.Mutex
	raceAudioAnnouncements  map[string]raceAudioAnnouncementReceipt
	racePublishedMessages   atomic.Uint64
	racePublishedBytes      atomic.Uint64
	raceDeliveredMessages   atomic.Uint64
	raceDeliveredBytes      atomic.Uint64
	raceQueueReplacements   atomic.Uint64
	raceWriteErrors         atomic.Uint64
	raceLastPublishedAt     atomic.Int64
	raceLastDeliveredAt     atomic.Int64
	pitEventsMu             sync.Mutex
	pitEvents               map[string]pitPresenceReceipt
	pitEventIDs             []string
	teamObserverDirectory   *teamObserverDirectorySource
}

type raceStateEnvelope struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	RaceID    string `json:"raceId"`
	RaceRunID string `json:"raceRunId"`
	Phase     string `json:"phase"`
	Flag      string `json:"flag"`
	Sequence  uint64 `json:"sequence"`
	RaceInfo  struct {
		SessionType string `json:"sessionType"`
	} `json:"raceInfo"`
	Standings []raceStateStanding `json:"standings"`
}

type raceStateStanding struct {
	CarID             string `json:"carId"`
	Position          int    `json:"position"`
	Lap               int    `json:"lap"`
	Status            string `json:"status"`
	IntervalToAheadMS *int64 `json:"intervalToAheadMs"`
	LapDeltaToAhead   *int   `json:"lapDeltaToAhead"`
	CurrentSector     int    `json:"currentSector"`
	SectorCount       int    `json:"sectorCount"`
	LastMarkerIndex   *int   `json:"lastMarkerIndex"`
	LastMarkerRaceMS  *int64 `json:"lastMarkerRaceMs"`
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
	Version    int                       `json:"version"`
	ServerTime time.Time                 `json:"serverTime"`
	RaceStream raceStreamOperationsState `json:"raceStream"`
	Sources    []sourceOperationsState   `json:"sources"`
}

type raceStreamOperationsState struct {
	Subscribers       int                               `json:"subscribers"`
	HasState          bool                              `json:"hasState"`
	PublishedMessages uint64                            `json:"publishedMessages"`
	PublishedBytes    uint64                            `json:"publishedBytes"`
	DeliveredMessages uint64                            `json:"deliveredMessages"`
	DeliveredBytes    uint64                            `json:"deliveredBytes"`
	QueueReplacements uint64                            `json:"queueReplacements"`
	WriteErrors       uint64                            `json:"writeErrors"`
	LastPublishedAt   *time.Time                        `json:"lastPublishedAt"`
	LastDeliveredAt   *time.Time                        `json:"lastDeliveredAt"`
	Clients           []raceStreamClientOperationsState `json:"clients"`
}

type raceStreamClientOperationsState struct {
	ID                 uint64 `json:"id"`
	RemoteHost         string `json:"remoteHost"`
	ClientKind         string `json:"clientKind"`
	ConnectedAgeMs     int64  `json:"connectedAgeMs"`
	LastDeliveredAgeMs *int64 `json:"lastDeliveredAgeMs"`
	DeliveredMessages  uint64 `json:"deliveredMessages"`
	DeliveredBytes     uint64 `json:"deliveredBytes"`
	QueueReplacements  uint64 `json:"queueReplacements"`
	WriteErrors        uint64 `json:"writeErrors"`
	QueueDepth         int    `json:"queueDepth"`
}

type raceSubscriber struct {
	id                uint64
	remoteHost        string
	clientKind        string
	connectedAt       time.Time
	queue             chan string
	lastDeliveredAt   atomic.Int64
	deliveredMessages atomic.Uint64
	deliveredBytes    atomic.Uint64
	queueReplacements atomic.Uint64
	writeErrors       atomic.Uint64
}

// pilotDevicesStatus は Pilot 用の車両選択画面だけに渡す縮約状態です。
// 運営用 status の復旧回数・エラーコード・DataChannel 診断は含めません。
type pilotDevicesStatus struct {
	Version    int                 `json:"version"`
	ServerTime time.Time           `json:"serverTime"`
	Devices    []pilotDeviceStatus `json:"devices"`
}

type pilotDeviceStatus struct {
	Device       string  `json:"device"`
	CarID        string  `json:"carId"`
	Availability string  `json:"availability"`
	State        string  `json:"state"`
	VideoFPS     float64 `json:"videoFps"`
	PilotInUse   bool    `json:"pilotInUse"`
}

type sourceOperationsState struct {
	ID            string                       `json:"id"`
	SourceKind    string                       `json:"sourceKind"`
	DisplayName   string                       `json:"displayName,omitempty"`
	RaceCarID     string                       `json:"raceCarId,omitempty"`
	State         string                       `json:"state"`
	Lifecycle     string                       `json:"lifecycle"`
	VideoHealth   string                       `json:"videoHealth"`
	Drive         driveOperationsState         `json:"drive"`
	VehicleHealth vehicleHealthOperationsState `json:"vehicleHealth"`
	Upstream      upstreamOperationsState      `json:"upstream"`
	Telemetry     telemetryOperationsState     `json:"telemetry"`
	Downstream    downstreamOperationsState    `json:"downstream"`
	Recovery      recoveryOperationsState      `json:"recovery"`
}

type driveOperationsState struct {
	Enabled       bool       `json:"enabled"`
	Revision      uint64     `json:"revision"`
	SessionID     string     `json:"sessionId,omitempty"`
	ChangedAt     *time.Time `json:"changedAt,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	OwnerViewerID uint64     `json:"ownerViewerId,omitempty"`
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

// DataChannel の payload 種別を source ごとに数える。音声追加後に TEL が
// binary 化されていないかを、実走中でも安全に切り分けるための診断値。
type telemetryOperationsState struct {
	TextTEL     uint64 `json:"textTel"`
	BinaryTEL   uint64 `json:"binaryTel"`
	BinaryAudio uint64 `json:"binaryAudio"`
	Other       uint64 `json:"other"`
}

type vehicleHealthOperationsState struct {
	DamageEnabled       bool    `json:"damageEnabled"`
	HP                  float64 `json:"hp"`
	SpeedCap            float64 `json:"speedCap"`
	Mode                string  `json:"mode"`
	RecoveryMode        string  `json:"recoveryMode"`
	Fuel                float64 `json:"fuel"`
	FuelState           string  `json:"fuelState"`
	Boost               float64 `json:"boost"`
	BoostState          string  `json:"boostState"`
	BoostRemainingMS    int64   `json:"boostRemainingMs"`
	Gear                int     `json:"gear"`
	Position            int     `json:"position"`
	FieldSize           int     `json:"fieldSize"`
	FuelRatePerSec      float64 `json:"fuelRatePerSecond"`
	FuelRateMultiplier  float64 `json:"fuelRateMultiplier"`
	FuelPowerScale      float64 `json:"fuelPowerScale"`
	FuelRoughMultiplier float64 `json:"fuelRoughMultiplier"`
	FuelBoostMultiplier float64 `json:"fuelBoostMultiplier"`
}

type downstreamOperationsState struct {
	PilotLeaseReserved bool                    `json:"pilotLeaseReserved"`
	NegotiatingPeers   int                     `json:"negotiatingPeers"`
	ConnectedPilots    int                     `json:"connectedPilots"`
	ConnectedObservers int                     `json:"connectedObservers"`
	TelemetryOpen      int                     `json:"telemetryChannelsOpen"`
	RaceOpen           int                     `json:"raceChannelsOpen"`
	EventsOpen         int                     `json:"eventsChannelsOpen"`
	Clients            []viewerOperationsState `json:"clients"`
}

type viewerOperationsState struct {
	ID                         uint64 `json:"id"`
	Role                       string `json:"role"`
	ClientKind                 string `json:"clientKind"`
	RemoteHost                 string `json:"remoteHost"`
	State                      string `json:"state"`
	DownlinkTransport          string `json:"downlinkTransport"`
	TelemetryDataChannelState  string `json:"telemetryDataChannelState"`
	CommandDataChannelState    string `json:"commandDataChannelState"`
	RaceDataChannelState       string `json:"raceDataChannelState"`
	EventsDataChannelState     string `json:"eventsDataChannelState"`
	TelemetryWebSocket         bool   `json:"telemetryWebSocket"`
	RaceWebSocket              bool   `json:"raceWebSocket"`
	EventsWebSocket            bool   `json:"eventsWebSocket"`
	LastTelemetryDeliveryAgeMs *int64 `json:"lastTelemetryDeliveryAgeMs"`
	LastCommandAgeMs           *int64 `json:"lastCommandAgeMs"`
	TelemetryMessages          uint64 `json:"telemetryMessages"`
	TelemetryBytes             uint64 `json:"telemetryBytes"`
	TelemetrySendErrors        uint64 `json:"telemetrySendErrors"`
	TelemetryDropped           uint64 `json:"telemetryDropped"`
	TelemetryThrottled         uint64 `json:"telemetryThrottled"`
}

type pliRequestCounts struct {
	NewTrack       uint64 `json:"newTrack"`
	ViewerConnect  uint64 `json:"viewerConnect"`
	ViewerFeedback uint64 `json:"viewerFeedback"`
	Watchdog       uint64 `json:"watchdog"`
}

type recoveryOperationsState struct {
	PLIRequests   pliRequestCounts `json:"pliRequests"`
	NACKRequests  uint64           `json:"nackRequests"`
	RTPStalls     uint64           `json:"rtpStalls"`
	RetryAttempts uint64           `json:"retryAttempts"`
	LastErrorCode *string          `json:"lastErrorCode"`
}

func newRelay(name string, upstreamURL string, raceCarID string, allowObserverCommand bool,
	rtpStallTimeout time.Duration, upstreamStartTimeout time.Duration,
	healthRecoveryMode vehicleHealthRecoveryMode, fuelDriveDurations ...time.Duration) (*relay, error) {
	api, err := newH264API()
	if err != nil {
		return nil, err
	}
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(h264Codec, "video", "momo")
	if err != nil {
		return nil, fmt.Errorf("create local H264 track: %w", err)
	}
	fuelDriveDuration := vehicleFuelDefaultDriveDuration
	if len(fuelDriveDurations) > 0 && fuelDriveDurations[0] > 0 {
		fuelDriveDuration = fuelDriveDurations[0]
	}
	relay := &relay{
		name:                 name,
		upstreamURL:          upstreamURL,
		sourceKind:           relaySourceKindVehicle,
		displayName:          name,
		raceCarID:            raceCarID,
		allowObserverCommand: allowObserverCommand,
		videoTrack:           videoTrack,
		api:                  api,
		viewers:              make(map[uint64]*viewer),
		rtpStallTimeout:      rtpStallTimeout,
		upstreamStartTimeout: upstreamStartTimeout,
		pilotCommandTimeout:  defaultPilotCommandTimeout,
		vehicleHealth:        newVehicleHealthWithFuelDuration(time.Now(), fuelDriveDuration),
		vehicleEvents:        newVehicleEventStore(),
		eventDispatch:        make(chan string, vehicleEventQueueLimit),
	}
	relay.pitPresence = newPitPresenceState(raceCarID, vehicleHealthMaximum)
	relay.lifecycle.Store(int32(sourceWaiting))
	relay.videoHealth.Store(int32(videoNotStarted))
	relay.upstreamPeerState.Store("new")
	relay.lastErrorCode.Store("")
	relay.vehicleHealth.setRecoveryMode(healthRecoveryMode)
	return relay, nil
}

func newH264API() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: h264Codec,
		PayloadType:        102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register H264 codec: %w", err)
	}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: opusCodec,
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register Opus codec: %w", err)
	}
	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	), nil
}

func (r *relay) start(ctx context.Context) {
	go r.watchPilotCommands(ctx)
	go r.runVehicleEventDispatcher(ctx)
	if r.raceAudio != nil {
		r.raceAudio.start(ctx)
	}
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
	connectionDone := make(chan struct{})
	defer close(connectionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = pc.Close()
			_ = ws.Close()
		case <-connectionDone:
		}
	}()
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
		r.handleUpstreamTelemetry(message, generation)
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
	displayName := strings.TrimSpace(r.displayName)
	if displayName == "" {
		displayName = r.name
	}
	var lastRtpAgeMs *int64
	if lastFrame != 0 {
		age := now.Sub(time.Unix(0, lastFrame)).Milliseconds()
		if age < 0 {
			age = 0
		}
		lastRtpAgeMs = &age
	}
	ingressFPS, relayWriteFPS := r.frameRate.snapshot(now)
	health := r.vehicleHealth.snapshot(now)

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
		SourceKind:  effectiveRelaySourceKind(r.sourceKind),
		DisplayName: displayName,
		RaceCarID:   r.raceCarID,
		State:       displaySourceState(lifecycle, videoHealth),
		Lifecycle:   lifecycle.String(),
		VideoHealth: videoHealth.String(),
		Drive:       r.driveStatusSnapshot(),
		VehicleHealth: vehicleHealthOperationsState{
			DamageEnabled:       health.DamageEnabled,
			HP:                  health.HP,
			SpeedCap:            health.SpeedCap,
			Mode:                health.Mode,
			RecoveryMode:        string(r.vehicleHealth.recoveryModeSnapshot()),
			Fuel:                health.Fuel,
			FuelState:           health.FuelState,
			Boost:               health.Boost,
			BoostState:          health.BoostState,
			BoostRemainingMS:    health.BoostRemainingMS,
			Gear:                health.Gear,
			Position:            health.Position,
			FieldSize:           health.FieldSize,
			FuelRatePerSec:      health.FuelRatePerSec,
			FuelRateMultiplier:  health.FuelRateMultiplier,
			FuelPowerScale:      health.FuelPowerScale,
			FuelRoughMultiplier: health.FuelRoughMultiplier,
			FuelBoostMultiplier: health.FuelBoostMultiplier,
		},
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
		Telemetry: telemetryOperationsState{
			TextTEL:     r.telemetryTextTEL.Load(),
			BinaryTEL:   r.telemetryBinaryTEL.Load(),
			BinaryAudio: r.telemetryBinaryAudio.Load(),
			Other:       r.telemetryOther.Load(),
		},
		Downstream: r.downstreamStatusSnapshot(now),
		Recovery: recoveryOperationsState{
			PLIRequests: pliRequestCounts{
				NewTrack:       r.pliNewTrack.Load(),
				ViewerConnect:  r.pliViewerConnect.Load(),
				ViewerFeedback: r.pliViewerFeedback.Load(),
				Watchdog:       r.pliWatchdog.Load(),
			},
			NACKRequests:  r.nackViewerFeedback.Load(),
			RTPStalls:     r.rtpStalls.Load(),
			RetryAttempts: retries,
			LastErrorCode: lastErrorCode,
		},
	}
}

func (r *relay) downstreamStatusSnapshot(now time.Time) downstreamOperationsState {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	state := downstreamOperationsState{
		PilotLeaseReserved: r.pilotID != 0,
		Clients:            make([]viewerOperationsState, 0, len(r.viewers)),
	}
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
		if client.events.Load() != nil {
			state.EventsOpen++
		}
		state.Clients = append(state.Clients, viewerStatusSnapshot(client, now))
	}
	sort.Slice(state.Clients, func(left, right int) bool { return state.Clients[left].ID < state.Clients[right].ID })
	return state
}

func viewerStatusSnapshot(client *viewer, now time.Time) viewerOperationsState {
	clientKind := client.clientKind
	if clientKind == "" {
		clientKind = "native"
	}
	return viewerOperationsState{
		ID:                         client.id,
		Role:                       client.role,
		ClientKind:                 clientKind,
		RemoteHost:                 remoteHost(client.remoteAddr),
		State:                      viewerConnectionState(client.state.Load()).String(),
		DownlinkTransport:          viewerDownlinkTransport(client),
		TelemetryDataChannelState:  dataChannelState(client.telemetry.Load()),
		CommandDataChannelState:    dataChannelState(client.command.Load()),
		RaceDataChannelState:       dataChannelState(client.race.Load()),
		EventsDataChannelState:     dataChannelState(client.events.Load()),
		TelemetryWebSocket:         client.telemetryWS != nil,
		RaceWebSocket:              client.raceWS != nil,
		EventsWebSocket:            client.eventsWS != nil,
		LastTelemetryDeliveryAgeMs: ageSinceUnixNano(client.lastTelemetrySentAt.Load(), now),
		LastCommandAgeMs:           ageSinceUnixNano(client.lastCommandUnixNano.Load(), now),
		TelemetryMessages:          client.telemetryMessages.Load(),
		TelemetryBytes:             client.telemetryBytes.Load(),
		TelemetrySendErrors:        client.telemetrySendErrors.Load(),
		TelemetryDropped:           client.telemetryDropped.Load(),
		TelemetryThrottled:         client.telemetryThrottled.Load(),
	}
}

func viewerDownlinkTransport(client *viewer) string {
	if client.telemetryWS != nil {
		return "websocket"
	}
	if client.telemetry.Load() != nil {
		return "datachannel"
	}
	return "pending"
}

func dataChannelState(channel *webrtc.DataChannel) string {
	if channel == nil {
		return "absent"
	}
	return channel.ReadyState().String()
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func ageSinceUnixNano(value int64, now time.Time) *int64 {
	if value == 0 {
		return nil
	}
	age := now.Sub(time.Unix(0, value)).Milliseconds()
	if age < 0 {
		age = 0
	}
	return &age
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
	sourceSnapshot := server.orderedSourcesSnapshot()
	sources := make([]sourceOperationsState, 0, len(sourceSnapshot))
	for _, source := range sourceSnapshot {
		sources = append(sources, source.statusSnapshot(now))
	}
	return operationsStatus{
		Version:    3,
		ServerTime: now.UTC(),
		RaceStream: server.raceStreamStatusSnapshot(),
		Sources:    sources,
	}
}

func (server *relayServer) raceStreamStatusSnapshot() raceStreamOperationsState {
	server.raceStreamMu.RLock()
	defer server.raceStreamMu.RUnlock()
	now := time.Now()
	clients := make([]raceStreamClientOperationsState, 0, len(server.raceSubscribers))
	for _, subscriber := range server.raceSubscribers {
		clients = append(clients, raceStreamClientOperationsState{
			ID:                 subscriber.id,
			RemoteHost:         subscriber.remoteHost,
			ClientKind:         subscriber.clientKind,
			ConnectedAgeMs:     now.Sub(subscriber.connectedAt).Milliseconds(),
			LastDeliveredAgeMs: ageSinceUnixMilliseconds(subscriber.lastDeliveredAt.Load(), now),
			DeliveredMessages:  subscriber.deliveredMessages.Load(),
			DeliveredBytes:     subscriber.deliveredBytes.Load(),
			QueueReplacements:  subscriber.queueReplacements.Load(),
			WriteErrors:        subscriber.writeErrors.Load(),
			QueueDepth:         len(subscriber.queue),
		})
	}
	sort.Slice(clients, func(left, right int) bool { return clients[left].ID < clients[right].ID })
	return raceStreamOperationsState{
		Subscribers:       len(server.raceSubscribers),
		HasState:          strings.TrimSpace(server.raceState) != "",
		PublishedMessages: server.racePublishedMessages.Load(),
		PublishedBytes:    server.racePublishedBytes.Load(),
		DeliveredMessages: server.raceDeliveredMessages.Load(),
		DeliveredBytes:    server.raceDeliveredBytes.Load(),
		QueueReplacements: server.raceQueueReplacements.Load(),
		WriteErrors:       server.raceWriteErrors.Load(),
		LastPublishedAt:   timeFromUnixMilliseconds(server.raceLastPublishedAt.Load()),
		LastDeliveredAt:   timeFromUnixMilliseconds(server.raceLastDeliveredAt.Load()),
		Clients:           clients,
	}
}

func ageSinceUnixMilliseconds(value int64, now time.Time) *int64 {
	if value == 0 {
		return nil
	}
	age := now.Sub(time.UnixMilli(value)).Milliseconds()
	if age < 0 {
		age = 0
	}
	return &age
}

func timeFromUnixMilliseconds(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	result := time.UnixMilli(value).UTC()
	return &result
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

func (server *relayServer) serveRaceState(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	state := strings.TrimSpace(server.currentGlobalRaceState())
	if state != "" {
		state = strings.TrimPrefix(state, "RACE:")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(state))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *relayServer) publishGlobalRaceState(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	now := time.Now()
	server.racePublishedMessages.Add(1)
	server.racePublishedBytes.Add(uint64(len(message)))
	server.raceLastPublishedAt.Store(now.UnixMilli())
	server.raceStreamMu.Lock()
	server.raceState = message
	for _, subscriber := range server.raceSubscribers {
		if enqueueLatestRaceState(subscriber.queue, message) {
			server.raceQueueReplacements.Add(1)
			subscriber.queueReplacements.Add(1)
		}
	}
	server.raceStreamMu.Unlock()
}

func enqueueLatestRaceState(queue chan string, payload string) bool {
	select {
	case queue <- payload:
		return false
	default:
	}
	replaced := false
	select {
	case <-queue:
		replaced = true
	default:
	}
	select {
	case queue <- payload:
	default:
	}
	return replaced
}

func (server *relayServer) currentGlobalRaceState() string {
	server.raceStreamMu.RLock()
	defer server.raceStreamMu.RUnlock()
	return server.raceState
}

func (server *relayServer) subscribeRaceState(remoteAddr string, clientKind string) (*raceSubscriber, string) {
	id := server.nextRaceSubscriberID.Add(1)
	clientKind = strings.TrimSpace(clientKind)
	if clientKind == "" {
		clientKind = "web"
	}
	subscriber := &raceSubscriber{
		id:          id,
		remoteHost:  remoteHost(remoteAddr),
		clientKind:  clientKind,
		connectedAt: time.Now(),
		queue:       make(chan string, 1),
	}
	server.raceStreamMu.Lock()
	if server.raceSubscribers == nil {
		server.raceSubscribers = make(map[uint64]*raceSubscriber)
	}
	server.raceSubscribers[id] = subscriber
	current := server.raceState
	server.raceStreamMu.Unlock()
	return subscriber, current
}

func (server *relayServer) unsubscribeRaceState(id uint64) {
	server.raceStreamMu.Lock()
	delete(server.raceSubscribers, id)
	server.raceStreamMu.Unlock()
}

func (server *relayServer) serveRaceStateWS(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ws, err := wsUpgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("upgrade race state WebSocket: %v", err)
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1024)
	_ = ws.SetReadDeadline(time.Now().Add(raceStreamPongWait))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(raceStreamPongWait))
	})
	subscriber, current := server.subscribeRaceState(req.RemoteAddr, req.URL.Query().Get("client"))
	defer server.unsubscribeRaceState(subscriber.id)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := ws.NextReader(); err != nil {
				return
			}
		}
	}()
	send := func(payload string) error {
		_ = ws.SetWriteDeadline(time.Now().Add(raceStreamWriteTimeout))
		if err := ws.WriteJSON(signalMessage{Type: "race-state", Data: payload}); err != nil {
			server.raceWriteErrors.Add(1)
			subscriber.writeErrors.Add(1)
			return err
		}
		server.raceDeliveredMessages.Add(1)
		server.raceDeliveredBytes.Add(uint64(len(payload)))
		deliveredAt := time.Now().UnixMilli()
		server.raceLastDeliveredAt.Store(deliveredAt)
		subscriber.lastDeliveredAt.Store(deliveredAt)
		subscriber.deliveredMessages.Add(1)
		subscriber.deliveredBytes.Add(uint64(len(payload)))
		return nil
	}
	sendHeartbeat := func() error {
		deadline := time.Now().Add(raceStreamWriteTimeout)
		_ = ws.SetWriteDeadline(deadline)
		if err := ws.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
			server.raceWriteErrors.Add(1)
			subscriber.writeErrors.Add(1)
			return err
		}
		if err := ws.WriteJSON(signalMessage{Type: "race-heartbeat"}); err != nil {
			server.raceWriteErrors.Add(1)
			subscriber.writeErrors.Add(1)
			return err
		}
		return nil
	}
	if strings.TrimSpace(current) != "" {
		if err := send(current); err != nil {
			return
		}
	}
	heartbeat := time.NewTicker(raceStreamPingInterval)
	defer heartbeat.Stop()
	for {
		select {
		case payload := <-subscriber.queue:
			if err := send(payload); err != nil {
				return
			}
		case <-heartbeat.C:
			if err := sendHeartbeat(); err != nil {
				return
			}
		case <-done:
			return
		case <-req.Context().Done():
			return
		}
	}
}

func (r *relay) sendInitialWebDownlinkState(send func(signalMessage) error) error {
	for _, payload := range r.currentGameplayMessages(time.Now()) {
		if payload != "" {
			if err := send(signalMessage{Type: "telemetry", Data: payload}); err != nil {
				return err
			}
		}
	}
	payload, err := marshalVehicleEvent(r.vehicleEvents.snapshot())
	if err != nil {
		return fmt.Errorf("encode initial web downlink vehicle event snapshot: %w", err)
	}
	return send(signalMessage{Type: "vehicle-event", Data: payload})
}

func (server *relayServer) pilotDevicesSnapshot(now time.Time) pilotDevicesStatus {
	sourceSnapshot := server.orderedSourcesSnapshot()
	devices := make([]pilotDeviceStatus, 0, len(sourceSnapshot))
	for _, source := range sourceSnapshot {
		if effectiveRelaySourceKind(source.sourceKind) != relaySourceKindVehicle {
			continue
		}
		status := source.statusSnapshot(now)
		pilotInUse := status.Downstream.PilotLeaseReserved || status.Downstream.ConnectedPilots > 0
		videoFPS := status.Upstream.RelayWriteAccessUnitFPS
		if videoFPS == 0 {
			videoFPS = status.Upstream.IngressAccessUnitFPS
		}
		devices = append(devices, pilotDeviceStatus{
			Device:       status.ID,
			CarID:        status.RaceCarID,
			Availability: pilotDeviceAvailability(status.State, pilotInUse),
			State:        status.State,
			VideoFPS:     videoFPS,
			PilotInUse:   pilotInUse,
		})
	}
	return pilotDevicesStatus{Version: 1, ServerTime: now.UTC(), Devices: devices}
}

func pilotDeviceAvailability(state string, pilotInUse bool) string {
	if pilotInUse {
		return "in_use"
	}
	if state == "STREAMING" {
		return "ready"
	}
	if state == "WAITING" || state == "CONNECTING" {
		return "connecting"
	}
	return "unavailable"
}

func (server *relayServer) servePilotDevices(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(server.pilotDevicesSnapshot(time.Now()))
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

func webAssetHandler(webRoot fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(webRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(strings.ToLower(req.URL.Path), ".mjs") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		fileServer.ServeHTTP(w, req)
	})
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
	case "viewer_feedback":
		r.pliViewerFeedback.Add(1)
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

func classifyDownstreamRTCP(packets []rtcp.Packet) (requestKeyframe bool, nackRequests uint64) {
	for _, packet := range packets {
		switch packet.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			requestKeyframe = true
		case *rtcp.TransportLayerNack:
			nackRequests++
		}
	}
	return requestKeyframe, nackRequests
}

// RTPSender.ReadRTCP lets the registered interceptors process downstream NACKs.
// A keyframe request cannot be fulfilled by the Relay, so forward it to Momo.
func (r *relay) readDownstreamRTCP(sender *webrtc.RTPSender) {
	for {
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		requestKeyframe, nackRequests := classifyDownstreamRTCP(packets)
		if nackRequests > 0 {
			r.nackViewerFeedback.Add(nackRequests)
		}
		if requestKeyframe {
			r.requestKeyframe("viewer_feedback")
		}
	}
}

func (r *relay) broadcastTelemetry(message webrtc.DataChannelMessage) {
	now := time.Now()
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if client.role == "pilot" && isM5AudioMessage(message) && client.audioWS != nil {
			if client.audioSubscribed.Load() {
				enqueueLatestTelemetry(client.audioWS, string(message.Data))
			}
			continue
		}
		if client.clientKind == "web-observer" && isM5AudioMessage(message) {
			continue
		}
		if client.clientKind == "web-observer" && !shouldDeliverObserverTelemetry(client, message, now) {
			client.telemetryThrottled.Add(1)
			continue
		}
		if message.IsString && client.telemetryWS != nil {
			payload := string(message.Data)
			if isVehicleGameplayTelemetry(message) && client.gameplayWS != nil {
				if !enqueueGameplayTelemetry(client.gameplayWS, payload) {
					client.telemetryDropped.Add(1)
					if shouldLogTelemetryDelivery(client, now) {
						log.Printf("source %q: gameplay delivery viewer=%d role=%s remote=%s transport=websocket action=drop queue=%d dropped=%d",
							r.name, client.id, client.role, client.remoteAddr, len(client.gameplayWS), client.telemetryDropped.Load())
					}
				}
			} else if source, state := telemetryStateSource(message); state && client.telemetryStateWS != nil {
				client.telemetryStateWS.Enqueue(source, payload)
			} else {
				enqueueLatestTelemetry(client.telemetryWS, payload)
			}
			client.telemetryMessages.Add(1)
			client.telemetryBytes.Add(uint64(len(message.Data)))
			client.lastTelemetrySentAt.Store(now.UnixNano())
			if shouldLogTelemetryDelivery(client, now) {
				log.Printf("source %q: telemetry delivery viewer=%d role=%s remote=%s transport=websocket messages=%d bytes=%d errors=%d",
					r.name, client.id, client.role, client.remoteAddr,
					client.telemetryMessages.Load(), client.telemetryBytes.Load(), client.telemetrySendErrors.Load())
			}
			continue
		}
		if channel := client.telemetry.Load(); channel != nil {
			if telemetryDataChannelSaturated(channel.BufferedAmount()) {
				client.telemetryDropped.Add(1)
				if client.role == "pilot" && shouldLogTelemetryDelivery(client, time.Now()) {
					log.Printf("source %q: telemetry delivery viewer=%d role=%s remote=%s transport=datachannel action=drop buffered=%d dropped=%d errors=%d",
						r.name, client.id, client.role, client.remoteAddr, channel.BufferedAmount(),
						client.telemetryDropped.Load(), client.telemetrySendErrors.Load())
				}
				continue
			}
			if err := sendDataChannel(channel, message); err != nil {
				client.telemetrySendErrors.Add(1)
				log.Printf("send telemetry to viewer %d: %v", client.id, err)
			} else {
				client.telemetryMessages.Add(1)
				client.telemetryBytes.Add(uint64(len(message.Data)))
				client.lastTelemetrySentAt.Store(now.UnixNano())
				if client.role == "pilot" && shouldLogTelemetryDelivery(client, time.Now()) {
					log.Printf("source %q: telemetry delivery viewer=%d role=%s remote=%s messages=%d bytes=%d state=%s buffered=%d errors=%d",
						r.name, client.id, client.role, client.remoteAddr,
						client.telemetryMessages.Load(), client.telemetryBytes.Load(),
						channel.ReadyState().String(), channel.BufferedAmount(), client.telemetrySendErrors.Load())
				}
			}
		}
	}
}

func shouldDeliverObserverTelemetry(client *viewer, message webrtc.DataChannelMessage, now time.Time) bool {
	if client == nil || !message.IsString || !bytes.HasPrefix(message.Data, []byte("TEL:")) {
		return true
	}
	if bytes.Contains(message.Data, []byte(`"k":"e"`)) || bytes.Contains(message.Data, []byte(`"evt"`)) {
		return true
	}
	source, state := telemetryStateSource(message)
	if !state {
		return true
	}
	nowUnixNano := now.UnixNano()
	client.observerStateMu.Lock()
	defer client.observerStateMu.Unlock()
	if client.observerStateAt == nil {
		client.observerStateAt = make(map[string]int64)
	}
	if _, exists := client.observerStateAt[source]; !exists && len(client.observerStateAt) >= maxTelemetryStateSources {
		source = "_overflow"
	}
	previous := client.observerStateAt[source]
	if previous != 0 && nowUnixNano-previous < observerTelemetryInterval.Nanoseconds() {
		return false
	}
	client.observerStateAt[source] = nowUnixNano
	return true
}

func telemetryStateSource(message webrtc.DataChannelMessage) (string, bool) {
	if !message.IsString || !bytes.HasPrefix(message.Data, []byte("TEL:")) {
		return "", false
	}
	var envelope struct {
		Kind   string `json:"k"`
		Source string `json:"src"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(message.Data, []byte("TEL:")), &envelope); err != nil || envelope.Kind != "s" {
		return "", false
	}
	if !isTelemetrySourceName(envelope.Source) {
		return "_default", true
	}
	return envelope.Source, true
}

func isTelemetrySourceName(source string) bool {
	if len(source) == 0 || len(source) > 16 {
		return false
	}
	for index := 0; index < len(source); index++ {
		character := source[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func isM5AudioMessage(message webrtc.DataChannelMessage) bool {
	return bytes.HasPrefix(message.Data, []byte("AUD:"))
}

func isVehicleGameplayTelemetry(message webrtc.DataChannelMessage) bool {
	if !message.IsString {
		return false
	}
	return bytes.HasPrefix(message.Data, []byte("VHS:")) ||
		bytes.HasPrefix(message.Data, []byte("VGS:")) ||
		bytes.HasPrefix(message.Data, []byte("PIT:"))
}

func telemetryDataChannelSaturated(buffered uint64) bool {
	return buffered >= telemetryDataHighWatermark
}

func enqueueLatestTelemetry(queue chan string, payload string) {
	select {
	case queue <- payload:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- payload:
	default:
	}
}

func enqueueGameplayTelemetry(queue chan string, payload string) bool {
	select {
	case queue <- payload:
		return true
	default:
		return false
	}
}

func shouldLogTelemetryDelivery(client *viewer, now time.Time) bool {
	if client == nil {
		return false
	}
	current := now.UnixNano()
	for {
		previous := client.lastTelemetryLog.Load()
		if previous != 0 && current-previous < telemetryDeliveryLogInterval.Nanoseconds() {
			return false
		}
		if client.lastTelemetryLog.CompareAndSwap(previous, current) {
			return true
		}
	}
}

func normalizeTelemetryMessage(message webrtc.DataChannelMessage) (webrtc.DataChannelMessage, string, bool, bool) {
	if message.IsString && strings.HasPrefix(string(message.Data), "TEL:") {
		return message, string(message.Data), true, false
	}
	if !message.IsString && bytes.HasPrefix(message.Data, []byte("TEL:")) {
		// Momo の serial DataChannel は、同じ TEL 行でも機体ごとに binary
		// として届くことがある。Viewer と NDJSON 契約は Text TEL を前提と
		// するため、内容が TEL: である場合だけ Relay 境界で正規化する。
		message.IsString = true
		return message, string(message.Data), true, true
	}
	return message, "", false, false
}

func (r *relay) handleUpstreamTelemetry(message webrtc.DataChannelMessage, generation uint64) {
	normalized, raw, isTEL, wasBinaryTEL := normalizeTelemetryMessage(message)
	if isTEL {
		now := time.Now()
		if wasBinaryTEL {
			r.telemetryBinaryTEL.Add(1)
		} else {
			r.telemetryTextTEL.Add(1)
		}
		if r.recorder != nil && r.driveLoggingEnabled.Load() {
			r.recorder.RecordTelemetry(r.name, r.raceCarID, generation, raw)
		}
		message = normalized
		if r.upstreamGeneration.Load() == generation && telemetryHasCapability(raw, fuelCommandCapability) {
			r.fuelCommandGeneration.Store(generation)
		}
		health, publish, event := r.vehicleHealth.ingestTelemetry(raw, r.raceCarID, now)
		var regenApplied bool
		health, regenApplied = r.observeBoostRegenTelemetry(raw, health, event, now)
		if event != nil {
			r.publishVehicleEvent(*event)
		} else if isLegacyImpactEvent(raw) {
			log.Printf("source %q: ignore diagnostic V1 impact event: legacy_event_unsupported", r.name)
		}
		if publish || regenApplied {
			r.broadcastVehicleGameplay(health)
			if r.pitPresence != nil {
				if pit, changed := r.pitPresence.observeHealth(health); changed {
					r.broadcastPitPresence(pit)
				}
			}
		}
	} else if bytes.HasPrefix(message.Data, []byte("AUD:")) {
		r.telemetryBinaryAudio.Add(1)
	} else {
		r.telemetryOther.Add(1)
	}
	r.broadcastTelemetry(message)
}

func telemetryHasCapability(message string, capability string) bool {
	if capability == "" || !strings.HasPrefix(message, "TEL:") {
		return false
	}
	var payload struct {
		Version int    `json:"v"`
		Kind    string `json:"k"`
		Source  string `json:"src"`
		Quality struct {
			Flags []string `json:"f"`
		} `json:"q"`
		LegacyQuality struct {
			Flags []string `json:"flags"`
		} `json:"qual"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(message, "TEL:"))), &payload); err != nil ||
		payload.Kind != "s" || payload.Source != "imu0" {
		return false
	}
	flags := payload.Quality.Flags
	if payload.Version == 1 {
		flags = payload.LegacyQuality.Flags
	} else if payload.Version != 2 {
		return false
	}
	for _, flag := range flags {
		if flag == capability {
			return true
		}
	}
	return false
}

func (r *relay) broadcastVehicleHealth(health vehicleHealthSnapshot) {
	r.broadcastTelemetry(webrtc.DataChannelMessage{
		Data:     []byte(formatVehicleHealthTelemetry(health)),
		IsString: true,
	})
}

func (r *relay) broadcastVehicleGameplay(health vehicleHealthSnapshot) {
	r.broadcastVehicleHealth(health)
	message := formatVehicleGameplayTelemetry(health)
	if message == "" {
		return
	}
	r.broadcastTelemetry(webrtc.DataChannelMessage{Data: []byte(message), IsString: true})
	if r.raceAudio != nil {
		r.raceAudio.observeVehicleGameplay(health)
	}
}

// Race Control の状態は操縦テレメトリーと分離して配る。
// LAN Web Viewer は signaling WebSocket、外部 Ayame Viewer は reliable DataChannel を使い、
// 順位やフラグを低遅延・非信頼の telemetry channel に混在させない。
func (r *relay) publishRaceState(message string) {
	r.raceStateMu.Lock()
	r.raceState = message
	r.raceStateMu.Unlock()
	r.broadcastRaceState(message)
	if r.raceAudio != nil {
		r.raceAudio.observe(message)
	}
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
		if client.raceWS != nil {
			enqueueLatestTelemetry(client.raceWS, message)
			continue
		}
		if channel := client.race.Load(); channel != nil {
			r.sendRaceState(client, channel, message)
		}
	}
}

func (r *relay) publishVehicleEvent(event vehicleImpactEvent) {
	if r.vehicleEvents == nil || !r.vehicleEvents.add(event) {
		return
	}
	r.recorder.RecordVehicleEvent(r.name, r.raceCarID, event)
	message, err := marshalVehicleEvent(event)
	if err != nil {
		log.Printf("source %q: encode vehicle event %q: %v", r.name, event.EventID, err)
		return
	}
	r.enqueueVehicleEventMessage(message)
}

func (r *relay) resetVehicleEvents(raceRunID string) {
	if r.vehicleEvents == nil || !r.vehicleEvents.reset(raceRunID) {
		return
	}
	message, err := marshalVehicleEvent(r.vehicleEvents.snapshot())
	if err != nil {
		log.Printf("source %q: encode empty vehicle event snapshot: %v", r.name, err)
		return
	}
	r.enqueueVehicleEventMessage(message)
}

func (r *relay) enqueueVehicleEventMessage(message string) {
	if r.eventDispatch == nil {
		return
	}
	select {
	case r.eventDispatch <- message:
	default:
		log.Printf("source %q: drop vehicle event delivery because the bounded queue is full", r.name)
	}
}

func (r *relay) runVehicleEventDispatcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-r.eventDispatch:
			r.broadcastVehicleEventMessage(message)
		}
	}
}

func (r *relay) broadcastVehicleEventMessage(message string) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if client.eventsWS != nil {
			if !enqueueVehicleEvent(client.eventsWS, message) {
				log.Printf("source %q: drop WebSocket vehicle event for viewer %d because the bounded queue is full", r.name, client.id)
			}
			continue
		}
		if channel := client.events.Load(); channel != nil {
			r.sendVehicleEventMessage(client, channel, message)
		}
	}
}

func enqueueVehicleEvent(queue chan string, message string) bool {
	select {
	case queue <- message:
		return true
	default:
		return false
	}
}

func (r *relay) sendVehicleEventMessage(client *viewer, channel *webrtc.DataChannel, message string) bool {
	client.eventsSendMu.Lock()
	defer client.eventsSendMu.Unlock()
	if client.events.Load() != channel {
		return false
	}
	if err := channel.SendText(message); err != nil {
		client.events.CompareAndSwap(channel, nil)
		log.Printf("send vehicle event to viewer %d: %v", client.id, err)
		_ = channel.Close()
		return false
	}
	return true
}

func (r *relay) openVehicleEventsChannel(client *viewer, channel *webrtc.DataChannel) {
	client.eventsSendMu.Lock()
	defer client.eventsSendMu.Unlock()
	client.events.Store(channel)
	message, err := marshalVehicleEvent(r.vehicleEvents.snapshot())
	if err != nil {
		log.Printf("source %q: encode vehicle event snapshot: %v", r.name, err)
		return
	}
	if client.eventsWS != nil {
		if !enqueueVehicleEvent(client.eventsWS, message) {
			log.Printf("source %q: drop WebSocket vehicle event snapshot for viewer %d because the bounded queue is full", r.name, client.id)
		}
		return
	}
	if err := channel.SendText(message); err != nil {
		client.events.CompareAndSwap(channel, nil)
		log.Printf("send vehicle event snapshot to viewer %d: %v", client.id, err)
		_ = channel.Close()
	}
}

func (r *relay) sendRaceState(client *viewer, channel *webrtc.DataChannel, message string) bool {
	client.raceSendMu.Lock()
	defer client.raceSendMu.Unlock()
	return r.sendRaceStateLocked(client, channel, message)
}

func (r *relay) sendRaceStateLocked(client *viewer, channel *webrtc.DataChannel, message string) bool {
	if client.race.Load() != channel {
		return false
	}
	if err := channel.SendText(message); err != nil {
		client.race.CompareAndSwap(channel, nil)
		log.Printf("send race state to viewer %d: %v", client.id, err)
		_ = channel.Close()
		return false
	}
	return true
}

func (r *relay) sendCurrentRaceState(client *viewer, channel *webrtc.DataChannel) {
	client.raceSendMu.Lock()
	defer client.raceSendMu.Unlock()
	if client.race.Load() != channel {
		return
	}
	message := r.currentRaceState()
	if message == "" {
		return
	}
	r.sendRaceStateLocked(client, channel, message)
}

func (r *relay) broadcastCommand(message webrtc.DataChannelMessage) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if client.role == "pilot" {
			// Pilot は自分の入力値をローカル表示しており、Relay からの監査エコーは
			// 使用しない。高頻度の下り SCTP トラフィックを作らないよう送信しない。
			continue
		}
		if message.IsString && client.commandWS != nil {
			enqueueLatestTelemetry(client.commandWS, string(message.Data))
			continue
		}
		if channel := client.command.Load(); channel != nil {
			if err := sendDataChannel(channel, message); err != nil {
				log.Printf("send command audit to viewer %d: %v", client.id, err)
			}
		}
	}
}

func commandAuditWithGear(message webrtc.DataChannelMessage, gear int32) webrtc.DataChannelMessage {
	if !message.IsString || gear < 1 || gear > 5 {
		return message
	}
	text := strings.TrimSpace(string(message.Data))
	hasThrottle := false
	for _, field := range strings.Split(text, ",") {
		field = strings.TrimSpace(field)
		if !strings.HasPrefix(field, "T:") {
			continue
		}
		pwm, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(field, "T:")))
		if err == nil && pwm >= 1000 && pwm <= 2000 {
			hasThrottle = true
		}
		break
	}
	if !hasThrottle {
		return message
	}
	message.Data = []byte(fmt.Sprintf("%s,G:%d", text, gear))
	return message
}

func commandWithFuelPercent(message string, fuel float64) string {
	if _, _, ok := parseDriveCommand(message); !ok {
		return message
	}
	lineEnding := ""
	body := strings.TrimRight(message, "\r\n")
	lineEnding = message[len(body):]
	fuelPercent := int(math.Round(math.Max(0, math.Min(vehicleFuelMaximum, fuel))))
	fuelField := fmt.Sprintf("F:%d", fuelPercent)
	parts := strings.Split(body, ",")
	found := false
	for index, part := range parts {
		trimmed := strings.TrimSpace(part)
		if !strings.HasPrefix(trimmed, "F:") {
			continue
		}
		leading := part[:len(part)-len(strings.TrimLeft(part, " \t"))]
		trailing := part[len(strings.TrimRight(part, " \t")):]
		parts[index] = leading + fuelField + trailing
		found = true
	}
	if !found {
		parts = append(parts, fuelField)
	}
	return strings.Join(parts, ",") + lineEnding
}

func commandWithoutFuelPercent(message string) string {
	if _, _, ok := parseDriveCommand(message); !ok {
		return message
	}
	body := strings.TrimRight(message, "\r\n")
	lineEnding := message[len(body):]
	parts := strings.Split(body, ",")
	filtered := parts[:0]
	for _, part := range parts {
		if strings.HasPrefix(strings.TrimSpace(part), "F:") {
			continue
		}
		filtered = append(filtered, part)
	}
	return strings.Join(filtered, ",") + lineEnding
}

func (r *relay) supportsFuelCommand() bool {
	generation := r.upstreamGeneration.Load()
	return generation != 0 && r.fuelCommandGeneration.Load() == generation
}

func sendDataChannel(channel *webrtc.DataChannel, message webrtc.DataChannelMessage) error {
	if message.IsString {
		return channel.SendText(string(message.Data))
	}
	return channel.Send(message.Data)
}

func (r *relay) viewerCommandAllowed(client *viewer) bool {
	return effectiveRelaySourceKind(r.sourceKind) == relaySourceKindVehicle &&
		(client.role == "pilot" || r.allowObserverCommand)
}

func (r *relay) handleCommand(client *viewer, message webrtc.DataChannelMessage) {
	if !r.viewerCommandAllowed(client) {
		log.Printf("drop command from observer viewer %d", client.id)
		return
	}
	r.upstreamMu.RLock()
	upstream := r.upstreamDC
	r.upstreamMu.RUnlock()
	if upstream == nil {
		if shouldLogCommandDrop(client, time.Now()) {
			log.Printf("drop command from viewer %d: upstream DataChannel is unavailable", client.id)
		}
		return
	}
	forwarded := message
	now := time.Now()
	health := r.vehicleHealth.snapshot(now)
	if message.IsString {
		limited := r.vehicleHealth.limitCommand(string(message.Data), now)
		health = r.vehicleHealth.snapshot(now)
		if r.supportsFuelCommand() {
			limited = commandWithFuelPercent(limited, health.Fuel)
		} else {
			limited = commandWithoutFuelPercent(limited)
		}
		forwarded.Data = []byte(limited)
	}
	if err := sendDataChannel(upstream, forwarded); err != nil {
		log.Printf("forward command from viewer %d to Momo: %v", client.id, err)
		return
	}
	client.lastCommandUnixNano.Store(time.Now().UnixNano())
	// 表示用にはダメージ制限前のペダル入力を返す。車両へ送る forwarded は
	// 引き続き制限後の値なので、走行性能の制御には影響しない。
	r.driveGear.Store(int32(health.Gear))
	if message.IsString {
		r.observeBoostRegenDriveCommand(string(message.Data), health, now)
	}
	r.recordDriveInput(client.id, message, forwarded, health, now)
	r.broadcastCommand(commandAuditWithGear(message, int32(health.Gear)))
}

const driveInputLogInterval = 100 * time.Millisecond

func (r *relay) recordDriveInput(pilotID uint64, requested webrtc.DataChannelMessage, effective webrtc.DataChannelMessage, health vehicleHealthSnapshot, now time.Time) {
	if r.recorder == nil || !r.driveLoggingEnabled.Load() || !requested.IsString || !effective.IsString {
		return
	}
	r.driveInputLogMu.Lock()
	if !r.lastDriveInputLogAt.IsZero() && now.Sub(r.lastDriveInputLogAt) < driveInputLogInterval {
		r.driveInputLogMu.Unlock()
		return
	}
	r.lastDriveInputLogAt = now
	r.driveInputLogMu.Unlock()

	steeringPWM, requestedPowerPWM, ok := parseDriveCommand(string(requested.Data))
	if !ok {
		return
	}
	_, effectivePowerPWM, ok := parseDriveCommand(string(effective.Data))
	if !ok {
		return
	}
	throttle, brake := normalizeDrivePower(requestedPowerPWM, health.Gear)
	effectiveThrottle, effectiveBrake := normalizeDrivePower(effectivePowerPWM, health.Gear)
	outputLimitReasons := driveOutputLimitReasons(requestedPowerPWM, effectivePowerPWM, health)
	boostChargeEligible := health.BoostState == "charging" && health.Fuel > 0 && r.vehicleHealth.isActivelyDriving(now)
	courseProgress := r.courseProgress.snapshot()
	r.recorder.RecordDriveInput(r.name, r.raceCarID, pilotID, driveInputLogSample{
		SteeringPWM:         steeringPWM,
		Steering:            clampFloat(float64(steeringPWM-1500)/500, -1, 1),
		RequestedPowerPWM:   requestedPowerPWM,
		EffectivePowerPWM:   effectivePowerPWM,
		Throttle:            throttle,
		Brake:               brake,
		EffectiveThrottle:   effectiveThrottle,
		EffectiveBrake:      effectiveBrake,
		Gear:                health.Gear,
		DriveEnabled:        r.driveLoggingEnabled.Load(),
		HP:                  health.HP,
		SpeedCap:            health.SpeedCap,
		Fuel:                health.Fuel,
		Boost:               health.Boost,
		BoostState:          health.BoostState,
		BoostRemainingMS:    health.BoostRemainingMS,
		BoostChargeEligible: boostChargeEligible,
		BoostChargeMS:       health.BoostChargeMS,
		BoostPassiveScale:   health.BoostPassiveScale,
		Position:            health.Position,
		FieldSize:           health.FieldSize,
		RaceGapKnown:        health.RaceGapKnown,
		GapToAheadMS:        health.GapToAheadMS,
		LapDeltaToAhead:     health.LapDeltaToAhead,
		OutputLimited:       requestedPowerPWM != effectivePowerPWM,
		OutputLimitReasons:  outputLimitReasons,
		Lap:                 courseProgress.Lap,
		LastMarkerIndex:     courseProgress.LastMarkerIndex,
		FuelRatePerSecond:   health.FuelRatePerSec,
		FuelRateMultiplier:  health.FuelRateMultiplier,
		FuelPowerScale:      health.FuelPowerScale,
		FuelRoughMultiplier: health.FuelRoughMultiplier,
		FuelBoostMultiplier: health.FuelBoostMultiplier,
		ThrottleVariation:   health.ThrottleVariation,
		SessionType:         health.SessionType,
	})
}

func parseDriveCommand(message string) (int, int, bool) {
	steeringPWM := 0
	powerPWM := 0
	for _, field := range strings.Split(strings.TrimSpace(message), ",") {
		field = strings.TrimSpace(field)
		if strings.HasPrefix(field, "S:") {
			value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(field, "S:")))
			if err == nil {
				steeringPWM = value
			}
		}
		if strings.HasPrefix(field, "T:") {
			value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(field, "T:")))
			if err == nil {
				powerPWM = value
			}
		}
	}
	return steeringPWM, powerPWM, steeringPWM >= 1000 && steeringPWM <= 2000 && powerPWM >= 1000 && powerPWM <= 2000
}

func normalizeDrivePower(pwm int, gear int) (float64, float64) {
	if pwm >= 1500 {
		return normalizeForwardThrottle(pwm, gear), 0
	}
	minimum := vehicleGearBrakeMinimum(gear)
	return 0, clampFloat(float64(1500-pwm)/float64(1500-minimum), 0, 1)
}

func driveOutputLimitReasons(requestedPWM int, effectivePWM int, health vehicleHealthSnapshot) []string {
	if requestedPWM == effectivePWM {
		return nil
	}

	reasons := make([]string, 0, 3)
	expectedPWM := requestedPWM
	if requestedPWM > 1500 {
		gearLimitedPWM := minInt(expectedPWM, vehicleGearForwardMaximum(health.Gear))
		if gearLimitedPWM != expectedPWM {
			reasons = append(reasons, "gear_cap")
		}
		expectedPWM = gearLimitedPWM

		damageLimitedPWM := 1500 + int(math.Round(float64(expectedPWM-1500)*health.SpeedCap))
		if damageLimitedPWM != expectedPWM {
			reasons = append(reasons, "damage_cap")
		}
		expectedPWM = damageLimitedPWM

		fuelLimitedPWM := minInt(expectedPWM, vehicleFuelEmptyForwardPWM)
		if effectivePWM == fuelLimitedPWM && fuelLimitedPWM != expectedPWM {
			reasons = append(reasons, "fuel_empty")
			expectedPWM = fuelLimitedPWM
		}
	} else if requestedPWM < 1500 {
		fuelLimitedPWM := maxInt(requestedPWM, vehicleFuelEmptyReversePWM)
		if effectivePWM == fuelLimitedPWM && fuelLimitedPWM != requestedPWM {
			reasons = append(reasons, "fuel_empty")
			expectedPWM = fuelLimitedPWM
		}
	}

	if expectedPWM != effectivePWM || len(reasons) == 0 {
		reasons = append(reasons, "other")
	}
	return reasons
}

func clampFloat(value float64, minimum float64, maximum float64) float64 {
	return math.Max(minimum, math.Min(maximum, value))
}

func shouldLogCommandDrop(client *viewer, now time.Time) bool {
	if client == nil {
		return true
	}
	current := now.UnixNano()
	for {
		previous := client.lastCommandDropLog.Load()
		if previous != 0 && current-previous < commandDropLogInterval.Nanoseconds() {
			return false
		}
		if client.lastCommandDropLog.CompareAndSwap(previous, current) {
			return true
		}
	}
}

func (r *relay) handleDriveState(client *viewer, message webrtc.DataChannelMessage) {
	if client.role != "pilot" || !r.isCurrentPilot(client.id) {
		log.Printf("drop drive state from viewer %d (%s)", client.id, client.role)
		return
	}
	if !message.IsString {
		log.Printf("drop non-text drive state from viewer %d", client.id)
		return
	}

	text := strings.TrimSpace(string(message.Data))
	switch text {
	case "DRIVE:1":
		r.setDriveLogging(client.id, true, "viewer drive on")
	case "DRIVE:0":
		r.setDriveLogging(client.id, false, "viewer drive off")
	case "BOOST:ACTIVATE":
		if health, activated := r.vehicleHealth.activateBoost(time.Now()); activated {
			r.driveGear.Store(int32(health.Gear))
			r.broadcastVehicleGameplay(health)
		} else {
			log.Printf("drop unavailable boost activation from viewer %d", client.id)
		}
	default:
		if strings.HasPrefix(text, "GEAR:") {
			gear, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(text, "GEAR:")))
			if err == nil {
				if r.vehicleHealth == nil && gear >= 1 && gear <= vehicleNormalGearMaximum {
					r.driveGear.Store(int32(gear))
					return
				}
				if health, accepted := r.vehicleHealth.setRequestedGear(gear, time.Now()); accepted {
					r.driveGear.Store(int32(health.Gear))
					r.broadcastVehicleGameplay(health)
					return
				}
				log.Printf("drop unavailable gear %d from viewer %d", gear, client.id)
				return
			}
		}
		log.Printf("drop invalid drive state from viewer %d", client.id)
	}
}

func (r *relay) isCurrentPilot(id uint64) bool {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	return r.pilotID == id
}

func (r *relay) setDriveLogging(pilotID uint64, enabled bool, reason string) {
	r.driveStateMu.Lock()
	if enabled {
		ownerID := r.driveOwnerID.Load()
		if ownerID != 0 && ownerID != pilotID {
			r.driveStateMu.Unlock()
			log.Printf("drop drive on from viewer %d: current owner is %d", pilotID, ownerID)
			return
		}
		r.driveOwnerID.Store(pilotID)
		if r.driveLoggingEnabled.Load() {
			r.driveStateMu.Unlock()
			return
		}
		r.driveLoggingEnabled.Store(true)
	} else {
		ownerID := r.driveOwnerID.Load()
		if ownerID != 0 && ownerID != pilotID {
			r.driveStateMu.Unlock()
			return
		}
		if ownerID == pilotID {
			r.driveOwnerID.CompareAndSwap(pilotID, 0)
		}
		if !r.driveLoggingEnabled.Load() {
			r.driveStateMu.Unlock()
			return
		}
		r.driveLoggingEnabled.Store(false)
	}
	now := time.Now()
	r.driveRevision++
	if enabled {
		sessionID, err := newRelaySessionID(now)
		if err != nil {
			log.Printf("source %q: generate drive session ID: %v", r.name, err)
			sessionID = fmt.Sprintf("%s-%d", now.UTC().Format("20060102T150405.000000000Z"), r.driveRevision)
		}
		r.driveSessionID = "ds_" + sessionID
	}
	r.driveChangedAt = now.UTC()
	r.driveReason = reason
	r.driveStateMu.Unlock()
	r.boostRegen.reset()
	if r.recorder != nil {
		r.recorder.RecordDriveState(r.name, r.raceCarID, pilotID, enabled, reason)
	}
	r.broadcastVehicleGameplay(r.vehicleHealth.setDriveEnabled(enabled, now))
}

func (r *relay) driveStatusSnapshot() driveOperationsState {
	r.driveStateMu.RLock()
	defer r.driveStateMu.RUnlock()
	var changedAt *time.Time
	if !r.driveChangedAt.IsZero() {
		value := r.driveChangedAt
		changedAt = &value
	}
	return driveOperationsState{
		Enabled:       r.driveLoggingEnabled.Load(),
		Revision:      r.driveRevision,
		SessionID:     r.driveSessionID,
		ChangedAt:     changedAt,
		Reason:        r.driveReason,
		OwnerViewerID: r.driveOwnerID.Load(),
	}
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
	var removed *viewer
	r.viewersMu.Lock()
	removed = r.viewers[id]
	delete(r.viewers, id)
	if r.pilotID == id {
		r.pilotID = 0
		wasPilot = true
	}
	r.viewersMu.Unlock()
	if removed != nil && removed.raceAudioStop != nil {
		removed.raceAudioStopOnce.Do(func() { close(removed.raceAudioStop) })
	}
	if wasPilot {
		r.driveGear.Store(0)
		r.setDriveLogging(id, false, "pilot disconnected")
		r.sendNeutralToUpstream("pilot disconnect")
	}
}

var errAyamePeerLeft = errors.New("Ayame peer left")

func ayamePilotRetryDelay(err error) time.Duration {
	if errors.Is(err, errAyamePeerLeft) {
		return 0
	}
	return 3 * time.Second
}

// Ayame の room は source ごとに 1 つだけ割り当てる。LAN Pilot と Ayame Pilot は
// 同じ source の Pilot lease と neutral failsafe を共有する。
func (r *relay) startAyamePilot(ctx context.Context, signalingURL string, roomID string, clientID string, key string) {
	go func() {
		for {
			err := r.connectAyamePilot(ctx, signalingURL, roomID, clientID, key)
			if err != nil && !errors.Is(err, context.Canceled) {
				if errors.Is(err, errAyamePeerLeft) {
					log.Printf("source %q: Ayame pilot peer left; re-registering immediately", r.name)
				} else {
					log.Printf("source %q: Ayame pilot disconnected: %v; retrying in 3 seconds", r.name, err)
				}
			}
			delay := ayamePilotRetryDelay(err)
			if delay == 0 {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
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
	connectionDone := make(chan struct{})
	defer close(connectionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-connectionDone:
		}
	}()

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
		client = &viewer{id: r.nextID.Add(1), role: "pilot", remoteAddr: "ayame"}
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
		client.pc = pc
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
					r.setDriveLogging(client.id, false, "Ayame command channel closed")
					r.sendNeutralToUpstream("Ayame command channel closed")
				})
			case driveLabel:
				channel.OnOpen(func() {
					client.drive.Store(channel)
					log.Printf("source %q: Ayame pilot drive channel opened", r.name)
				})
				channel.OnMessage(func(message webrtc.DataChannelMessage) {
					r.handleDriveState(client, message)
				})
				channel.OnClose(func() {
					client.drive.CompareAndSwap(channel, nil)
					r.driveGear.Store(0)
					r.setDriveLogging(client.id, false, "Ayame drive channel closed")
				})
			case telemetryLabel:
				channel.OnOpen(func() {
					client.telemetry.Store(channel)
					r.sendCurrentGameplayState(client, channel)
				})
				channel.OnClose(func() { client.telemetry.CompareAndSwap(channel, nil) })
			case raceLabel:
				channel.OnOpen(func() {
					client.race.Store(channel)
					r.sendCurrentRaceState(client, channel)
				})
				channel.OnClose(func() { client.race.CompareAndSwap(channel, nil) })
			case raceAudioLabel:
				r.handleRaceAudioChannel(client, channel)
			case eventsLabel:
				channel.OnOpen(func() {
					r.openVehicleEventsChannel(client, channel)
					log.Printf("source %q: Ayame pilot events channel opened", r.name)
				})
				channel.OnClose(func() { client.events.CompareAndSwap(channel, nil) })
			default:
				log.Printf("source %q: Ayame pilot opened unsupported DataChannel %q", r.name, channel.Label())
			}
		})
		rtpSender, addTrackErr := pc.AddTrack(r.videoTrack)
		if addTrackErr != nil {
			_ = pc.Close()
			r.removeViewer(client.id)
			pc = nil
			return fmt.Errorf("add Ayame video track: %w", addTrackErr)
		}
		go r.readDownstreamRTCP(rtpSender)
		if err := r.configureRaceAudioPeer(client, pc); err != nil {
			_ = pc.Close()
			r.removeViewer(client.id)
			pc = nil
			return err
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
		case "bye":
			return fmt.Errorf("%w: %s", errAyamePeerLeft, firstNonEmpty(message.Reason, message.Error))
		case "reject":
			return fmt.Errorf("Ayame reject: %s", firstNonEmpty(message.Reason, message.Error))
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
	if device == "" {
		if source, ok := server.onlySource(); ok {
			device = source.name
		}
	}
	source, ok := server.acquireSourceSession(device)
	if !ok {
		if device == "" {
			http.Error(w, "device is required when multiple Momo sources are configured", http.StatusBadRequest)
			return
		}
		http.Error(w, "unknown device: "+device, http.StatusNotFound)
		return
	}
	defer source.activeSessions.Add(-1)
	source.serveViewerWS(w, req)
}

func (r *relay) serveViewerWS(w http.ResponseWriter, req *http.Request) {
	role := req.URL.Query().Get("role")
	if role != "pilot" {
		role = "observer"
	}
	if role == "pilot" && effectiveRelaySourceKind(r.sourceKind) != relaySourceKindVehicle {
		http.Error(w, "pilot connections are not available for source kind "+effectiveRelaySourceKind(r.sourceKind), http.StatusForbidden)
		return
	}
	clientKind := req.URL.Query().Get("client")
	if clientKind != "web-observer" && clientKind != "web-pilot" {
		clientKind = ""
	}
	client := &viewer{id: r.nextID.Add(1), role: role, clientKind: clientKind, remoteAddr: req.RemoteAddr}
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
	viewerDataDone := make(chan struct{})
	defer close(viewerDataDone)
	if role == "pilot" || clientKind == "web-observer" {
		client.telemetryWS = make(chan string, 1)
		client.telemetryStateWS = newSourceLatestTelemetryQueue()
		client.gameplayWS = make(chan string, gameplayWebSocketQueueSize)
		client.eventsWS = make(chan string, 64)
	}
	if clientKind == "web-pilot" {
		client.raceWS = make(chan string, 1)
	}
	if clientKind == "web-observer" {
		client.commandWS = make(chan string, 1)
	}
	if role == "pilot" {
		client.audioWS = make(chan string, 8)
	}
	if role == "pilot" || clientKind == "web-observer" {
		go func() {
			for {
				select {
				case payload := <-client.raceWS:
					if err := sendSignal(signalMessage{Type: "race-state", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket race state to viewer %d: %v", r.name, client.id, err)
						return
					}
					continue
				default:
				}
				select {
				case payload := <-client.eventsWS:
					if err := sendSignal(signalMessage{Type: "vehicle-event", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket vehicle event to viewer %d: %v", r.name, client.id, err)
						return
					}
					continue
				default:
				}
				select {
				case payload := <-client.gameplayWS:
					if err := sendSignal(signalMessage{Type: "telemetry", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket gameplay to viewer %d: %v", r.name, client.id, err)
						return
					}
					continue
				default:
				}
				select {
				case payload := <-client.eventsWS:
					if err := sendSignal(signalMessage{Type: "vehicle-event", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket vehicle event to viewer %d: %v", r.name, client.id, err)
						return
					}
				case payload := <-client.gameplayWS:
					if err := sendSignal(signalMessage{Type: "telemetry", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket gameplay to viewer %d: %v", r.name, client.id, err)
						return
					}
				case payload := <-client.telemetryWS:
					if err := sendSignal(signalMessage{Type: "telemetry", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket telemetry to viewer %d: %v", r.name, client.id, err)
						return
					}
				case <-client.telemetryStateWS.Ready():
					payload, ok := client.telemetryStateWS.Dequeue()
					if !ok {
						continue
					}
					if err := sendSignal(signalMessage{Type: "telemetry", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket telemetry state to viewer %d: %v", r.name, client.id, err)
						return
					}
				case payload := <-client.commandWS:
					if err := sendSignal(signalMessage{Type: "command", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket command audit to viewer %d: %v", r.name, client.id, err)
						return
					}
				case payload := <-client.raceWS:
					if err := sendSignal(signalMessage{Type: "race-state", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket race state to viewer %d: %v", r.name, client.id, err)
						return
					}
				case payload := <-client.audioWS:
					if err := sendSignal(signalMessage{Type: "m5-audio", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket M5 audio to viewer %d: %v", r.name, client.id, err)
						return
					}
				case <-viewerDataDone:
					return
				}
			}
		}()
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
				log.Printf("source %q: viewer %d (%s remote=%s) telemetry channel opened", r.name, client.id, client.role, client.remoteAddr)
				r.sendCurrentGameplayState(client, channel)
			})
			channel.OnClose(func() {
				client.telemetry.CompareAndSwap(channel, nil)
				log.Printf("source %q: viewer %d (%s remote=%s) telemetry channel closed", r.name, client.id, client.role, client.remoteAddr)
			})
		case commandLabel:
			channel.OnOpen(func() {
				client.command.Store(channel)
				log.Printf("viewer %d command channel opened", client.id)
			})
			channel.OnMessage(func(message webrtc.DataChannelMessage) {
				r.handleCommand(client, message)
			})
			channel.OnClose(func() {
				client.command.CompareAndSwap(channel, nil)
				r.setDriveLogging(client.id, false, "command channel closed")
			})
		case driveLabel:
			channel.OnOpen(func() {
				client.drive.Store(channel)
				log.Printf("viewer %d drive channel opened", client.id)
			})
			channel.OnMessage(func(message webrtc.DataChannelMessage) {
				r.handleDriveState(client, message)
			})
			channel.OnClose(func() {
				client.drive.CompareAndSwap(channel, nil)
				r.driveGear.Store(0)
				r.setDriveLogging(client.id, false, "drive channel closed")
			})
		case raceLabel:
			channel.OnOpen(func() {
				client.race.Store(channel)
				log.Printf("viewer %d race channel opened", client.id)
				r.sendCurrentRaceState(client, channel)
			})
			channel.OnClose(func() { client.race.CompareAndSwap(channel, nil) })
		case raceAudioLabel:
			r.handleRaceAudioChannel(client, channel)
		case eventsLabel:
			channel.OnOpen(func() {
				r.openVehicleEventsChannel(client, channel)
				log.Printf("viewer %d events channel opened", client.id)
			})
			channel.OnClose(func() { client.events.CompareAndSwap(channel, nil) })
		default:
			log.Printf("viewer %d opened unsupported DataChannel %q", client.id, channel.Label())
		}
	})

	rtpSender, err := pc.AddTrack(r.videoTrack)
	if err != nil {
		_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
		return
	}
	go r.readDownstreamRTCP(rtpSender)
	if err := r.configureRaceAudioPeer(client, pc); err != nil {
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
			if clientKind == "web-observer" || clientKind == "web-pilot" {
				if err := r.sendInitialWebDownlinkState(sendSignal); err != nil {
					log.Printf("source %q: send initial web downlink state: %v", r.name, err)
					return
				}
			}
			if client.raceWS != nil {
				if state := r.currentRaceState(); state != "" {
					enqueueLatestTelemetry(client.raceWS, state)
				}
			}
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
		case "m5-audio-subscription":
			if client.role == "pilot" {
				enabled := message.Data == "1"
				client.audioSubscribed.Store(enabled)
				log.Printf("source %q: viewer %d M5 audio subscription=%t", r.name, client.id, enabled)
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

// unwrapRaceStateMessage accepts both the Race Control raw race_state v2
// payload and the LAN Relay /ws/race-state wrapper. The latter lets a local
// measurement Relay follow the site Relay without copying its Viewer token.
func unwrapRaceStateMessage(data []byte) ([]byte, bool, error) {
	data = bytes.TrimSpace(data)
	var message struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return nil, false, err
	}
	switch message.Type {
	case "race-state":
		payload := strings.TrimSpace(strings.TrimPrefix(message.Data, "RACE:"))
		if payload == "" {
			return nil, false, errors.New("relay race-state message has no data")
		}
		return []byte(payload), true, nil
	case "race-heartbeat":
		return nil, false, nil
	default:
		return data, true, nil
	}
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
	defer server.markRaceControlDisconnected()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Race Control WebSocket: %w", err)
		}
		payload, present, err := unwrapRaceStateMessage(data)
		if err != nil {
			log.Printf("ignore malformed Race Control message: %v", err)
			continue
		}
		if !present {
			continue
		}
		var envelope raceStateEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			log.Printf("ignore malformed Race Control state: %v", err)
			continue
		}
		if envelope.Type != "race_state" || envelope.Version != 2 {
			log.Printf("ignore unsupported Race Control message: type=%q version=%d", envelope.Type, envelope.Version)
			continue
		}
		server.publishGlobalRaceState("RACE:" + string(payload))
		if server.recorder != nil {
			server.recorder.RecordRaceState(string(payload), telemetryRaceContext{
				RaceID:    envelope.RaceID,
				RaceRunID: envelope.RaceRunID,
				Phase:     envelope.Phase,
				Flag:      envelope.Flag,
				Sequence:  envelope.Sequence,
				Present:   true,
			})
		}
		server.observeRaceContext(envelope, time.Now())
		for _, source := range server.sourceSnapshot() {
			if effectiveRelaySourceKind(source.sourceKind) != relaySourceKindVehicle {
				continue
			}
			message, err := raceMessageForCar(payload, source.raceCarID)
			if err != nil {
				log.Printf("source %q: ignore Race Control state: %v", source.name, err)
				continue
			}
			source.publishRaceState(message)
		}
	}
}

func main() {
	var configPath string
	var sourceRegistryPath string
	var upstream string
	var listen string
	var allowObserverCommand bool
	var rtpStallTimeout time.Duration
	var upstreamStartTimeout time.Duration
	var sources sourceFlag
	var raceCars sourceFlag
	var operationsAllowCIDRs sourceFlag
	var sourceAdminAllowCIDRs sourceFlag
	var garageAllowCIDRs sourceFlag
	var gameplayAllowCIDRs sourceFlag
	var raceURL string
	var raceViewerToken string
	var raceAudioServiceURL string
	var raceAudioLanguageValue string
	var raceAudioEnglishVoice string
	var raceAudioJapaneseVoice string
	var raceAudioSpeed float64
	var raceAudioBrowserKokoro bool
	var ayameSignalingURL string
	var ayameClientIDPrefix string
	var ayameSignalingKey string
	var ayameRoomPrefix string
	var ayamePilotRooms sourceFlag
	var telemetryLogDir string
	var telemetryLogRetention time.Duration
	var healthRecoveryModeValue string
	var vehicleDamageEnabled bool
	var fuelDriveDuration time.Duration
	var teamObserverDirectoryCache string
	var teamObserverDirectoryOrganization string
	var teamObserverDirectoryEvent string
	var teamObserverDirectoryMaxAge time.Duration
	var configuredDefinitions []relayFileSource
	flag.StringVar(&configPath, "config", "", "JSON source configuration file; cannot be combined with -upstream, -source, -race-car, or -ayame-pilot-room")
	flag.StringVar(&sourceRegistryPath, "source-registry", strings.TrimSpace(os.Getenv("MOMO_RELAY_SOURCE_REGISTRY")), "Relay-owned JSON registry for dynamically managed sources")
	flag.StringVar(&upstream, "upstream", "", "Momo P2P WebSocket URL, for example ws://192.168.11.3:8080/ws")
	flag.Var(&sources, "source", "Momo source as DEVICE=WS_URL; can be repeated")
	flag.Var(&raceCars, "race-car", "Race Control car mapping as DEVICE=CAR_ID; can be repeated")
	flag.Var(&operationsAllowCIDRs, "operations-allow-cidr", "CIDR allowed to read /operations.html and /api/v1/status; can be repeated (default: loopback only)")
	flag.Var(&sourceAdminAllowCIDRs, "source-admin-allow-cidr", "CIDR allowed to manage dynamic Relay sources; Bearer token is also required (default: loopback only)")
	flag.Var(&garageAllowCIDRs, "garage-allow-cidr", "CIDR allowed to read /garage.html and /api/v1/pilot-devices; can be repeated (default: loopback only)")
	flag.Var(&gameplayAllowCIDRs, "gameplay-allow-cidr", "CIDR allowed to call gameplay APIs; can be repeated (default: loopback only)")
	flag.StringVar(&listen, "listen", ":8090", "HTTP and WebSocket listen address")
	flag.StringVar(&raceURL, "race-url", strings.TrimSpace(os.Getenv("MOMO_RACE_CONTROL_WS_URL")), "Race Control WebSocket URL for race_state v2 distribution")
	flag.StringVar(&raceViewerToken, "race-viewer-token", strings.TrimSpace(os.Getenv("MOMO_RACE_CONTROL_VIEWER_TOKEN")), "Race Control Viewer Bearer token")
	flag.StringVar(&raceAudioServiceURL, "race-audio-service-url", strings.TrimSpace(os.Getenv("MOMO_RACE_AUDIO_SERVICE_URL")), "internal race audio synthesis service URL")
	flag.StringVar(&raceAudioLanguageValue, "race-audio-default-language", raceAudioDefaultLanguage, "race audio language before the Pilot preference arrives: en-US or ja-JP")
	flag.StringVar(&raceAudioEnglishVoice, "race-audio-en-voice", raceAudioDefaultEnglishVoice, "English voice name sent to the race audio service")
	flag.StringVar(&raceAudioJapaneseVoice, "race-audio-ja-voice", raceAudioDefaultJapaneseVoice, "Japanese voice name sent to the race audio service")
	flag.Float64Var(&raceAudioSpeed, "race-audio-speed", 1.04, "race audio speech speed from 0.5 to 2.0")
	flag.BoolVar(&raceAudioBrowserKokoro, "race-audio-browser-kokoro", true, "advertise Browser Kokoro synthesis to Pilot viewers")
	flag.StringVar(&ayameSignalingURL, "ayame-signaling-url", "", "Ayame signaling WebSocket URL for external pilot distribution")
	flag.StringVar(&ayameClientIDPrefix, "ayame-client-id-prefix", "momo-relay", "Ayame client ID prefix; source name is appended")
	flag.StringVar(&ayameSignalingKey, "ayame-signaling-key", strings.TrimSpace(os.Getenv("MOMO_AYAME_SIGNALING_KEY")), "Ayame backend signaling key for external pilot distribution; prefer MOMO_AYAME_SIGNALING_KEY")
	flag.StringVar(&ayameRoomPrefix, "ayame-room-prefix", strings.TrimSpace(os.Getenv("MOMO_AYAME_ROOM_PREFIX")), "generate one unique Ayame Pilot room for every source that does not opt out")
	flag.Var(&ayamePilotRooms, "ayame-pilot-room", "Ayame external pilot room as DEVICE=ROOM_ID; can be repeated")
	flag.StringVar(&telemetryLogDir, "telemetry-log-dir", "", "directory for Relay-local interleaved telemetry NDJSON logs (disabled when empty)")
	flag.DurationVar(&telemetryLogRetention, "telemetry-log-retention", defaultTelemetryLogRetention, "retain telemetry NDJSON logs for this duration; clean every 2h while race is idle (0 disables cleanup)")
	flag.StringVar(&healthRecoveryModeValue, "health-recovery-mode", strings.TrimSpace(os.Getenv("MOMO_RELAY_HEALTH_RECOVERY_MODE")), "vehicle HP recovery mode: legacy, pit-marker, hybrid, or disabled")
	flag.BoolVar(&vehicleDamageEnabled, "vehicle-damage-enabled", true, "apply confirmed vehicle impacts to HP and forward speed limits")
	flag.DurationVar(&fuelDriveDuration, "fuel-drive-duration", vehicleFuelDefaultDriveDuration, "active forward-driving time required to consume a full fuel tank")
	flag.StringVar(&teamObserverDirectoryCache, "team-observer-directory-cache", strings.TrimSpace(os.Getenv("MOMO_TEAM_OBSERVER_DIRECTORY_CACHE")), "validated Race Directory cache used for the read-only Team Observer projection")
	flag.StringVar(&teamObserverDirectoryOrganization, "team-observer-directory-organization", strings.TrimSpace(os.Getenv("MOMO_TEAM_OBSERVER_DIRECTORY_ORGANIZATION")), "expected organization slug for the Team Observer directory cache")
	flag.StringVar(&teamObserverDirectoryEvent, "team-observer-directory-event", strings.TrimSpace(os.Getenv("MOMO_TEAM_OBSERVER_DIRECTORY_EVENT")), "expected event slug for the Team Observer directory cache")
	flag.DurationVar(&teamObserverDirectoryMaxAge, "team-observer-directory-max-age", time.Hour, "age after which the Team Observer directory projection is marked stale")
	flag.BoolVar(&allowObserverCommand, "allow-observer-command", false, "allow observer viewers to send commands to Momo")
	flag.DurationVar(&rtpStallTimeout, "rtp-stall-timeout", defaultRTPStallTimeout, "reconnect a source when received RTP stops for this duration")
	flag.DurationVar(&upstreamStartTimeout, "upstream-start-timeout", defaultUpstreamStartTimeout, "reconnect a source when no RTP arrives after connection")
	flag.Parse()
	if strings.TrimSpace(configPath) != "" {
		if strings.TrimSpace(upstream) != "" || len(sources) > 0 || len(raceCars) > 0 || len(ayamePilotRooms) > 0 {
			log.Fatal("-config cannot be combined with -upstream, -source, -race-car, or -ayame-pilot-room")
		}
		mappings, err := loadRelayConfig(strings.TrimSpace(configPath))
		if err != nil {
			log.Fatal(err)
		}
		sources = mappings.Sources
		raceCars = mappings.RaceCars
		ayamePilotRooms = mappings.AyamePilotRooms
		configuredDefinitions = mappings.Definitions
	}
	if rtpStallTimeout <= 0 || upstreamStartTimeout <= 0 || fuelDriveDuration <= 0 || telemetryLogRetention < 0 {
		log.Fatal("-rtp-stall-timeout, -upstream-start-timeout, and -fuel-drive-duration must be positive; -telemetry-log-retention must not be negative")
	}
	teamObserverDirectory, err := newTeamObserverDirectorySource(
		teamObserverDirectoryCache,
		teamObserverDirectoryOrganization,
		teamObserverDirectoryEvent,
		teamObserverDirectoryMaxAge,
	)
	if err != nil {
		log.Fatal(err)
	}
	if strings.TrimSpace(healthRecoveryModeValue) == "" {
		healthRecoveryModeValue = string(vehicleHealthRecoveryDefault)
	}
	healthRecoveryMode, err := parseVehicleHealthRecoveryMode(healthRecoveryModeValue)
	if err != nil {
		log.Fatal(err)
	}
	gameplayToken := strings.TrimSpace(os.Getenv("MOMO_RELAY_GAMEPLAY_TOKEN"))
	sourceAdminToken := strings.TrimSpace(os.Getenv("MOMO_RELAY_ADMIN_TOKEN"))
	raceAudioService, err := newRaceAudioServiceClient(
		raceAudioServiceURL,
		strings.TrimSpace(os.Getenv("MOMO_RACE_AUDIO_SERVICE_TOKEN")),
		raceAudioLanguageValue,
		raceAudioEnglishVoice,
		raceAudioJapaneseVoice,
		raceAudioSpeed,
		raceAudioBrowserKokoro,
	)
	if err != nil {
		log.Fatal(err)
	}
	if healthRecoveryMode.allowsPitRecovery() {
		if gameplayToken == "" {
			log.Fatalf("MOMO_RELAY_GAMEPLAY_TOKEN is required when -health-recovery-mode=%s", healthRecoveryMode)
		}
		if strings.TrimSpace(raceURL) == "" {
			log.Fatalf("-race-url is required when -health-recovery-mode=%s", healthRecoveryMode)
		}
	}
	if upstream != "" {
		sources = append(sources, "default="+upstream)
	}
	if len(sources) == 0 && strings.TrimSpace(sourceRegistryPath) == "" {
		log.Fatal("-upstream or at least one -source is required")
	}
	if strings.TrimSpace(sourceRegistryPath) != "" && sourceAdminToken == "" {
		log.Fatal("MOMO_RELAY_ADMIN_TOKEN is required when -source-registry is set")
	}
	operationsPolicy, err := parseOperationsAccessPolicy(operationsAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	garagePolicy, err := parseOperationsAccessPolicy(garageAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	gameplayPolicy, err := parseOperationsAccessPolicy(gameplayAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raceCarBySource := make(map[string]string, len(raceCars))
	raceCarSources := make(map[string]string, len(raceCars))
	for _, raceCarValue := range raceCars {
		name, carID, err := parseSource(raceCarValue)
		if err != nil {
			log.Fatalf("invalid -race-car: %v", err)
		}
		if _, exists := raceCarBySource[name]; exists {
			log.Fatalf("duplicate Race Control source mapping: %q", name)
		}
		if existingSource, exists := raceCarSources[carID]; exists {
			log.Fatalf("duplicate Race Control car mapping %q for sources %q and %q", carID, existingSource, name)
		}
		raceCarBySource[name] = carID
		raceCarSources[carID] = name
	}
	ayameRoomBySource := make(map[string]string, len(ayamePilotRooms))
	ayameRoomSources := make(map[string]string, len(ayamePilotRooms))
	for _, ayameRoomValue := range ayamePilotRooms {
		name, roomID, err := parseSource(ayameRoomValue)
		if err != nil {
			log.Fatalf("invalid -ayame-pilot-room: %v", err)
		}
		if _, exists := ayameRoomBySource[name]; exists {
			log.Fatalf("duplicate Ayame source mapping: %q", name)
		}
		if existingSource, exists := ayameRoomSources[roomID]; exists {
			log.Fatalf("duplicate Ayame room %q for sources %q and %q", roomID, existingSource, name)
		}
		ayameRoomBySource[name] = roomID
		ayameRoomSources[roomID] = name
	}
	sourceAdminPolicy, err := parseOperationsAccessPolicy(sourceAdminAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	if (len(ayameRoomBySource) > 0 || strings.TrimSpace(ayameRoomPrefix) != "") && ayameSignalingURL == "" {
		log.Fatal("-ayame-signaling-url is required when Ayame Pilot rooms are enabled")
	}
	dynamicRegistry, dynamicDefinitions, err := loadDynamicSourceRegistry(sourceRegistryPath)
	if err != nil {
		log.Fatal(err)
	}
	var recorder *telemetryRecorder
	if strings.TrimSpace(telemetryLogDir) != "" {
		recorder, err = newTelemetryRecorder(strings.TrimSpace(telemetryLogDir))
		if err != nil {
			log.Fatal(err)
		}
		defer func() {
			if err := recorder.Close(); err != nil {
				log.Printf("close telemetry recorder: %v", err)
			}
			stats := recorder.Stats()
			log.Printf("telemetry recorder stopped: path=%s telemetry=%d raceState=%d driveState=%d driveInput=%d courseMarkers=%d boostRegenProbes=%d vehicleEvents=%d queueDrops=%d writeErrors=%d",
				recorder.Path(), stats.TelemetryRecords, stats.RaceStateRecords, stats.DriveStateRecords, stats.DriveInputRecords, stats.CourseMarkerRecords, stats.BoostRegenProbeRecords, stats.VehicleEventRecords, stats.QueueDrops, stats.WriteErrors)
		}()
		log.Printf("telemetry recorder started: path=%s", recorder.Path())
	}
	if configuredDefinitions == nil {
		configuredDefinitions = make([]relayFileSource, 0, len(sources))
		configuredSourceIDs := make(map[string]struct{}, len(sources))
		for _, sourceValue := range sources {
			name, sourceURL, err := parseSource(sourceValue)
			if err != nil {
				log.Fatal(err)
			}
			if _, exists := configuredSourceIDs[name]; exists {
				log.Fatalf("duplicate source name: %q", name)
			}
			configuredSourceIDs[name] = struct{}{}
			configuredDefinitions = append(configuredDefinitions, relayFileSource{
				ID:             name,
				URL:            sourceURL,
				SourceKind:     relaySourceKindVehicle,
				DisplayName:    name,
				RaceCarID:      raceCarBySource[name],
				AyamePilotRoom: ayameRoomBySource[name],
			})
		}
		for name := range raceCarBySource {
			if _, exists := configuredSourceIDs[name]; !exists {
				log.Fatalf("Race Control source %q is not configured by -source", name)
			}
		}
		for name := range ayameRoomBySource {
			if _, exists := configuredSourceIDs[name]; !exists {
				log.Fatalf("Ayame source %q is not configured by -source", name)
			}
		}
	}
	totalSourceCapacity := len(configuredDefinitions) + len(dynamicDefinitions)
	serverRelay := &relayServer{
		sources:               make(map[string]*relay, totalSourceCapacity),
		sourceOrder:           make([]string, 0, totalSourceCapacity),
		managedSources:        make(map[string]*managedRelaySource, totalSourceCapacity),
		dynamicSourceRegistry: dynamicRegistry,
		recorder:              recorder,
		pitEvents:             make(map[string]pitPresenceReceipt),
		teamObserverDirectory: teamObserverDirectory,
		sourceRuntime: relaySourceRuntime{
			rootContext:          ctx,
			allowObserverCommand: allowObserverCommand,
			rtpStallTimeout:      rtpStallTimeout,
			upstreamStartTimeout: upstreamStartTimeout,
			healthRecoveryMode:   healthRecoveryMode,
			vehicleDamageEnabled: vehicleDamageEnabled,
			fuelDriveDuration:    fuelDriveDuration,
			raceAudioService:     raceAudioService,
			ayameSignalingURL:    strings.TrimSpace(ayameSignalingURL),
			ayameClientIDPrefix:  strings.TrimSpace(ayameClientIDPrefix),
			ayameSignalingKey:    strings.TrimSpace(ayameSignalingKey),
			ayameRoomPrefix:      strings.TrimSpace(ayameRoomPrefix),
		},
	}
	for _, definition := range configuredDefinitions {
		if raceURL != "" && effectiveRelaySourceKind(definition.SourceKind) == relaySourceKindVehicle && strings.TrimSpace(definition.RaceCarID) == "" {
			log.Fatalf("Race Control is enabled but static source %q has no raceCarId mapping", definition.ID)
		}
		if err := serverRelay.addInitialSource(definition, false); err != nil {
			log.Fatal(err)
		}
	}
	for _, definition := range dynamicDefinitions {
		if err := serverRelay.addInitialSource(definition, true); err != nil {
			log.Fatal(err)
		}
	}
	serverRelay.startRaceControl(ctx, raceURL, raceViewerToken)
	serverRelay.startTelemetryLogRetention(ctx, strings.TrimSpace(telemetryLogDir), telemetryLogRetention)

	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatal(err)
	}
	operationsHTML, err := fs.ReadFile(webRoot, "operations.html")
	if err != nil {
		log.Fatal(err)
	}
	garageHTML, err := fs.ReadFile(webRoot, "garage.html")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", operationsPolicy.wrap(serverRelay.serveOperationsStatus))
	mux.HandleFunc("/api/v1/coordinator-directory-cache", operationsPolicy.wrap(serverRelay.serveCoordinatorDirectoryCache))
	mux.HandleFunc("/api/v1/sources", sourceAdminPolicy.wrap(sourceAdminTokenHandler(sourceAdminToken, serverRelay.serveSources)))
	mux.HandleFunc("/api/v1/sources/", sourceAdminPolicy.wrap(sourceAdminTokenHandler(sourceAdminToken, serverRelay.serveSourceByID)))
	mux.HandleFunc("/api/v1/race-state", serverRelay.serveRaceState)
	mux.HandleFunc("/ws/race-state", serverRelay.serveRaceStateWS)
	mux.HandleFunc("/operations.html", operationsPolicy.wrap(operationsPageHandler(operationsHTML)))
	mux.HandleFunc("/api/v1/pilot-devices", garagePolicy.wrap(serverRelay.servePilotDevices))
	mux.HandleFunc("/api/v1/team-observer-directory", garagePolicy.wrap(serverRelay.serveTeamObserverDirectory))
	mux.HandleFunc("/api/v1/gameplay/status",
		gameplayPolicy.wrap(bearerTokenHandler(gameplayToken, serveGameplayStatus)))
	mux.HandleFunc("/api/v1/gameplay/pit-recovery-ticks",
		gameplayPolicy.wrap(bearerTokenHandler(gameplayToken, serverRelay.servePitRecoveryTick)))
	mux.HandleFunc("/api/v1/gameplay/pit-presence-events",
		gameplayPolicy.wrap(bearerTokenHandler(gameplayToken, serverRelay.servePitPresenceEvent)))
	mux.HandleFunc("/api/v1/race-audio/announcements",
		gameplayPolicy.wrap(bearerTokenHandler(gameplayToken, serverRelay.serveRaceAudioAnnouncement)))
	mux.HandleFunc("/garage.html", garagePolicy.wrap(operationsPageHandler(garageHTML)))
	mux.Handle("/", webAssetHandler(webRoot))
	mux.HandleFunc("/pilot", func(w http.ResponseWriter, req *http.Request) {
		target := "/pilot.html"
		if req.URL.RawQuery != "" {
			target += "?" + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/ws", serverRelay.serveViewerWS)
	server := &http.Server{Addr: listen, Handler: mux}
	activeSourceIDs := make([]string, 0, totalSourceCapacity)
	for _, source := range serverRelay.sourceSnapshot() {
		activeSourceIDs = append(activeSourceIDs, source.name)
	}
	log.Printf("Momo relay is listening on http://%s/ for sources: %s", listen, strings.Join(activeSourceIDs, ", "))
	log.Fatal(server.ListenAndServe())
}
