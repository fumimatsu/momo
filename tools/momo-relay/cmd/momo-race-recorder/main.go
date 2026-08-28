package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shiguredo/momo-local-relay/internal/racerecorder"
)

func main() {
	var listen string
	var relayWebSocketURL string
	var storageRoot string
	var minimumFreeGiB int64
	var maximumSources int
	var startTimeout time.Duration
	var segmentDuration time.Duration
	flag.StringVar(&listen, "listen", envOrDefault("MOMO_RACE_RECORDER_LISTEN", "127.0.0.1:8792"), "Recorder API listen address")
	flag.StringVar(&relayWebSocketURL, "relay-ws", envOrDefault("MOMO_RACE_RECORDER_RELAY_WS", "ws://127.0.0.1:8090/ws"), "Relay viewer WebSocket URL")
	flag.StringVar(&storageRoot, "storage-root", envOrDefault("MOMO_RACE_RECORDER_STORAGE_ROOT", "recordings"), "recording storage root")
	flag.Int64Var(&minimumFreeGiB, "minimum-free-gib", 10, "minimum free storage reserve required before a run")
	flag.IntVar(&maximumSources, "maximum-sources", 64, "maximum sources accepted in a full_archive request")
	flag.DurationVar(&startTimeout, "start-timeout", 4*time.Second, "maximum wait for every source to write an IDR frame")
	flag.DurationVar(&segmentDuration, "segment-duration", 2*time.Minute, "target raw H.264 segment duration; rotation waits for an IDR")
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("MOMO_RACE_RECORDER_TOKEN"))
	server, err := racerecorder.NewServer(racerecorder.Config{
		RelayWebSocketURL: relayWebSocketURL,
		StorageRoot:       storageRoot,
		Token:             token,
		MinimumFreeBytes:  minimumFreeGiB * 1024 * 1024 * 1024,
		MaximumSources:    maximumSources,
		StartTimeout:      startTimeout,
		SegmentDuration:   segmentDuration,
	})
	if err != nil {
		log.Fatalf("configure Recorder: %v", err)
	}
	defer server.Close()

	httpServer := &http.Server{
		Addr: listen, Handler: server.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("shutdown Recorder HTTP server: %v", err)
		}
	}()
	log.Printf("Momo Race Recorder listening on http://%s; relay=%s storage=%s", listen, relayWebSocketURL, storageRoot)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("serve Recorder API: %w", err))
	}
}

func envOrDefault(name string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
