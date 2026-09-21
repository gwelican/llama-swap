package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionsStrategyConfig builds a minimal in-memory config for strategy-level
// sessions tests.
func sessionsStrategyConfig(idle time.Duration, onFull string, queueTimeout, retryAfter time.Duration) config.Config {
	return config.Config{
		Selectors: map[string]config.SelectorConfig{
			"pool": {
				Strategy: config.SelectorStrategySessions,
				Targets:  []string{"a", "b"},
				Settings: config.SelectorSettings{
					SessionIdleTimeout:   idle,
					MaxSessionsPerTarget: 1,
					OnFull:               onFull,
					QueueTimeout:         queueTimeout,
					RetryAfter:           retryAfter,
				},
			},
		},
	}
}

// sessionsRequest builds a chat request for the sessions tests, optionally
// tagged with an explicit session id header.
func sessionsRequest(selector, session string) *http.Request {
	body := `{"model":"` + selector + `","messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set(swaputil.SessionHeaderName, session)
	}
	return req
}

func TestServer_SelectorStrategySessions_StickyAllocation(t *testing.T) {
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		time.Hour, config.SelectorSessionsOnFullReject, 0, 30*time.Second))

	first, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	assert.Equal(t, "a", first)

	// Same session sticks to its target while a slot is held.
	second, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	assert.Equal(t, "a", second)

	// A new session takes the other free target.
	third, err := strategySessions("pool", tracker, sessionsRequest("pool", "beta"))
	require.NoError(t, err)
	assert.Equal(t, "b", third)

	// In-flight releases do not break stickiness while the session is fresh.
	tracker.release("pool", "a")
	tracker.release("pool", "b")
	fourth, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	assert.Equal(t, "a", fourth)

	// A slot stays reserved for its session until idle expiry: a third
	// distinct session finds no free slot even with zero in-flight requests.
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "gamma"))
	var full *sessionsFullError
	require.ErrorAs(t, err, &full)
}

func TestServer_SelectorStrategySessions_RejectWhenFull(t *testing.T) {
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		time.Hour, config.SelectorSessionsOnFullReject, 0, 45*time.Second))

	_, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "beta"))
	require.NoError(t, err)

	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "gamma"))
	var full *sessionsFullError
	require.ErrorAs(t, err, &full)
	assert.Equal(t, 45*time.Second, full.retry)
	assert.Contains(t, err.Error(), "busy")
}

func TestServer_SelectorStrategySessions_IdleExpiry(t *testing.T) {
	idle := 100 * time.Millisecond
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		idle, config.SelectorSessionsOnFullReject, 0, 30*time.Second))
	state := tracker.states["pool"]
	now := time.Now()
	state.now = func() time.Time { return now }

	_, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "beta"))
	require.NoError(t, err)

	// After the idle timeout both sessions have been swept and their slots
	// freed; a new session can take over.
	now = now.Add(idle + 1)
	target, err := strategySessions("pool", tracker, sessionsRequest("pool", "gamma"))
	require.NoError(t, err)
	assert.Equal(t, "a", target)

	// The expired alpha session is not sticky anymore. If it were, it would
	// return its old target "a"; as a new session it takes the free "b"
	// instead.
	target, err = strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	assert.Equal(t, "b", target)
}

func TestServer_SelectorStrategySessions_UntrackedLeastBusy(t *testing.T) {
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		time.Hour, config.SelectorSessionsOnFullReject, 0, 30*time.Second))

	// A body without messages has no fingerprint; requests route least-busy
	// and register no session.
	untracked := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"pool"}`))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first, err := strategySessions("pool", tracker, untracked())
	require.NoError(t, err)
	assert.Equal(t, "a", first)

	second, err := strategySessions("pool", tracker, untracked())
	require.NoError(t, err)
	assert.Equal(t, "b", second)

	tracker.release("pool", "a")
	third, err := strategySessions("pool", tracker, untracked())
	require.NoError(t, err)
	assert.Equal(t, "a", third)
}

func TestServer_SelectorStrategySessions_QueueThenServes(t *testing.T) {
	idle := 120 * time.Millisecond
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		idle, config.SelectorSessionsOnFullQueue, 3*time.Second, 30*time.Second))

	_, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "beta"))
	require.NoError(t, err)

	// A third session queues until the held sessions expire idle.
	result := make(chan struct {
		target string
		err    error
	}, 1)
	go func() {
		target, err := strategySessions("pool", tracker, sessionsRequest("pool", "gamma"))
		result <- struct {
			target string
			err    error
		}{target, err}
	}()
	time.Sleep(50 * time.Millisecond)
	tracker.release("pool", "a")
	tracker.release("pool", "b")

	select {
	case r := <-result:
		require.NoError(t, r.err)
		assert.Contains(t, []string{"a", "b"}, r.target)
	case <-time.After(2 * time.Second):
		t.Fatal("queued session was not woken after idle expiry")
	}
}

func TestServer_SelectorStrategySessions_QueueTimeout(t *testing.T) {
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		time.Hour, config.SelectorSessionsOnFullQueue, 150*time.Millisecond, 30*time.Second))

	_, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "beta"))
	require.NoError(t, err)

	start := time.Now()
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "gamma"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)
}

func TestServer_SelectorStrategySessions_CancelWhileQueued(t *testing.T) {
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		time.Hour, config.SelectorSessionsOnFullQueue, 5*time.Second, 30*time.Second))

	_, err := strategySessions("pool", tracker, sessionsRequest("pool", "alpha"))
	require.NoError(t, err)
	_, err = strategySessions("pool", tracker, sessionsRequest("pool", "beta"))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	req := sessionsRequest("pool", "gamma").WithContext(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := strategySessions("pool", tracker, req)
		result <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled queued session did not return")
	}
}

func TestServer_SelectorStrategySessions_BodyFingerprintStable(t *testing.T) {
	tracker := newSelectorSessionsTracker(sessionsStrategyConfig(
		time.Hour, config.SelectorSessionsOnFullReject, 0, 30*time.Second))

	first, err := strategySessions("pool", tracker, sessionsRequest("pool", ""))
	require.NoError(t, err)

	// The same body (same system prompt and first user message) resolves to
	// the same target, regardless of how much history a later turn carries.
	longerBody := strings.Replace(`{"model":"pool","messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"}]}`,
		`{"role":"user","content":"first"}]`,
		`{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]`, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(longerBody))
	req.Header.Set("Content-Type", "application/json")
	second, err := strategySessions("pool", tracker, req)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestServer_SelectorMiddleware_SessionsRejectWhenFull(t *testing.T) {
	cfg := selectorSessionsTestConfig(t, `
      sessionIdleTimeout: 1h
      onFull: reject
      retryAfter: 30s
`)
	local := newStubRouter([]string{"a", "b"}, "")
	selected := make(chan string, 2)
	release := make(chan struct{})
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		data, _ := swaputil.ReadContext(r.Context())
		selected <- data.ModelID
		<-release
		w.WriteHeader(http.StatusOK)
	}
	s := selectorTestServer(t, cfg, local)

	done := make(chan struct{}, 2)
	for _, session := range []string{"alpha", "beta"} {
		go func(session string) {
			s.ServeHTTP(httptest.NewRecorder(), sessionsRequest("coding", session))
			done <- struct{}{}
		}(session)
	}
	first := <-selected
	second := <-selected
	assert.NotEqual(t, first, second)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, sessionsRequest("coding", "gamma"))
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.Contains(t, w.Body.String(), "busy")

	close(release)
	<-done
	<-done
}

func TestServer_SelectorMiddleware_SessionsBodyFingerprintSticky(t *testing.T) {
	cfg := selectorSessionsTestConfig(t, `
      sessionIdleTimeout: 1h
      onFull: reject
      retryAfter: 30s
`)
	local := newStubRouter([]string{"a", "b"}, "")
	selected := make(chan string, 2)
	release := make(chan struct{})
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		data, _ := swaputil.ReadContext(r.Context())
		selected <- data.ModelID
		<-release
		w.WriteHeader(http.StatusOK)
	}
	s := selectorTestServer(t, cfg, local)

	// Two concurrent turns of the same conversation share a body-derived
	// fingerprint and must resolve to the same target.
	done := make(chan struct{}, 2)
	go func() {
		s.ServeHTTP(httptest.NewRecorder(), sessionsRequest("coding", ""))
		done <- struct{}{}
	}()
	first := <-selected

	go func() {
		s.ServeHTTP(httptest.NewRecorder(), sessionsRequest("coding", ""))
		done <- struct{}{}
	}()
	second := <-selected
	assert.Equal(t, first, second)

	close(release)
	<-done
	<-done
}

func TestServer_SelectorMiddleware_SessionsQueueThenServes(t *testing.T) {
	cfg := selectorSessionsTestConfig(t, `
      sessionIdleTimeout: 150ms
      onFull: queue
      queueTimeout: 3s
`)
	local := newStubRouter([]string{"a", "b"}, "")
	release := make(chan struct{})
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		// The held session blocks in the backend; the queued one must not
		// reach it until the slot is actually free.
		if r.Header.Get(swaputil.SessionHeaderName) == "gamma" {
			w.WriteHeader(http.StatusOK)
			return
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}
	s := selectorTestServer(t, cfg, local)

	// alpha holds its slot in the blocking backend stub.
	alphaDone := make(chan struct{})
	go func() {
		defer close(alphaDone)
		s.ServeHTTP(httptest.NewRecorder(), sessionsRequest("coding", "alpha"))
	}()
	time.Sleep(100 * time.Millisecond)

	gammaDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, sessionsRequest("coding", "gamma"))
		gammaDone <- w.Code
	}()
	time.Sleep(50 * time.Millisecond)

	// alpha finishing frees the in-flight slot; the session itself expires
	// after the idle timeout and the queued waiter is woken.
	close(release)
	select {
	case code := <-gammaDone:
		assert.Equal(t, http.StatusOK, code)
	case <-time.After(3 * time.Second):
		t.Fatal("queued session was not routed after idle expiry")
	}
	<-alphaDone
}

func TestServer_SelectorMiddleware_SessionsQueueTimeout(t *testing.T) {
	cfg := selectorSessionsTestConfig(t, `
      sessionIdleTimeout: 1h
      onFull: queue
      queueTimeout: 200ms
`)
	local := newStubRouter([]string{"a", "b"}, "")
	release := make(chan struct{})
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}
	s := selectorTestServer(t, cfg, local)

	// alpha and beta hold both slots in the blocking backend stub.
	holdersDone := make(chan struct{}, 2)
	for _, session := range []string{"alpha", "beta"} {
		go func(session string) {
			s.ServeHTTP(httptest.NewRecorder(), sessionsRequest("coding", session))
			holdersDone <- struct{}{}
		}(session)
	}
	time.Sleep(100 * time.Millisecond)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, sessionsRequest("coding", "gamma"))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "timed out")

	close(release)
	<-holdersDone
	<-holdersDone
}

// selectorSessionsTestConfig loads the sessions middleware test fixture with
// a caller-supplied settings block.
func selectorSessionsTestConfig(t *testing.T, settings string) config.Config {
	t.Helper()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
groups:
  gpus:
    swap: false
    members: [a, b]
selectors:
  coding:
    strategy: sessions
    targets: [a, b]
    settings:
` + settings))
	require.NoError(t, err)
	return cfg
}
