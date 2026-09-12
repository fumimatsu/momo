package main

import (
	"fmt"
	"strings"
	"time"
)

// Only timed practice/qualifying use this cue. Lap race announcements are unchanged.
type raceAudioRemainingTime struct {
	RaceRunID   string `json:"raceRunId"`
	CarID       string `json:"carId"`
	SessionType string `json:"sessionType"`
	TimeLimitMS int64  `json:"timeLimitMs"`
	Minute      int64  `json:"minute"`
	ExpiresAtMS int64  `json:"expiresAtMs"`
}

type raceAudioRemainingSample struct {
	timer                         raceAudioRemainingTime
	elapsed, sequence, serverTime int64
	received                      time.Time
	eligible                      bool
}

type raceAudioRemainingTracker struct {
	previous       *raceAudioRemainingSample
	identity       raceAudioRemainingTime
	consumedMinute int64
}

func (detector *raceAudioDetector) invalidateRemaining() {
	detector.mu.Lock()
	detector.remaining.previous = nil
	detector.mu.Unlock()
}

func (timer raceAudioRemainingTime) identity() raceAudioRemainingTime {
	timer.Minute, timer.ExpiresAtMS = 0, 0
	return timer
}

func (tracker *raceAudioRemainingTracker) observe(state raceAudioState, carID string, now time.Time) *raceAudioEvent {
	self := raceAudioStandingForCar(state, carID)
	limit, mode := state.RaceInfo.TimeLimitMS, state.RaceInfo.SessionType
	if strings.TrimSpace(state.RaceRunID) == "" || strings.TrimSpace(carID) == "" || self == nil ||
		(mode != "practice" && mode != "qualify") || state.AllTimeMode != "elapsed" ||
		limit == nil || *limit <= 0 || *limit > 86400000 || self.AllTimeMS == nil || *self.AllTimeMS < 0 ||
		state.Sequence == nil || *state.Sequence < 0 || state.ServerTimeMS == nil || *state.ServerTimeMS <= 0 {
		tracker.previous = nil
		return nil
	}
	elapsed := *self.AllTimeMS
	minute := elapsed / 60000
	timer := raceAudioRemainingTime{RaceRunID: state.RaceRunID, CarID: carID, SessionType: mode,
		TimeLimitMS: *limit, Minute: minute, ExpiresAtMS: *state.ServerTimeMS + 10000}
	current := &raceAudioRemainingSample{timer: timer, elapsed: elapsed, sequence: *state.Sequence,
		serverTime: *state.ServerTimeMS, received: now,
		eligible: state.Phase == "green" && self.Status == "racing" &&
			state.Flag != "yellow" && state.Flag != "red" && self.DirectionStatus != "wrong_way"}
	previous := tracker.previous
	if previous != nil && previous.timer.identity() == timer.identity() &&
		(current.sequence < previous.sequence || current.serverTime <= previous.serverTime) {
		return nil
	}
	if tracker.identity != timer.identity() {
		tracker.identity, tracker.consumedMinute = timer.identity(), -1
	}
	fresh := previous != nil && previous.timer.identity() == timer.identity() && current.eligible && previous.eligible &&
		!now.Before(previous.received) && now.Sub(previous.received) <= 3500*time.Millisecond &&
		current.serverTime-previous.serverTime <= 3500 && elapsed >= previous.elapsed && elapsed-previous.elapsed <= 3500 &&
		minute == previous.timer.Minute+1 && minute > tracker.consumedMinute && elapsed-minute*60000 <= 3500
	tracker.previous = current
	tracker.consumedMinute = max(tracker.consumedMinute, minute)
	seconds := (*limit - minute*60000 + 999) / 1000
	if !fresh || minute < 1 || seconds <= 0 || elapsed >= *limit {
		return nil
	}
	minutes, tail := seconds/60, seconds%60
	ja := "残り"
	en := make([]string, 0, 2)
	if minutes > 0 {
		ja += fmt.Sprintf("%d分", minutes)
		unit := "minutes"
		if minutes == 1 {
			unit = "minute"
		}
		en = append(en, fmt.Sprintf("%d %s", minutes, unit))
	}
	if tail > 0 {
		ja += fmt.Sprintf("%d秒", tail)
		unit := "seconds"
		if tail == 1 {
			unit = "second"
		}
		en = append(en, fmt.Sprintf("%d %s", tail, unit))
	}
	return &raceAudioEvent{EventID: fmt.Sprintf("%s:%s:time_remaining:%d", state.RaceRunID, carID, minute),
		Kind: "time_remaining", Priority: 60, JapaneseText: ja + "。", EnglishText: strings.Join(en, " ") + " remaining.", RemainingTime: &timer}
}

// Recheck before synthesis, after synthesis, and at playback: a stopped or old run cannot speak from the queue.
func (detector *raceAudioDetector) remainingEventCurrent(event raceAudioEvent, now time.Time) bool {
	if event.Kind != "time_remaining" {
		return true
	}
	detector.mu.Lock()
	defer detector.mu.Unlock()
	timer, previous := event.RemainingTime, detector.remaining.previous
	return timer != nil && previous != nil && previous.eligible && !now.Before(previous.received) &&
		now.Sub(previous.received) <= 3500*time.Millisecond && previous.timer.identity() == timer.identity() &&
		previous.timer.Minute == timer.Minute && previous.elapsed < timer.TimeLimitMS &&
		previous.serverTime+now.Sub(previous.received).Milliseconds() < timer.ExpiresAtMS
}
