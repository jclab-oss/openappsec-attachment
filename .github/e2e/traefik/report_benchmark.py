#!/usr/bin/env python3
"""Renders the oha result files written by run-benchmark.sh as a markdown report."""
import json
import os
import sys

SCENARIOS = [
    ("get", "GET /", "no request body"),
    ("post", "POST /form", "with request body inspection"),
]

# Column label -> result file suffix. The baseline is the same traefik binary
# serving the same backend without the middleware.
VARIANTS = [
    ("No attachment", "baseline-yaegi"),
    ("Yaegi plugin", "yaegi"),
    ("Built-in middleware", "native"),
]

METRICS = [
    ("Throughput", "rps", " req/s"),
    ("Latency mean", "mean", " ms"),
    ("Latency p50", "p50", " ms"),
    ("Latency p95", "p95", " ms"),
    ("Latency p99", "p99", " ms"),
]


def load(result_dir, name):
    path = os.path.join(result_dir, name + ".json")
    if not os.path.exists(path):
        return None
    with open(path) as handle:
        data = json.load(handle)
    metrics = data["metrics"]
    latency = metrics["latency_ms"]
    return {
        "rps": metrics["requests_per_sec"],
        "success_rate": metrics["success_rate"],
        "mean": latency["mean"],
        "p50": latency["p50"],
        "p95": latency["p95"],
        "p99": latency["p99"],
        "chunks": read_int(result_dir, name, "chunks"),
        "failures": read_int(result_dir, name, "failures"),
        "inspecting_after": read_text(result_dir, name, "inspecting"),
    }


def read_int(result_dir, name, suffix):
    """A counter recorded alongside the run, if present."""
    value = read_text(result_dir, name, suffix)
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def read_text(result_dir, name, suffix):
    path = os.path.join(result_dir, "{}.{}".format(name, suffix))
    if not os.path.exists(path):
        return None
    with open(path) as handle:
        return handle.read().strip()


def dedupe(items):
    """Keeps the first occurrence of each message, preserving order."""
    seen = set()
    return [item for item in items if not (item in seen or seen.add(item))]


def pct_delta(before, after):
    """Relative change from before to after, as a signed percentage string."""
    if before is None or after is None or before == 0:
        return "n/a"
    return "{:+.1f}%".format((after - before) / before * 100)


def fmt(value, unit=""):
    if value is None:
        return "n/a"
    return "{:.2f}{}".format(value, unit)


def cell(value, unit, reference, is_reference):
    """A measurement, with its change against the no-attachment column."""
    if value is None:
        return "n/a"
    if is_reference:
        return fmt(value, unit)
    return "{} ({})".format(fmt(value, unit), pct_delta(reference, value))


def main():
    result_dir, requests, concurrency = sys.argv[1], sys.argv[2], sys.argv[3]

    lines = [
        "## traefik attachment benchmark",
        "",
        "The same backend served three ways through the same traefik routing and "
        "the same middleware configuration: without the attachment, with the "
        "middleware interpreted by traefik's Yaegi plugin runtime, and with the "
        "same middleware compiled into traefik. Each variant was measured on its "
        "own, and only after an attack request returned 403 — a middleware that "
        "is failing open would measure nothing but the daemon round-trip.",
        "",
        "`{} requests, concurrency {}, keep-alive disabled` on `{} CPUs` with "
        "`{} attachment workers`".format(
            requests,
            concurrency,
            os.environ.get("BENCH_CPUS", "unknown"),
            os.environ.get("BENCH_WORKERS", "unknown"),
        ),
        "",
    ]

    notes = []
    warnings = []
    for key, title, note in SCENARIOS:
        measurements = [(label, load(result_dir, "{}-{}".format(key, suffix))) for label, suffix in VARIANTS]
        reference = measurements[0][1]
        if reference is None:
            continue

        lines += [
            "### {} ({})".format(title, note),
            "",
            "| Metric | " + " | ".join(label for label, _ in measurements) + " |",
            "| --- |" + " ---: |" * len(measurements),
        ]
        for label, metric, unit in METRICS:
            cells = [
                cell(data[metric] if data else None, unit, reference[metric], index == 0)
                for index, (_, data) in enumerate(measurements)
            ]
            lines.append("| {} | {} |".format(label, " | ".join(cells)))
        lines.append(
            "| Success rate | {} |".format(
                " | ".join(
                    "{:.1%}".format(data["success_rate"]) if data else "n/a"
                    for _, data in measurements
                )
            )
        )
        lines.append(
            "| Chunks inspected | {} |".format(
                " | ".join(
                    "—" if index == 0 else
                    ("{:,}".format(data["chunks"]) if data and data["chunks"] is not None else "n/a")
                    for index, (_, data) in enumerate(measurements)
                )
            )
        )
        lines.append("")

        # Timings are only comparable when the agent asked both variants for a
        # similar amount of inspection.
        inspected = [
            (label, data["chunks"])
            for label, data in measurements[1:]
            if data and data["chunks"] is not None
        ]
        if len(inspected) == 2:
            low, high = sorted(value for _, value in inspected)
            # Both at zero is not a mismatch — neither inspected anything, which
            # the "no longer inspecting" warning already covers.
            if high > 0 and (low <= 0 or high > low * 2):
                notes.append(
                    "{}: the agent asked the two variants for very different amounts of "
                    "inspection ({}), so their timings measure different work, not two "
                    "implementations of the same work.".format(
                        title,
                        " vs ".join("{} {:,} chunks".format(label, value) for label, value in inspected),
                    )
                )

        for index, (label, data) in enumerate(measurements):
            if data is None:
                notes.append("{}: {} was not measured.".format(title, label))
                continue
            if data["success_rate"] < 1.0:
                notes.append(
                    "{}: {} dropped requests (success rate {:.1%}).".format(
                        title, label, data["success_rate"]
                    )
                )
            if index == 0:
                continue

            # An inspected column that inspected nothing is not a fast column.
            if data["inspecting_after"] == "no":
                warnings.append(
                    "{}: {} was no longer inspecting when the run finished — the agent "
                    "sheds inspection under this load — so its numbers are the cost of "
                    "passing traffic through, not of inspecting it.".format(title, label)
                )
            if data["failures"]:
                warnings.append(
                    "{}: {} hit {} daemon failures during the run; each one opens the "
                    "fail-open window, so part of the run was not inspected.".format(
                        title, label, data["failures"]
                    )
                )

        # The two variants run different traefik binaries, so their
        # uninspected baselines have to agree for the comparison to hold.
        native_baseline = load(result_dir, key + "-baseline-native")
        if native_baseline and reference["rps"] and native_baseline["rps"]:
            drift = abs(native_baseline["rps"] - reference["rps"]) / reference["rps"]
            if drift > 0.1:
                notes.append(
                    "{}: the two traefik builds differ by {:.0f}% without the "
                    "middleware ({} vs {} req/s), so part of the built-in column "
                    "is the binary, not the middleware.".format(
                        title,
                        drift * 100,
                        fmt(native_baseline["rps"]),
                        fmt(reference["rps"]),
                    )
                )

    if warnings:
        lines += ["> [!WARNING]"] + ["> " + item for item in dedupe(warnings)] + [""]
    if notes:
        lines += ["> [!NOTE]"] + ["> " + item for item in dedupe(notes)] + [""]

    print("\n".join(lines))


if __name__ == "__main__":
    main()
