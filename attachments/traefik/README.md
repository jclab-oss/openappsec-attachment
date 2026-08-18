# open-appsec traefik attachment

This directory contains the open-appsec attachment for [traefik](https://traefik.io).

Traefik only supports middleware plugins that run inside the
[Yaegi](https://github.com/traefik/yaegi) Go interpreter, which cannot use cgo
or shared memory. The attachment is therefore split into two cooperating
pieces that run inside the same container:

```
             ┌─────────────────────── traefik container ───────────────────────┐
 client ───► │ traefik ──► openappsec plugin (Yaegi, pure Go)                  │
             │                 │ HTTP (127.0.0.1:8579 or unix socket)          │
             │                 ▼                                               │
             │ openappsec-traefik-daemon (cgo, libnano_attachment)             │
             └─────────────────┬─────────────────────────────────────────────--┘
                               │ shared memory IPC (/dev/shm, ipc: service:...)
                               ▼
                     open-appsec agent container
```

- **plugin/** – a traefik [local plugin](https://plugins.traefik.io/install)
  (`github.com/openappsec/openappsec-traefik-plugin`). It is pure Go (stdlib
  only) so it can be interpreted by Yaegi. For every request it sends the
  request metadata, headers and body — and optionally the upstream response —
  to the daemon and enforces the returned verdict (forward, block page,
  redirect, custom response).
- **daemon/** – a small Go program built with cgo against the
  `nano_attachment` C library (the same library used by the kong and envoy
  attachments). It registers with the open-appsec agent over shared memory
  IPC and exposes a local HTTP API for the plugin.

  A transaction holds one attachment for its whole lifetime, the way an nginx
  worker process does, so the number of attachments is also the number of
  transactions that can be inspected at once; past that, requests wait for one.
  This is not tuning. The library runs each IPC call in a thread it cancels on
  timeout, and a cancelled thread can leave the shared-memory ring queue
  half-written — interleaving a second transaction onto the same attachment
  writes over that damage, which the agent reports as a corrupted queue shortly
  before the attachment dies and takes inspection with it.

  Exclusivity contains that damage but does not prevent it: a call that outruns
  the agent's thread timeout still corrupts its own attachment's queue, the
  library restarts that channel, and the requests in flight on it are lost —
  served as block pages even though the agent logged no security decision.

  So more attachments is not more capacity, and fewer is not more safety. Both
  directions lose requests, for opposite reasons — past the agent's capacity
  the queues corrupt, below it transactions pile up waiting for an attachment.
  Measured on a 20-core host at concurrency 20, 20 s of GET traffic:

  | attachments | throughput | reached the backend | corrupted queues |
  | ---: | ---: | ---: | ---: |
  | 4  | 1093 req/s | 84.5% | 0 |
  | 8  | 1335 req/s | 93.2% | 0 |
  | 20 | 1409 req/s | 98.6% | 24 |
  | 60 | 4077 req/s | 0.4% | 102 |

  The 60-attachment row is the shape to recognise: throughput climbs while the
  traffic that actually reached the backend collapses, because serving a block
  page is cheaper than inspecting. One attachment per core — the default — was
  the best of these; treat the setting as how much inspection the agent is
  asked to do at once, and check the share of traffic reaching the backend
  after changing it in either direction.
- **native/** – the same plugin package compiled into traefik instead of
  interpreted. Traefik has no supported way to build a middleware in, so
  `patch_traefik.py` adds one: it registers a compiled-in builder keyed by
  plugin module name and has the local-plugin loop consult that registry
  before falling back to Yaegi. The middleware is declared, configured and
  chained exactly as the interpreted plugin, so the two are drop-in
  alternatives — see the benchmark below for what the interpreter costs.

## Plugin configuration

```yaml
http:
  middlewares:
    appsec:
      plugin:
        openappsec:
          daemonAddr: http://127.0.0.1:8579   # or unix:///path/to.sock
          responseInspection: true            # inspect upstream responses
          maxRequestBodySize: 10485760        # bytes buffered for inspection
          failClose: false                    # block traffic when the daemon is unreachable
          timeoutMs: 30000                    # per-call daemon timeout
          errorBackoffMs: 2000                # skip inspection after a daemon failure
```

The daemon is configured through environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `OPENAPPSEC_DAEMON_LISTEN` | `127.0.0.1:8579` | Listen address (`host:port` or `unix:///path`). |
| `OPENAPPSEC_SESSION_TTL_SEC` | `60` | Idle session garbage-collection timeout. A stuck session holds an attachment, so this bounds how long it can. |
| `OPENAPPSEC_ACQUIRE_TIMEOUT_MS` | `5000` | How long a transaction waits for a free attachment before giving up and going uninspected. |
| `CONCURRENCY_CALC` | `numOfCores` | `numOfCores`, `istioCpuLimit` or `custom`. |
| `CONCURRENCY_NUMBER` | – | Number of attachment workers when `CONCURRENCY_CALC=custom`. This is the inspection concurrency limit. |
| `OPENAPPSEC_DAEMON_DISABLED` | `false` | (entrypoint) do not start the daemon. |

## Verdict semantics

The plugin fails open by default: when the daemon (or the agent behind it) is
unavailable, traffic is forwarded without inspection. Set `failClose: true` to
block instead. Final verdicts (`accept`/`drop`) finalize the session early;
`drop` renders the block page / redirect / custom response provided by the
agent.

## Benchmark

`.github/e2e/traefik/run-benchmark.sh` serves the same backend three ways and
compares them: without the attachment, with the middleware interpreted by
Yaegi, and with the same middleware compiled into traefik. Each traefik variant
serves the backend on two entrypoints — inspected on `:80` and uninspected on
`:81` — so every variant is measured against its own binary's baseline. The
script refuses to measure until an attack request returns 403, because a
failing-open middleware would only measure the daemon round-trip.

Each variant is measured on its own against a freshly started agent. An
attachment derives its shared-memory identity from its worker index, so two
attachments sharing an IPC namespace collide — whether at the same time or one
after the other, since the second inherits the first's registrations. Starting
clean also gives both variants the same warm-up state and keeps the idle
variant from stealing CPU from the measured one.

```bash
docker build -f docker/openappsec-traefik/Dockerfile -t openappsec-traefik:test .
docker build -f docker/openappsec-traefik/Dockerfile.native \
    --build-arg OPENAPPSEC_TRAEFIK_IMAGE=openappsec-traefik:test \
    -t openappsec-traefik-native:test .
./.github/e2e/traefik/run-benchmark.sh
```

`BENCH_DURATION` (default `20s` per case) and `BENCH_CONCURRENCY` (default 20)
tune the load. The CI workflows run it and publish the table to the job summary.

The benchmark runs the middleware **fail-closed**. Failing open makes "could
not inspect" the fastest path through the middleware, which is how an inspected
run ends up looking faster than no middleware at all — a result that cannot be
true. Fail-closed removes that path, and because it replaces it with a flood of
cheap 403s the report also tracks how much traffic actually reached the
backend: a column serving blocked responses is measuring the cost of rejecting
traffic, not of inspecting it. Load is measured over a fixed duration rather
than a fixed request count for the same reason — a couple of thousand requests
is a fraction of a second at these rates, and the run-to-run noise at that
length reached 15%.

Read the "chunks inspected" row before the timings. How much inspection a
request costs is the agent's decision, not the middleware's: a transaction the
agent finalizes on its first call costs one round trip, while one it keeps
inspecting costs a call per request body chunk and per response stage — six or
more. That decision changes as the agent learns, so two variants measured
against separate agents can be asked for very different amounts of work, and
the difference dwarfs anything the execution mode contributes. The report
compares the counts and says so when they diverge; a variant showing no
overhead is usually one the agent stopped inspecting, not a fast one.

That is not hypothetical. Under a burst at concurrency 20 the agent has been
observed to finalize every transaction on its first call and to stop blocking
attacks entirely — a run that looked *faster* than no attachment at all. The
benchmark therefore re-checks after each run that an attack is still blocked,
and labels any column that was no longer inspecting as measuring pass-through,
not inspection. Numbers carrying that warning say something about how the
agent behaves under load; they say nothing about the middleware.

See `docker/openappsec-traefik/` for the container image build.
