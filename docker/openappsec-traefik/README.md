# openappsec-traefik image

Traefik with the open-appsec attachment baked in: the official traefik image
plus the `openappsec` local middleware plugin and the
`openappsec-traefik-daemon` (see `attachments/traefik/`).

## Build

From the repository root:

```bash
docker build -f docker/openappsec-traefik/Dockerfile \
    --build-arg TRAEFIK_BASE=docker.io/library/traefik:v3.7.10 \
    -t openappsec-traefik .
```

`TRAEFIK_BASE` selects the traefik base image (default
`docker.io/library/traefik:v3.7.10`). Any alpine-based official traefik v3
image works.

## Run

The container must share the IPC namespace (and therefore `/dev/shm`) with the
open-appsec agent container. Minimal `docker compose` example:

```yaml
services:
  appsec-agent:
    image: ghcr.io/openappsec/agent:latest
    ipc: shareable
    environment:
      - user_email=user@example.com
      - registered_server=Traefik Server
      - autoPolicyLoad=true
    volumes:
      - ./appsec-config:/etc/cp/conf
      - ./appsec-data:/etc/cp/data
      - ./appsec-logs:/var/log/nano_agent
      - ./appsec-localconfig:/ext/appsec
    command: /cp-nano-agent

  traefik:
    image: ghcr.io/openappsec/openappsec-traefik:v3.7.10
    ipc: service:appsec-agent
    depends_on:
      - appsec-agent
    ports:
      - "80:80"
    volumes:
      - ./dynamic.yml:/etc/traefik/dynamic.yml:ro
    command:
      - --entrypoints.web.address=:80
      - --providers.file.filename=/etc/traefik/dynamic.yml
```

The image already registers the local plugin (via
`TRAEFIK_EXPERIMENTAL_LOCALPLUGINS_OPENAPPSEC_MODULENAME`), so the middleware
only has to be referenced in the dynamic configuration:

```yaml
http:
  routers:
    my-app:
      rule: PathPrefix(`/`)
      entryPoints: [web]
      middlewares: [appsec]
      service: my-app
  middlewares:
    appsec:
      plugin:
        openappsec: {}
  services:
    my-app:
      loadBalancer:
        servers:
          - url: http://my-app:8080
```

See `attachments/traefik/README.md` for all plugin/daemon options. Traffic
fails open while the agent is unavailable (set `failClose: true` on the
middleware to invert this).

## Running the daemon beside traefik instead

`daemon.Dockerfile` builds the daemon on its own
(`ghcr.io/jclab-oss/openappsec-traefik-daemon`, amd64 and arm64), for
deployments that would rather keep the traefik image untouched and run the
daemon next to it. The plugin still has to be loaded into traefik — that part
cannot move — but everything cgo reaches for lives in this image.

```bash
docker buildx build -f docker/openappsec-traefik/daemon.Dockerfile \
    --platform linux/amd64,linux/arm64 .
```

In Kubernetes it belongs in the traefik pod: containers in a pod share both the
loopback address the plugin dials and the `/dev/shm` the agent communicates
over, so the daemon needs no ports or volumes of its own. Label or annotate the
pod and the admission webhook adds it:

```yaml
metadata:
  labels:
    attachment.openappsec.io/traefik: "true"
```

`docker/openappsec-waf-webhook` reads `TRAEFIK_DAEMON_IMAGE` and
`TRAEFIK_DAEMON_TAG` for which image to inject, and passes through
`CONCURRENCY_CALC` / `CONCURRENCY_NUMBER` (and the other daemon settings) if
they are set on the webhook, each also overridable per-setting with a
`TRAEFIK_` prefix.
