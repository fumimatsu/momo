package racerecorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type runManifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	RaceID        string           `json:"raceId"`
	RaceRunID     string           `json:"raceRunId"`
	HeatID        string           `json:"heatId,omitempty"`
	Mode          string           `json:"mode"`
	State         string           `json:"state"`
	StartedAt     string           `json:"startedAt"`
	EndedAt       string           `json:"endedAt,omitempty"`
	Error         string           `json:"error,omitempty"`
	Sources       []sourceManifest `json:"sources"`
}

var errRunDirectoryExists = errors.New("recording directory already exists")

type runSession struct {
	request      StartRequest
	directory    string
	startedAt    time.Time
	captures     []*sourceCapture
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	errors       chan error
	stopOnce     sync.Once
	manifestMu   sync.Mutex
	manifestFile sync.Mutex
	manifest     runManifest
	onSourceFail func(error)
}

func newRunSession(parent context.Context, request StartRequest, storageRoot string, relayWebSocketURL string, segmentDuration time.Duration, onSourceFail func(error)) (*runSession, error) {
	directory := filepath.Join(storageRoot, request.RaceRunID)
	if err := os.Mkdir(directory, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w for raceRunId %s", errRunDirectoryExists, request.RaceRunID)
		}
		return nil, fmt.Errorf("create recording directory: %w", err)
	}
	startedAt := time.Now().UTC()
	ctx, cancel := context.WithCancel(parent)
	session := &runSession{
		request: request, directory: directory, startedAt: startedAt, ctx: ctx, cancel: cancel,
		errors: make(chan error, len(request.Sources)), onSourceFail: onSourceFail,
		manifest: runManifest{
			SchemaVersion: SchemaVersion, RaceID: request.RaceID, RaceRunID: request.RaceRunID,
			HeatID: request.HeatID, Mode: request.Mode, State: "starting", StartedAt: startedAt.Format(time.RFC3339Nano),
		},
	}
	for _, source := range request.Sources {
		capture, err := newSourceCapture(source, relayWebSocketURL, directory, startedAt, segmentDuration)
		if err != nil {
			cancel()
			_ = session.closeCaptures("degraded", err.Error())
			_ = archiveFailedRunDirectory(storageRoot, request.RaceRunID)
			return nil, err
		}
		session.captures = append(session.captures, capture)
	}
	if err := session.writeManifest(); err != nil {
		cancel()
		_ = session.closeCaptures("degraded", err.Error())
		_ = archiveFailedRunDirectory(storageRoot, request.RaceRunID)
		return nil, err
	}
	return session, nil
}

func (session *runSession) Start(timeout time.Duration) error {
	for _, capture := range session.captures {
		capture := capture
		session.wg.Add(1)
		go func() {
			defer session.wg.Done()
			if err := capture.Run(session.ctx); err != nil && !errors.Is(err, context.Canceled) && session.ctx.Err() == nil {
				select {
				case session.errors <- err:
				default:
				}
			}
		}()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, capture := range session.captures {
		select {
		case <-capture.Ready():
		case err := <-session.errors:
			session.cancel()
			session.wg.Wait()
			_ = session.finish("degraded", err.Error())
			return err
		case <-deadline.C:
			err := fmt.Errorf("Recorder start timed out waiting for source %s to write an H.264 keyframe", capture.source.SourceID)
			session.cancel()
			session.wg.Wait()
			_ = session.finish("degraded", err.Error())
			return err
		}
	}
	select {
	case err := <-session.errors:
		session.cancel()
		session.wg.Wait()
		_ = session.finish("degraded", err.Error())
		return err
	default:
	}
	session.manifestMu.Lock()
	session.manifest.State = "recording"
	session.manifestMu.Unlock()
	if err := session.writeManifest(); err != nil {
		session.cancel()
		session.wg.Wait()
		_ = session.finish("degraded", err.Error())
		return err
	}
	go session.watchSourceErrors()
	return nil
}

func (session *runSession) watchSourceErrors() {
	for {
		select {
		case <-session.ctx.Done():
			return
		case err := <-session.errors:
			if err == nil {
				continue
			}
			if session.ctx.Err() != nil {
				return
			}
			session.manifestMu.Lock()
			session.manifest.State = "degraded"
			if session.manifest.Error == "" {
				session.manifest.Error = err.Error()
			}
			session.manifestMu.Unlock()
			_ = session.writeManifest()
			if session.onSourceFail != nil {
				session.onSourceFail(err)
			}
		}
	}
}

func (session *runSession) Stop(reason string) error {
	var result error
	session.stopOnce.Do(func() {
		session.cancel()
		session.wg.Wait()
		state := "complete"
		if reason == "race_aborted" {
			state = "aborted"
		}
		result = session.finish(state, "")
	})
	return result
}

func (session *runSession) currentState() (string, string) {
	session.manifestMu.Lock()
	defer session.manifestMu.Unlock()
	return session.manifest.State, session.manifest.Error
}

func (session *runSession) finish(state string, runError string) error {
	session.manifestMu.Lock()
	if session.manifest.Error != "" {
		state = "degraded"
		if runError == "" {
			runError = session.manifest.Error
		}
	}
	session.manifestMu.Unlock()
	closeErr := session.closeCaptures(state, runError)
	session.manifestMu.Lock()
	session.manifest.State = state
	session.manifest.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if runError != "" {
		session.manifest.Error = runError
	}
	if closeErr != nil && session.manifest.Error == "" {
		session.manifest.Error = closeErr.Error()
		session.manifest.State = "degraded"
	}
	session.manifestMu.Unlock()
	manifestErr := session.writeManifest()
	if closeErr != nil {
		return closeErr
	}
	return manifestErr
}

func (session *runSession) closeCaptures(state string, runError string) error {
	session.manifestMu.Lock()
	if len(session.manifest.Sources) > 0 {
		session.manifestMu.Unlock()
		return nil
	}
	session.manifestMu.Unlock()
	var manifests []sourceManifest
	var firstErr error
	for _, capture := range session.captures {
		manifest, err := capture.Close(state, runError, time.Now().UTC())
		manifests = append(manifests, manifest)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	session.manifestMu.Lock()
	session.manifest.Sources = manifests
	session.manifestMu.Unlock()
	return firstErr
}

func (session *runSession) writeManifest() error {
	session.manifestFile.Lock()
	defer session.manifestFile.Unlock()
	session.manifestMu.Lock()
	payload, err := json.MarshalIndent(session.manifest, "", "  ")
	session.manifestMu.Unlock()
	if err != nil {
		return fmt.Errorf("encode run manifest: %w", err)
	}
	temporary := filepath.Join(session.directory, "manifest.json.tmp")
	final := filepath.Join(session.directory, "manifest.json")
	if err := os.WriteFile(temporary, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write run manifest: %w", err)
	}
	if err := os.Rename(temporary, final); err != nil {
		return fmt.Errorf("publish run manifest: %w", err)
	}
	return nil
}
