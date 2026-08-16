add_subdirectory(nano_attachment)
if ("${ATTACHMENT_TYPE}" STREQUAL "envoy")
    add_subdirectory(envoy)
elseif("${ATTACHMENT_TYPE}" STREQUAL "kong")
    message(STATUS "ATTACHMENT_TYPE = ${ATTACHMENT_TYPE}")
elseif("${ATTACHMENT_TYPE}" STREQUAL "traefik")
    # The traefik daemon (attachments/traefik/daemon) is built with `go build`
    # against the nano_attachment libraries; see docker/openappsec-traefik.
    message(STATUS "ATTACHMENT_TYPE = ${ATTACHMENT_TYPE}")
else()
    add_subdirectory(nginx)
endif()
