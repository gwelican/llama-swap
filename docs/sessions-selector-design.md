# sessions selector — design and usage

Target reader: a human operator **or a coding agent** maintaining this
llama-swap deployment. This document is written so an agent can read it cold,
understand the `sessions` selector strategy, and edit the deployment config
without reading the source first. A short user-facing version lives in
`docs/kb/guides/routing/sessions-selector.md`.

## 1. What it does

`strategy: sessions` makes a selector **sticky per conversation**:

- A client's first request through the selector is assigned to one target
  GPU (one local model) and stays on that target for the whole life of its
  session — its KV cache stays warm on that GPU.
- A *new* conversation can only be admitted when a target has a free session
  slot (`settings.maxSessionsPerTarget`, default 1). With two GPUs and the
  default setting, at most two concurrent agent conversations run: one per
  GPU.
- If every target is full, the new conversation is either **queued inside
  llama-swap** (default, `onFull: queue`) until a slot is idle, or **rejected**
  with `429 + Retry-After` (`onFull: reject`).
- A session is released after `settings.sessionIdleTimeout` (default 5m)
  without requests. Its GPU slot is then offered to the next queued or new
  conversation (cold KV there — a normal model warm-up, transparent to the
  client).

The point: coding agents (pi, OpenHands, DSH, …) talk to llama-swap and
nobody has to track which GPU is free.

## 2. Configuration reference

Selector config block (all settings optional except `strategy` and
`targets`):

```yaml
selectors:
  coding:
    strategy: sessions
    targets:
      - coder-model-4090
      - coder-model-3090
    settings:
      sessionIdleTimeout: 5m        # idle time before a session slot is freed
      maxSessionsPerTarget: 1       # concurrent sessions allowed per target
      onFull: queue                 # queue | reject
      queueTimeout: 10m             # max time a new session may wait (onFull: queue)
      retryAfter: 30s               # Retry-After hint sent with 429 (onFull: reject)
```

| Setting | Type | Default | Constraint (exact validation error) |
| --- | --- | --- | --- |
| `strategy` | string | — | must be `sessions` |
| `targets[]` | model IDs | — | each must resolve to a **local** model: `selectors.<id>.targets[i] must resolve to a local model for strategy "sessions"`; all targets must be **co-resident** (see below): `selectors.<id> sessions targets must share a group with swap: false` / `must share one routing group` / `must all appear together in one expanded matrix set` |
| `settings.sessionIdleTimeout` | duration | `5m` | positive: `must be a positive duration` |
| `settings.maxSessionsPerTarget` | int | `1` | `must be >= 1` |
| `settings.onFull` | `queue`\|`reject` | `queue` | `unknown mode %q (valid: reject, queue)` |
| `settings.queueTimeout` | duration | `10m` | positive when `onFull: queue` |
| `settings.retryAfter` | duration | `30s` | positive when `onFull: reject` |

**Co-residency requirement (important):** all targets of one sessions
selector must be loadable *at the same time*, because the strategy never
allows a target to be evicted while a session holds it.

- `groups` router: put every target in one group with `swap: false`.
- `matrix` router: every target must appear together in one expanded matrix
  set (e.g. `singles: "+gpu4090 & +gpu3090"` keeps one model per GPU
  resident, which satisfies it for a 4090+3090 target pair).
- `spillover` and `sessions` selectors may coexist in one config, but a
  sessions selector's targets must share one routing group.

Schema lives in `config-schema.json` (`settings.properties.sessionIdleTimeout`
etc., `additionalProperties: false` — unknown settings fail config load).

## 3. How a session is identified

`internal/swaputil/session.go` computes a fingerprint per request, in this
order:

1. **`X-Session-Id` header present** → fingerprint `header:<value>`.
   Explicit, always wins. Use it per client route when you want guaranteed
   separate identities.
2. **JSON POST body with a recognizable message array** (OpenAI-style
   `messages`) → fingerprint `fp:<hex(sha256(UA + "\x00" + system + "\x00" +
   firstUser))>` — user agent, the `system` message (or role/system field),
   and the first user message. Two conversations that share all three are
   treated as one session (collision → shared GPU; see §8).
3. **Anything else** (GETs, non-JSON, empty bodies, no messages) →
   *untracked*: routed to the least-busy target, takes no slot, never
   sticky. Health checks and plain `/v1/models`-style traffic cannot fill a
   GPU.

The request body is read back to the cursor before returning, so downstream
middleware sees it untouched.

## 4. Lifecycle

```
request in ──► fingerprint? ──► known fingerprint ──► stay on that target
                 │                        (slot.lastActive refreshed,
                 │                         in-flight counter bumped)
                 ▼
             new fingerprint ──► free slot? ── yes ──► least-busy free target,
                 │                          │            register slot
                 │                          no
                 │                          │
                 │            onFull: reject ──► 429 + Retry-After
                 │            onFull: queue  ──► wait (up to queueTimeout)
                 ▼
             untracked ──► least-busy target, no slot, no stickiness
```

Rules that matter when reasoning about behavior:

- **Sticky as long as active.** A known fingerprint always returns the same
  target, even when other targets are idle. `lastActive` is refreshed on
  every request, so a long agent that keeps working never loses its GPU.
- **The slot stays reserved at zero in-flight.** "Idle" means *no request
  for 5m*, not "no running request". A session between bursts of tool calls
  keeps its GPU — that is deliberate (prevents flapping between GPUs across
  burst gaps). A third distinct session is rejected/queued while two live
  sessions exist, even if both are momentarily at zero in-flight.
- **Idle expiry frees the slot.** A background sweeper (one goroutine per
  sessions selector, started lazily on first request, ticking every
  `min(30s, idleTimeout/4)` floored at 10ms) drops sessions idle for longer
  than the timeout and wakes queued waiters. The slot is offered again to
  whoever is next in line.
- **Re-request after expiry is a new session.** A fingerprint that came back
  after its slot expired is allocated fresh — it may land on a different
  target (cold KV). Clients never see an error for this.
- **Allocation order:** least-busy free target, with round-robin rotation
  among equally-busy ones, so concurrent new sessions spread across GPUs.
- **Config reload resets all session state** (trackers are rebuilt from
  config), the same way spillover does. Clients are not told; next request
  re-allocates.

## 5. Behavior matrix (what a client observes)

| Scenario | Result |
| --- | --- |
| First session, GPU A free | 200, assigned GPU A, sticky from now on |
| Second session while A is held | 200, assigned GPU B |
| Third session, `onFull: queue` | Request **holds inside llama-swap** (client sees a slow first token) until a session is idle 5m or `queueTimeout` fires |
| Third session, queue times out | `503`, body `timed out after 10m0s waiting for a free session slot` |
| Third session, `onFull: reject` | `429`, header `Retry-After: 30`, body `all session targets are busy, retry in 30s` |
| Client cancels while queued | Connection closed, **no response** (silent by design; the waiter is removed) |
| Same conversation resumes after 5m idle | 200, possibly on the *other* GPU (new session, cold KV) |
| GET / non-JSON / body without messages | 200, least-busy target, no slot taken, not sticky |
| WebSocket upgrade on a model with `compat.ignoreWebsockets: true` | Served, but the in-flight reservation is released immediately and no session is registered |
| llama-swap restart / config reload | All sessions forgotten; next requests re-allocate |

All of the above is covered by tests in
`internal/server/selector_sessions_test.go` (strategy + middleware) and
`internal/config/selectors_sessions_test.go` (config validation).

## 6. Client integration

Any OpenAI-compatible client works unchanged — sessions are detected
automatically (§3). Optionally give each client route an explicit identity:

```
X-Session-Id: dsh-coding-main
```

- **DSH:** set it per provider route via
  `llm-pi-ai.providers.<route>.headers.X-Session-Id` in DSH settings
  (static value; one identity per route). This removes any fingerprint
  collision risk and keeps a route's whole lifetime on one GPU.
- **pi / other HTTP clients:** add the header in the client's provider
  config, or leave it unset and rely on the auto-fingerprint.

Client-side timeout advice: with `onFull: queue`, the *first* request of a
queued session may wait up to `queueTimeout` before any byte is returned.
Set the client's connect/first-byte timeout above `queueTimeout` (or use
`onFull: reject` and let the client retry on `Retry-After`).

## 7. Worked example: this deployment (matrix router)

The live config is `/home/gwelican/ai/config.yaml` (llama-swap in front of a
4090 = `gpu 0` and a 3090 = `gpu 1`, `routing.router.use: matrix` with sets
`gpu4090`, `gpu3090`, `singles: "+gpu4090 & +gpu3090"`, `dual`).

The matrix keeps **one model per GPU** resident via the `singles` set, so a
4090+3090 target pair satisfies the co-residency rule. Verified working
block (add under `selectors:`; the existing `qwen38-planner-balanced` /
`qwen38-coder-balanced` spillover selectors can stay for non-sticky
traffic):

```yaml
selectors:
  qwen38-coder-sessions:
    strategy: sessions
    targets:
      - coder-qwen38-27b-mtp-q4-4090
      - coder-qwen38-27b-mtp-q4-3090
    settings:
      sessionIdleTimeout: 5m
      maxSessionsPerTarget: 1
      onFull: queue
      queueTimeout: 10m
    name: "Qwen3.8 27B coder — sticky session per agent, one agent per GPU"
    description: "Each agent session sticks to one GPU (4090 or 3090) for its whole session; a new agent takes the other GPU, a third agent queues until a slot is idle. Existing per-card model IDs remain directly addressable."
    metadata:
      routing: sessions
      workload: coder
      targets:
        - gpu4090
        - gpu3090
```

Client wiring: point the coding agents' provider model ID at
`qwen38-coder-sessions` (exactly like they use `qwen38-coder-balanced`
today). Optionally add a static `X-Session-Id` per DSH route (§6).

Edit procedure for an agent updating the config:

1. Add/extend the selector block above under `selectors:` in
   `/home/gwelican/ai/config.yaml`. Do not reorder the `macros:` block —
   macro expansion is order-dependent.
2. Validate: run `make test-dev` in a checkout of this repo (covers the
   config validation tests), or just start llama-swap — a bad selector
   fails at config load with the exact message from §2.
3. Restart llama-swap (config reload rebuilds trackers; session state
   resets, §4).
4. Smoke test: `curl` two different `X-Session-Id` values at
   `/v1/chat/completions` with the selector model — both should return 200;
   a third different value should hold (queue) or 429 (reject).

## 8. Design rationale and known limits

Why the shape it has:

- **Sticky, not per-request load-balancing** — the KV cache is per-process.
  Reassigning a conversation to another GPU mid-session throws away the
  cache (a full prompt re-prefill). Stickiness is the whole feature.
- **Reserved slots at zero in-flight** — coding agents alternate long model
  turns with long tool-call gaps. Releasing on "no in-flight request" would
  flap a session between GPUs every tool-call gap; idle-timeout release
  matches real session semantics.
- **Queueing lives in llama-swap** — agents do not implement backoff
  against GPU availability; centralizing the wait gives one place to time
  out, cancel, and observe. `reject` remains for clients that prefer to
  back off themselves (`429` + `Retry-After`).
- **Fingerprint = UA + system + first user** — identifies "the same agent
  conversation" over stateless HTTP without requiring client support. The
  `X-Session-Id` override exists for routes that want a stable identity or
  must be split despite identical prompts.
- **Targets must be co-resident** — the strategy never evicts a target under
  a live session; allowing evictable targets would silently cold-start a
  "sticky" session.

Known limits (accepted, do not "fix" without deciding otherwise):

- **Fingerprint collision:** two distinct conversations with identical
  user agent + system + first user share one slot (both get the same GPU,
  serialized-ish by the underlying server). Mitigation: send
  `X-Session-Id`.
- **Session state is per-process.** Config reload or restart forgets all
  sessions (same behavior as spillover).
- **One sweeper goroutine per sessions selector** for the process lifetime
  (`sync.Once`, no cleanup hook in the middleware factory) — negligible
  cost, no leak concern at this scale.
- **Queued requests look like a slow first token** to the client; there is
  no out-of-band "you are queued" signal.

## 9. Code map

| File | Role |
| --- | --- |
| `internal/swaputil/session.go` | `SessionFingerprint(r)`, `X-Session-Id` constant, body parsing (`sessionMessages`) |
| `internal/config/selectors_sessions.go` | Strategy constant, `onFull` constants, `validateSessionsSelector` + matrix/group coexistence check |
| `internal/config/selectors.go` | Two small upstream-touching hunks: `sessions` added to the strategy switch case, validation call after the targets loop |
| `internal/server/selector_sessions.go` | All runtime logic: state struct, tracker, `resolve`/`allocate`, pickers, sweeper, queue loop, `sessionsWriteError` (429/ctx handling), `releaseAfter` (websocket release) |
| `internal/server/selector.go` | Four additive middleware hunks: tracker construction, strategy dispatch, error write-out, in-flight release |
| `internal/server/selector_sessions_test.go` | Strategy behavior tests + middleware tests (sticky, reject, idle expiry, queue, timeout, cancel, untracked) |
| `internal/config/selectors_sessions_test.go` | Config validation tests (defaults, errors, coexistence) |
| `config-schema.json` | `sessions` in the strategy enum + the five settings |
| `docs/kb/guides/routing/sessions-selector.md` | Short KB user guide (frontmatter contract) |

Key invariants when modifying `selector_sessions.go`:

- `resolve` **always returns holding the mutex** for the allocator;
  `release` takes the mutex itself.
- The queue loop unlocks while waiting and must re-sweep on relock
  (the broadcast closes *and drains* all waiter channels, so a woken
  waiter re-checks `pickFreeLocked`).
- The `now` field is injectable (`s.now = ...`) — tests drive idle expiry
  without real timers; do not call `time.Now` directly inside state
  methods.

## 10. Maintaining this patch against upstream

This is a **local patch, not an upstream PR**: branch `sessions`, one commit
on top of `mostlygeek/llama-swap` main.

Conflict surface (deliberately minimal — everything movable lives in new
files, which never conflict):

| File | Change |
| --- | --- |
| `internal/server/selector.go` | +13/−0, four purely additive hunks |
| `internal/config/selectors.go` | +27/−4, only the strategy case line and the "valid strategies" message truly modified |
| `config-schema.json` | +35/−1, one enum entry + one settings block |
| `.gitignore` | +3 lines |
| 7 new files | all runtime code, tests, and this doc |

Update flow:

```bash
git fetch upstream
git rebase upstream/main sessions
# expected hotspots, if any:
#   internal/server/selector.go  — strategy switch case
#   internal/config/selectors.go — strategy switch case + message
#   config-schema.json           — strategy enum / settings block
go build ./...
make test-dev
git format-patch -1   # insurance: keep the patch file after every rebase
```
