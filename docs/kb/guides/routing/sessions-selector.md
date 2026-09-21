---
title: Sticky sessions with the sessions selector
summary: Pin each conversation to one target until it goes idle, and queue or reject new sessions when every target is held.
category: guides
tags: [selectors, sessions, sticky, routing, queue]
config_keys: [selectors, selectors.*.strategy, selectors.*.targets, selectors.*.settings.sessionIdleTimeout, selectors.*.settings.maxSessionsPerTarget, selectors.*.settings.onFull, selectors.*.settings.queueTimeout, selectors.*.settings.retryAfter]
updated: 2026-09-20
---

# Sticky sessions with the sessions selector

The `sessions` strategy pins each conversation to one target for the whole
session. Use it when several clients (for example coding agents) share a set
of equivalent targets — like the same model on two GPUs — and each client
should keep its own target instead of being rebalanced per request.

A session is identified by the `X-Session-Id` request header. Without the
header, llama-swap fingerprints the system prompt and first user message,
which stays stable across the turns of one conversation. The session keeps
its target until it has been idle for `sessionIdleTimeout` (default `5m`);
after that the slot is released and the next request is treated as a new
session.

```yaml
groups:
  gpus:
    swap: false
    members: [coder-3090, coder-4090]

selectors:
  coding:
    strategy: sessions
    targets: [coder-3090, coder-4090]
    settings:
      sessionIdleTimeout: 5m    # slot released after this much silence
      maxSessionsPerTarget: 1   # distinct sessions per target
      onFull: queue             # or reject
      queueTimeout: 10m         # when onFull is queue
      retryAfter: 30s           # when onFull is reject
```

Clients just ask for model `coding`. Agent A lands on `coder-3090`, agent B
on `coder-4090`, and both stay there across turns.

What goes wrong:

- **Config fails with "must resolve to a local model"** — session state lives
  in this process, so peer targets are not allowed.
- **Config fails with "must share a group with swap: false"** — every target
  must be resident at the same time; put them in one `swap: false` group (or
  one matrix set). Otherwise a session could stick to a model that gets
  unloaded.
- **New client hangs, then errors** — every target holds
  `maxSessionsPerTarget` live sessions. With `onFull: queue` the request
  waits inside llama-swap until a slot frees or `queueTimeout` fires (503);
  with `onFull: reject` it fails immediately with `429` and a `Retry-After`
  header.
- **Two clients land on the same target unexpectedly** — they share a system
  prompt and first user message, so they share one fingerprint. Send a
  distinct `X-Session-Id` header per client when that matters.
- **Requests with no JSON chat body** (no header, no parseable messages)
  route to the least busy target without registering a session.

## Related

- `guides/routing/profiles-and-selectors` — profiles and the other selector
  strategies
- `guides/routing/groups-and-matrix` — which models can be loaded at the same
  time
