package plugins

// This file is injected into traefik's pkg/plugins by patch_traefik.py to build
// a traefik binary with the open-appsec middleware compiled in, instead of
// interpreted by Yaegi.
//
// It hooks the plugin builder rather than traefik's middleware chain on
// purpose: the middleware is still declared and configured exactly like the
// Yaegi plugin (`experimental.localPlugins` plus a `plugin.openappsec` entry in
// the dynamic configuration), so the two variants differ only in how the
// handler is executed. That is what makes them comparable in a benchmark.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mitchellh/mapstructure"
	openappsec "github.com/openappsec/openappsec-traefik-plugin"
	"github.com/rs/zerolog/log"
)

// nativeMiddlewareBuilders maps a plugin module name to a compiled-in builder.
var nativeMiddlewareBuilders = map[string]middlewareBuilder{
	"github.com/openappsec/openappsec-traefik-plugin": openappsecMiddlewareBuilder{},
}

type openappsecMiddlewareBuilder struct{}

// newMiddleware decodes the plugin configuration the same way the Yaegi builder
// does, so identical dynamic configuration produces identical settings.
func (openappsecMiddlewareBuilder) newMiddleware(config map[string]any, middlewareName string) (pluginMiddleware, error) {
	cfg := openappsec.CreateConfig()

	if len(config) > 0 {
		decoderConfig := &mapstructure.DecoderConfig{
			DecodeHook:       mapstructure.StringToSliceHookFunc(","),
			WeaklyTypedInput: true,
			Result:           cfg,
		}

		decoder, err := mapstructure.NewDecoder(decoderConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create configuration decoder: %w", err)
		}

		if err = decoder.Decode(config); err != nil {
			return nil, fmt.Errorf("failed to decode configuration: %w", err)
		}
	}

	// Both variants are configured identically, so this line is what tells a
	// reader (and the benchmark) which one actually served the traffic.
	log.Info().
		Str("middlewareName", middlewareName).
		Msg("open-appsec middleware is compiled in, not interpreted")

	return &openappsecMiddleware{middlewareName: middlewareName, config: cfg}, nil
}

type openappsecMiddleware struct {
	middlewareName string
	config         *openappsec.Config
}

// NewHandler creates a new HTTP handler.
func (m *openappsecMiddleware) NewHandler(ctx context.Context, next http.Handler) (http.Handler, error) {
	return openappsec.New(ctx, next, m.config, m.middlewareName)
}
