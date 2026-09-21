package swaputil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// SessionHeaderName is the optional request header that overrides the
// body-derived session fingerprint. It lets a client pin its own session id
// without relying on body content, for example when two different
// conversations share the same system prompt.
const SessionHeaderName = "X-Session-Id"

// SessionFingerprint returns a stable identifier for the conversation
// (session) behind the request, or an empty string when no identifier can
// be determined.
//
// If the SessionHeaderName header is present, its value is returned
// (prefixed with "header:"). Otherwise, for JSON POSTs with a recognizable
// chat body, a hash of the user agent, system prompt, and first user message
// is returned (prefixed with "fp:"). Both OpenAI-style bodies (system as the
// first message in messages) and Anthropic-style bodies (top-level system)
// are supported; clients resend the full history on every turn, so the first
// user message stays constant within a session.
//
// The request body is always restored before returning, so the caller may
// read it again.
func SessionFingerprint(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(SessionHeaderName)); id != "" {
		return "header:" + id
	}
	if r.Method != http.MethodPost {
		return ""
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) == 0 {
		return ""
	}

	system, firstUser := sessionMessages(body)
	if system == "" && firstUser == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(r.Header.Get("User-Agent") + "\x00" + system + "\x00" + firstUser))
	return "fp:" + hex.EncodeToString(sum[:])
}

// sessionMessages extracts the raw JSON of the system prompt and the first
// user message from an OpenAI- or Anthropic-style chat body.
func sessionMessages(body []byte) (system, firstUser string) {
	system = gjson.GetBytes(body, "system").Raw
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		switch msg.Get("role").String() {
		case "system", "developer":
			if system == "" {
				system = msg.Raw
			}
		case "user":
			if firstUser == "" {
				firstUser = msg.Raw
			}
		}
		if system != "" && firstUser != "" {
			break
		}
	}
	return system, firstUser
}
