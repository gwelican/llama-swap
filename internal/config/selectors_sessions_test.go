package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_Selectors_SessionsValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "remote target",
			config: `
selectors:
  public:
    strategy: sessions
    targets: [remote-model]
`,
			wantErr: `must resolve to a local model for strategy "sessions"`,
		},
		{
			name: "unknown onFull",
			config: `
selectors:
  public:
    strategy: sessions
    targets: [a]
    settings:
      onFull: bogus
`,
			wantErr: `unknown mode "bogus" (valid: reject, queue)`,
		},
		{
			name: "zero maxSessionsPerTarget",
			config: `
selectors:
  public:
    strategy: sessions
    targets: [a]
    settings:
      maxSessionsPerTarget: 0
`,
			wantErr: "maxSessionsPerTarget must be >= 1",
		},
		{
			name: "zero idle timeout",
			config: `
selectors:
  public:
    strategy: sessions
    targets: [a]
    settings:
      sessionIdleTimeout: 0s
`,
			wantErr: "sessionIdleTimeout must be a positive duration",
		},
		{
			name: "queue without timeout",
			config: `
selectors:
  public:
    strategy: sessions
    targets: [a]
    settings:
      onFull: queue
      queueTimeout: 0s
`,
			wantErr: `queueTimeout must be a positive duration when onFull is "queue"`,
		},
		{
			name: "reject without retryAfter",
			config: `
selectors:
  public:
    strategy: sessions
    targets: [a]
    settings:
      onFull: reject
      retryAfter: 0s
`,
			wantErr: `retryAfter must be a positive duration when onFull is "reject"`,
		},
	}

	const base = `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
peers:
  remote:
    proxy: http://example.com
    models: [remote-model]
`

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader(base + tc.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestConfig_Selectors_SessionsDefaults(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
groups:
  pool:
    swap: false
    members: [a, b]
selectors:
  public:
    strategy: sessions
    targets: [a, b]
`))
	require.NoError(t, err)
	settings := cfg.Selectors["public"].Settings
	assert.Equal(t, 5*time.Minute, settings.SessionIdleTimeout)
	assert.Equal(t, 1, settings.MaxSessionsPerTarget)
	assert.Equal(t, SelectorSessionsOnFullQueue, settings.OnFull)
	assert.Equal(t, 10*time.Minute, settings.QueueTimeout)
	assert.Equal(t, 30*time.Second, settings.RetryAfter)
}

func TestConfig_Selectors_SessionsCoexistence(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
groups:
  pool:
    swap: false
    members: [a, b]
selectors:
  public:
    strategy: sessions
    targets: [a, b]
    settings:
      sessionIdleTimeout: 10m
      maxSessionsPerTarget: 2
      onFull: queue
      queueTimeout: 1m
`))
		require.NoError(t, err)
		assert.Equal(t, 2, cfg.Selectors["public"].Settings.MaxSessionsPerTarget)
	})

	t.Run("swapping group", func(t *testing.T) {
		_, err := LoadConfigFromReader(strings.NewReader(`
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
groups:
  pool:
    swap: true
    members: [a, b]
selectors:
  public:
    strategy: sessions
    targets: [a, b]
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must share a group with swap: false")
	})

	t.Run("different groups", func(t *testing.T) {
		_, err := LoadConfigFromReader(strings.NewReader(`
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
groups:
  first:
    swap: false
    members: [a]
  second:
    swap: false
    members: [b]
selectors:
  public:
    strategy: sessions
    targets: [a, b]
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must share one routing group")
	})
}
