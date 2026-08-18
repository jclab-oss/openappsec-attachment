#!/bin/sh
# Starts the open-appsec daemon next to traefik. The daemon keeps retrying the
# registration with the open-appsec agent in the background, so traefik starts
# serving (fail-open) even when the agent is not up yet.
set -e

if [ "${OPENAPPSEC_DAEMON_DISABLED:-false}" != "true" ]; then
    /usr/local/bin/openappsec-traefik-daemon &
fi

# Traefik uses only ONE static configuration source (file > CLI flags > env
# vars). When traefik is started with CLI flags (the common docker setup), the
# TRAEFIK_EXPERIMENTAL_LOCALPLUGINS_* env var of this image is ignored, so the
# local plugin registration flag is appended to the CLI arguments here.
# Users providing a static configuration FILE must declare the plugin there:
#   experimental:
#     localPlugins:
#       openappsec:
#         moduleName: github.com/openappsec/openappsec-traefik-plugin
PLUGIN_FLAG="--experimental.localplugins.openappsec.modulename=github.com/openappsec/openappsec-traefik-plugin"
if [ "${OPENAPPSEC_AUTO_PLUGIN_FLAG:-true}" = "true" ]; then
    case "${1:-}" in
        traefik|"")
            set -- "$@" "$PLUGIN_FLAG"
            ;;
        -*)
            set -- "$@" "$PLUGIN_FLAG"
            ;;
        *)
            # Custom command (not traefik); leave the arguments untouched.
            ;;
    esac
fi

exec /entrypoint.sh "$@"
