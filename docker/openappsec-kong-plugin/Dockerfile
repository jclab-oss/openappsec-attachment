# Kong with the open-appsec plugin.
#
# Build from the repository root:
#   docker build -f docker/openappsec-kong-plugin/Dockerfile --build-arg KONG_BASE=kong .
ARG KONG_BASE=kong

FROM ${KONG_BASE}

USER root

RUN touch /etc/dual_docker
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential git curl zlib1g-dev libbrotli-dev ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN curl -sL https://github.com/USCiLab/cereal/archive/refs/tags/v1.3.2.tar.gz | tar xz && \
    mkdir -p /usr/local/include/cereal && \
    cp -r cereal-1.3.2/include/cereal /usr/local/include/ && \
    rm -rf cereal-1.3.2

# Create attachment-metadata file
RUN echo "EFFECTIVE_SHM_SEGMENT_SIZE=4096" > /etc/attachment-metadata

# Build the plugin from this checkout. `luarocks install <url>` would fetch the
# published rockspec instead, which pins its own source and would leave the
# image without whatever this tree changed — including the C sources the
# rockspec compiles into the plugin. The module paths in the rockspec are
# relative to the repository root, so the build runs from there.
COPY attachments /src/attachments
COPY core /src/core
COPY external /src/external

RUN cd /src && \
    luarocks make attachments/kong/plugins/open-appsec-waf-kong-plugin/open-appsec-waf-kong-plugin-1.0.0-1.rockspec && \
    rm -rf /src

USER kong

# Enable the plugin
ENV KONG_PLUGINS=bundled,open-appsec-waf-kong-plugin
