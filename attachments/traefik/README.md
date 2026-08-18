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
| `OPENAPPSEC_SESSION_TTL_SEC` | `120` | Idle session garbage-collection timeout. |
| `CONCURRENCY_CALC` | `numOfCores` | `numOfCores`, `istioCpuLimit` or `custom`. |
| `CONCURRENCY_NUMBER` | – | Number of attachment workers when `CONCURRENCY_CALC=custom`. |
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

`BENCH_REQUESTS` (default 2000) and `BENCH_CONCURRENCY` (default 20) tune the
load. The CI workflows run it and publish the table to the job summary.

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
