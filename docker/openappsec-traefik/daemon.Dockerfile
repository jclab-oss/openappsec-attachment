# The open-appsec traefik daemon on its own, for running it beside an unmodified
# traefik instead of inside the image built by ./Dockerfile — as a sidecar in
# the traefik pod, for instance. The traefik plugin reaches it over localhost,
# and it reaches the agent over /dev/shm, both of which pod containers share.
#
# Build from the repository root:
#   docker buildx build -f docker/openappsec-traefik/daemon.Dockerfile \
#       --platform linux/amd64,linux/arm64 .
#
# There is no cross-compilation here: the daemon links the nano attachment C
# libraries through cgo, so each architecture is built natively (under emulation
# on the runner if need be).
#
# The builder stage mirrors the one in ./Dockerfile; changes to how the
# attachment libraries or the daemon are built belong in both.
ARG GO_IMAGE=golang:1.24-alpine
ARG RUNTIME_IMAGE=alpine:3.21

FROM ${GO_IMAGE} AS builder

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
    go build -ldflags "-s -w" -o /out/openappsec-traefik-daemon .

FROM ${RUNTIME_IMAGE}

RUN apk add --no-cache libstdc++ brotli-libs zlib

COPY --from=builder /src/build/core/shmem_ipc_2/libshmem_ipc_2.so /usr/lib/
COPY --from=builder /src/build/core/compression/libosrc_compression_utils.so /usr/lib/
COPY --from=builder /src/build/attachments/nano_attachment/libnano_attachment.so /usr/lib/
COPY --from=builder /src/build/attachments/nano_attachment/nano_attachment_util/libnano_attachment_util.so /usr/lib/
COPY --from=builder /out/openappsec-traefik-daemon /usr/local/bin/openappsec-traefik-daemon

# Marks this as an attachment running in its own container beside the agent
# rather than in the agent's own image.
RUN touch /etc/dual_docker && \
    echo "EFFECTIVE_SHM_SEGMENT_SIZE=4096" > /etc/attachment-metadata

# The plugin talks to the daemon over the loopback address the pod's containers
# share; OPENAPPSEC_DAEMON_LISTEN moves it if that is not the case.
EXPOSE 8579

ENTRYPOINT ["/usr/local/bin/openappsec-traefik-daemon"]
