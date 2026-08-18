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
