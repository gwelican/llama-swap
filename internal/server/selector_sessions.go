package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// sessionsFullError reports that every target of a sessions selector is
// held by an active session. The middleware turns it into a 429 with a
// Retry-After hint.
type sessionsFullError struct {
	retry time.Duration
}

func (e *sessionsFullError) Error() string {
	return fmt.Sprintf("all session targets are busy, retry in %s", e.retry.Round(time.Second))
}

// sessionSlot tracks one conversation pinned to a target.
type sessionSlot struct {
	target     string
	modelID    string
	lastActive time.Time
}

// selectorSessionsState holds the session bookkeeping for one sessions
// selector: which fingerprint owns which target, in-flight request counts
// per target, and waiters queued while every target is full.
type selectorSessionsState struct {
	mu           sync.Mutex
	idleTimeout  time.Duration
	maxPer       int
	onFull       string
	queueTimeout time.Duration
	retryAfter   time.Duration
	targets      []spilloverTarget
	sessions     map[string]*sessionSlot
	inflight     map[string]int
	rr           uint64
	waiters      []chan struct{}
	sweepEvery   time.Duration
	now          func() time.Time
	sweeperOnce  sync.Once
}

type selectorSessionsTracker struct {
	states map[string]*selectorSessionsState
}

func newSelectorSessionsTracker(cfg config.Config) *selectorSessionsTracker {
	tracker := &selectorSessionsTracker{states: make(map[string]*selectorSessionsState)}
	for selectorID, selector := range cfg.Selectors {
		if selector.Strategy != config.SelectorStrategySessions {
			continue
		}
		idle := selector.Settings.SessionIdleTimeout
		sweepEvery := idle / 4
		if sweepEvery < 10*time.Millisecond {
			sweepEvery = 10 * time.Millisecond
		}
		if sweepEvery > 30*time.Second {
			sweepEvery = 30 * time.Second
		}
		tracker.states[selectorID] = &selectorSessionsState{
			idleTimeout:  idle,
			maxPer:       selector.Settings.MaxSessionsPerTarget,
			onFull:       selector.Settings.OnFull,
			queueTimeout: selector.Settings.QueueTimeout,
			retryAfter:   selector.Settings.RetryAfter,
			targets:      make([]spilloverTarget, 0, len(selector.Targets)),
			sessions:     make(map[string]*sessionSlot),
			inflight:     make(map[string]int, len(selector.Targets)),
			sweepEvery:   sweepEvery,
			now:          time.Now,
		}
		for _, target := range selector.Targets {
			realName, local := cfg.RealModelName(target)
			// Validation guarantees local targets for this strategy.
			tracker.states[selectorID].targets = append(tracker.states[selectorID].targets, spilloverTarget{
				target:  target,
				modelID: realName,
				local:   local,
			})
		}
	}
	return tracker
}

func (t *selectorSessionsTracker) state(selectorID string) *selectorSessionsState {
	if t == nil {
		return nil
	}
	return t.states[selectorID]
}

// release drops one in-flight reservation for the target and sweeps for
// idle sessions so a freed slot wakes queued waiters promptly.
func (t *selectorSessionsTracker) release(selectorID, target string) {
	state := t.state(selectorID)
	if state == nil {
		return
	}
	state.mu.Lock()
	for i := range state.targets {
		if state.targets[i].target == target && state.inflight[state.targets[i].modelID] > 0 {
			state.inflight[state.targets[i].modelID]--
			break
		}
	}
	state.sweepLocked()
	state.mu.Unlock()
}

// resolve picks the target for one request: a known fresh fingerprint stays
// on its target, a new fingerprint takes the least-busy free target (or
// waits / fails per onFull when none is free), and an untracked request
// (no fingerprint) routes least-busy without registering.
//
// On success the caller holds a session slot (when tracked) and an in-flight
// reservation, and must call the tracker's release when the request
// completes.
func (s *selectorSessionsState) resolve(r *http.Request) (string, error) {
	s.startSweeper()

	fp := swaputil.SessionFingerprint(r)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	if fp != "" {
		if slot := s.sessions[fp]; slot != nil && s.targetConfigured(slot.target) {
			slot.lastActive = s.now()
			s.inflight[slot.modelID]++
			return slot.target, nil
		}
		delete(s.sessions, fp)
	}

	target, modelID, err := s.allocate(r, fp)
	if err != nil {
		return "", err
	}
	if fp != "" {
		s.sessions[fp] = &sessionSlot{target: target, modelID: modelID, lastActive: s.now()}
	}
	s.inflight[modelID]++
	return target, nil
}

// allocate picks the target for a new or untracked session. The caller must
// hold s.mu; it is released while a queued waiter blocks.
func (s *selectorSessionsState) allocate(r *http.Request, fp string) (string, string, error) {
	if fp == "" {
		target, modelID := s.pickLeastBusyLocked()
		return target, modelID, nil
	}

	if s.onFull != config.SelectorSessionsOnFullQueue {
		target, modelID, ok := s.pickFreeLocked()
		if ok {
			return target, modelID, nil
		}
		return "", "", &sessionsFullError{retry: s.retryAfter}
	}

	deadline := s.now().Add(s.queueTimeout)
	for {
		if target, modelID, ok := s.pickFreeLocked(); ok {
			return target, modelID, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", "", fmt.Errorf("timed out after %s waiting for a free session slot", s.queueTimeout)
		}

		ch := make(chan struct{})
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()
		select {
		case <-ch:
			s.mu.Lock()
		case <-r.Context().Done():
			s.mu.Lock()
			s.removeWaiter(ch)
			return "", "", r.Context().Err()
		case <-time.After(remaining):
			s.mu.Lock()
			s.removeWaiter(ch)
			return "", "", fmt.Errorf("timed out after %s waiting for a free session slot", s.queueTimeout)
		}
		s.sweepLocked()
	}
}

// pickFreeLocked picks the least-busy target with a free session slot,
// breaking ties by rotating through the target list.
func (s *selectorSessionsState) pickFreeLocked() (string, string, bool) {
	n := len(s.targets)
	if n == 0 {
		return "", "", false
	}
	start := int(s.rr) % n
	best, bestBusy := -1, 0
	for i := 0; i < n; i++ {
		t := s.targets[(start+i)%n]
		if s.sessionsOn(t.target) >= s.maxPer {
			continue
		}
		busy := s.inflight[t.modelID]
		if best == -1 || busy < bestBusy {
			best, bestBusy = i, busy
		}
	}
	if best == -1 {
		return "", "", false
	}
	s.rr = uint64((start + best + 1) % n)
	t := s.targets[(start+best)%n]
	return t.target, t.modelID, true
}

// pickLeastBusyLocked picks the least-busy target regardless of session
// slots; used for untracked requests that hold no session.
func (s *selectorSessionsState) pickLeastBusyLocked() (string, string) {
	n := len(s.targets)
	if n == 0 {
		return "", ""
	}
	start := int(s.rr) % n
	best, bestBusy := -1, 0
	for i := 0; i < n; i++ {
		t := s.targets[(start+i)%n]
		busy := s.inflight[t.modelID]
		if best == -1 || busy < bestBusy {
			best, bestBusy = i, busy
		}
	}
	s.rr = uint64((start + best + 1) % n)
	t := s.targets[(start+best)%n]
	return t.target, t.modelID
}

func (s *selectorSessionsState) sessionsOn(target string) int {
	n := 0
	for _, slot := range s.sessions {
		if slot.target == target {
			n++
		}
	}
	return n
}

func (s *selectorSessionsState) targetConfigured(target string) bool {
	for i := range s.targets {
		if s.targets[i].target == target {
			return true
		}
	}
	return false
}

// sweepLocked expires idle sessions and wakes queued waiters when a slot
// was freed. The caller must hold s.mu.
func (s *selectorSessionsState) sweepLocked() {
	now := s.now()
	freed := false
	for fp, slot := range s.sessions {
		if now.Sub(slot.lastActive) >= s.idleTimeout {
			delete(s.sessions, fp)
			freed = true
		}
	}
	if freed {
		for _, ch := range s.waiters {
			close(ch)
		}
		s.waiters = nil
	}
}

func (s *selectorSessionsState) removeWaiter(ch chan struct{}) {
	for i, w := range s.waiters {
		if w == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			break
		}
	}
}

// startSweeper lazily starts the background goroutine that frees idle
// sessions, so queued waiters are woken even without new traffic.
func (s *selectorSessionsState) startSweeper() {
	s.sweeperOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(s.sweepEvery)
			defer ticker.Stop()
			for range ticker.C {
				s.mu.Lock()
				s.sweepLocked()
				s.mu.Unlock()
			}
		}()
	})
}

// strategySessions resolves the target for a sessions-strategy request.
// Session state lives entirely in this process, so targets are local by
// construction (validated at load time); process state does not gate the
// choice because a swap: false group keeps every target resident.
func strategySessions(selectorID string, tracker *selectorSessionsTracker, r *http.Request) (string, error) {
	state := tracker.state(selectorID)
	if state == nil || len(state.targets) == 0 {
		return "", fmt.Errorf("selector has no targets")
	}
	return state.resolve(r)
}

// sessionsWriteError answers the errors a sessions resolve can return: a
// full selector becomes a 429 with Retry-After, and a client that
// disconnected while queued needs no answer at all. It reports whether the
// error was handled and the caller should stop processing the request.
func sessionsWriteError(w http.ResponseWriter, r *http.Request, err error) bool {
	var full *sessionsFullError
	switch {
	case errors.As(err, &full):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(full.retry.Seconds()))))
		swaputil.SendResponse(w, r, http.StatusTooManyRequests, full.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Client went away while the request was queued; nothing to answer.
	default:
		return false
	}
	return true
}

// releaseAfter returns the function that drops the in-flight reservation
// once the request completes. An ignored websocket holds no session, so its
// reservation is released immediately and the returned function is a no-op.
func (t *selectorSessionsTracker) releaseAfter(cfg config.Config, selectorID, target string, r *http.Request) func() {
	modelConfig, _, local := cfg.FindConfig(target)
	if local && modelConfig.Compat.IgnoreWebsockets && swaputil.IsWebSocketUpgrade(r) {
		t.release(selectorID, target)
		return func() {}
	}
	return func() { t.release(selectorID, target) }
}
