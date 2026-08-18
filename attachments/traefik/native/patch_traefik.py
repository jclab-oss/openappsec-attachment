#!/usr/bin/env python3
"""Patches a traefik source tree to compile the open-appsec middleware in.

Usage: patch_traefik.py <traefik-source-dir> <plugin-source-dir>

Traefik has no supported way to build a middleware into the binary, so this
adds one: `middlewarenative.go` registers a compiled-in builder keyed by plugin
module name, and the local-plugin loop consults that registry before falling
back to Yaegi. Everything else — how the middleware is declared, configured and
placed in the chain — stays identical to the interpreted plugin.

The edits are anchored on distinctive lines rather than applied as a context
diff, so they survive minor traefik releases; anything unexpected is a hard
error rather than a silently unpatched binary.
"""
import os
import shutil
import subprocess
import sys

MODULE = "github.com/openappsec/openappsec-traefik-plugin"

BUILDER_ANCHOR = "\tfor pName, desc := range localPlugins {\n"
BUILDER_HOOK = """\t\t// Middlewares compiled into this binary bypass the interpreter and do
\t\t// not need a manifest on disk.
\t\tif nativeBuilder, ok := nativeMiddlewareBuilders[desc.ModuleName]; ok {
\t\t\tpb.middlewareBuilders[pName] = nativeBuilder
\t\t\tcontinue
\t\t}

"""


def fail(message):
    print("patch_traefik: " + message, file=sys.stderr)
    sys.exit(1)


def patch_builder(traefik_dir):
    path = os.path.join(traefik_dir, "pkg", "plugins", "builder.go")
    if not os.path.exists(path):
        fail("{} not found; traefik layout changed".format(path))

    with open(path) as handle:
        source = handle.read()

    if "nativeMiddlewareBuilders" in source:
        return  # already patched

    if source.count(BUILDER_ANCHOR) != 1:
        fail(
            "expected exactly one local-plugin loop in builder.go, found {}; "
            "traefik's plugin builder changed and the hook needs updating".format(
                source.count(BUILDER_ANCHOR)
            )
        )

    with open(path, "w") as handle:
        handle.write(source.replace(BUILDER_ANCHOR, BUILDER_ANCHOR + BUILDER_HOOK))


def wire_module(traefik_dir, plugin_dir):
    # The plugin is stdlib-only, so a local replace is all it takes to build it
    # into traefik.
    for args in (
        ["go", "mod", "edit", "-require={}@v0.0.0".format(MODULE)],
        ["go", "mod", "edit", "-replace={}={}".format(MODULE, plugin_dir)],
    ):
        subprocess.run(args, cwd=traefik_dir, check=True)


def main():
    if len(sys.argv) != 3:
        fail("usage: patch_traefik.py <traefik-source-dir> <plugin-source-dir>")

    traefik_dir, plugin_dir = sys.argv[1], sys.argv[2]
    here = os.path.dirname(os.path.abspath(__file__))

    shutil.copyfile(
        os.path.join(here, "middlewarenative.go"),
        os.path.join(traefik_dir, "pkg", "plugins", "middlewarenative.go"),
    )
    patch_builder(traefik_dir)
    wire_module(traefik_dir, plugin_dir)
    print("patch_traefik: traefik patched to build in " + MODULE)


if __name__ == "__main__":
    main()
