# open-appsec attachment for traefik.
#
# Build from the repository root:
#   docker build -f docker/openappsec-traefik/Dockerfile \
#       --build-arg TRAEFIK_BASE=docker.io/library/traefik:v3.7.10 .
#
# The resulting image is the official traefik image plus:
#   - openappsec-traefik-daemon: links the nano attachment libraries and talks
#     to the open-appsec agent over shared memory IPC.
#   - the "openappsec" local traefik plugin (pure Go middleware) that forwards
#     traffic to the daemon for inspection.
ARG TRAEFIK_BASE=docker.io/library/traefik:v3.7.10

FROM golang:1.24-alpine AS builder

RUN apk add --no-cache build-base cmake pkgconfig zlib-dev brotli-dev python3

WORKDIR /src
COPY CMakeLists.txt ./
COPY cmake ./cmake
COPY external ./external
COPY core ./core
COPY nodes ./nodes
COPY docker/CMakeLists.txt ./docker/CMakeLists.txt
COPY docker/openappsec-envoy-attachments/CMakeLists.txt ./docker/openappsec-envoy-attachments/CMakeLists.txt
COPY docker/openappsec-kong-plugin/CMakeLists.txt ./docker/openappsec-kong-plugin/CMakeLists.txt
COPY docker/openappsec-kong-gateway-plugin/CMakeLists.txt ./docker/openappsec-kong-gateway-plugin/CMakeLists.txt
COPY docker/openappsec-waf-webhook/CMakeLists.txt ./docker/openappsec-waf-webhook/CMakeLists.txt
COPY attachments ./attachments

RUN cmake -S . -B build -DATTACHMENT_TYPE=traefik -DCMAKE_BUILD_TYPE=Release -DCMAKE_POLICY_VERSION_MINIMUM=3.5 && \
    cmake --build build --target nano_attachment nano_attachment_util shmem_ipc_2 osrc_compression_utils -j "$(nproc)"

ENV CGO_ENABLED=1
RUN cd attachments/traefik/daemon && \
    CGO_LDFLAGS="-L/src/build/attachments/nano_attachment -L/src/build/attachments/nano_attachment/nano_attachment_util -L/src/build/core/shmem_ipc_2 -L/src/build/core/compression" \
    go build -o /out/openappsec-traefik-daemon .

FROM ${TRAEFIK_BASE}

USER root

RUN apk add --no-cache libstdc++ brotli-libs zlib

COPY --from=builder /src/build/core/shmem_ipc_2/libshmem_ipc_2.so /usr/lib/
COPY --from=builder /src/build/core/compression/libosrc_compression_utils.so /usr/lib/
COPY --from=builder /src/build/attachments/nano_attachment/libnano_attachment.so /usr/lib/
COPY --from=builder /src/build/attachments/nano_attachment/nano_attachment_util/libnano_attachment_util.so /usr/lib/
COPY --from=builder /out/openappsec-traefik-daemon /usr/local/bin/openappsec-traefik-daemon

# The local (Yaegi) plugin sources; traefik loads them from /plugins-local.
COPY attachments/traefik/plugin/ /plugins-local/src/github.com/openappsec/openappsec-traefik-plugin/
RUN rm -f /plugins-local/src/github.com/openappsec/openappsec-traefik-plugin/*_test.go

# Mark this deployment as an attachment running in its own container next to
# the agent container (shared /dev/shm via "ipc: service:<agent>").
RUN touch /etc/dual_docker && \
    echo "EFFECTIVE_SHM_SEGMENT_SIZE=4096" > /etc/attachment-metadata

COPY docker/openappsec-traefik/entrypoint.sh /openappsec-entrypoint.sh
RUN chmod +x /openappsec-entrypoint.sh

# Register the local plugin so users only need to reference the "openappsec"
# middleware in their dynamic configuration.
ENV TRAEFIK_EXPERIMENTAL_LOCALPLUGINS_OPENAPPSEC_MODULENAME=github.com/openappsec/openappsec-traefik-plugin

ENTRYPOINT ["/openappsec-entrypoint.sh"]
CMD ["traefik"]
