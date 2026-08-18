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

`.github/e2e/traefik/run-benchmark.sh` measures the cost of inspection: one
traefik instance serves the same backend on two entrypoints, with the
middleware on `:80` and without it on `:81`, so the delta between the two is
the attachment overhead. The script refuses to measure until it has confirmed
the agent is actively inspecting (an attack request returns 403), because a
failing-open attachment would only measure the daemon round-trip.

```bash
OPENAPPSEC_TRAEFIK_IMAGE=openappsec-traefik:test ./.github/e2e/traefik/run-benchmark.sh
```

Concurrency is derived from the CPU count (`nproc / 4`, at least 2) so the
benchmark stays below the point where the agent saturates and the numbers stop
describing per-request cost: on a 20-core host, driving one connection per core
pushed POST p99 from 16 ms to 3 s while p50 stayed at 11 ms. `BENCH_REQUESTS`
(default 2000), `BENCH_CONCURRENCY_DIVISOR` (default 4) and `BENCH_CONCURRENCY`
(pins concurrency outright) tune the load. The CI workflows run it and publish
the table to the job summary.

See `docker/openappsec-traefik/` for the container image build.
