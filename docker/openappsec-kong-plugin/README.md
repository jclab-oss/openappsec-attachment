# openappsec-kong-plugin

Kong with the open-appsec plugin installed and enabled, published as
`ghcr.io/<owner>/openappsec-kong-plugin` (amd64 and arm64). The Kong Gateway
build in `../openappsec-kong-gateway-plugin/` is the same image on the
`kong/kong-gateway` base.

CI rebuilds these daily against kong's newest release, as well as whenever the
sources compiled into the plugin change, and publishes three tags:

| Tag | Example | Moves |
| --- | --- | --- |
| `latest` | `:latest` | every build |
| `{kong_version}` | `:3.9.3` | when this repository changes |
| `{kong_version}-openappsec_{commit}` | `:3.9.3-openappsec_81cdf2c0d213` | never |

Two things vary independently — the kong release and the attachment sources —
so only the third tag names both and stays put.

## Build

From the repository root:

```bash
docker build -f docker/openappsec-kong-plugin/Dockerfile \
    --build-arg KONG_BASE=kong:3.8 \
    -t openappsec-kong-plugin .
```

`KONG_BASE` selects the base image; omit it for the base image's own default.

The plugin is built from the checkout you build from, not downloaded. The
rockspec compiles the nano attachment C sources into the plugin, so an image
built from a published rockspec carries whatever those sources looked like at
that release — which is not what this repository contains if it has changed
them. Building locally is what makes the two agree.

## Use

The image sets `KONG_PLUGINS=bundled,open-appsec-waf-kong-plugin`; enable the
plugin per service or globally as with any Kong plugin. It reaches the
open-appsec agent over `/dev/shm`, so the agent has to share that namespace —
`ipc: service:<agent>` in compose, or the same pod in Kubernetes.
