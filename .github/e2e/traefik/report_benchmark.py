#!/usr/bin/env python3
"""Renders the oha result files written by run-benchmark.sh as a markdown report."""
import json
import os
import sys

SCENARIOS = [
    ("get", "GET /", "no request body"),
    ("post", "POST /form", "with request body inspection"),
]

# Column label -> result file suffix. The baseline is the same traefik serving
# the same backend on the entrypoint without the middleware.
VARIANTS = [
    ("No attachment", "baseline"),
    ("open-appsec", "appsec"),
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
    codes = data.get("statusCodeDistribution") or {}
    served = sum(codes.values())
    return {
        "rps": metrics["requests_per_sec"],
        "success_rate": metrics["success_rate"],
        "codes": codes,
        # Fraction of responses that actually came from the backend. Blocked
        # requests are cheap to serve, so a run that got 403'd throughout looks
        # fast for the same reason an uninspected one does.
        "ok_ratio": (sum(count for code, count in codes.items() if code.startswith("2")) / served)
        if served
        else None,
        "mean": latency["mean"],
        "p50": latency["p50"],
        "p95": latency["p95"],
        "p99": latency["p99"],
        "chunks": read_int(result_dir, name, "chunks"),
        "failures": read_int(result_dir, name, "failures"),
        "rejected": read_int(result_dir, name, "rejected"),
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
    result_dir, duration, concurrency = sys.argv[1], sys.argv[2], sys.argv[3]

    lines = [
        "## traefik attachment benchmark",
        "",
        "The same traefik serves the same backend on two entrypoints, with the "
        "middleware and without it, so the difference is the cost of inspection. "
        "Measured only after the middleware had been seen to pass a benign "
        "request and block an attack. It runs fail-closed here: failing open "
        "would make \"could not inspect\" the fastest path through it.",
        "",
        "`{} per case, concurrency {}, keep-alive disabled` on `{} CPUs` with "
        "`{} attachment workers`".format(
            duration,
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
            "| Served by the backend | {} |".format(
                " | ".join(
                    "{:.1%}".format(data["ok_ratio"]) if data and data["ok_ratio"] is not None else "n/a"
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

            # Inspecting cannot make traffic faster than not inspecting it, so
            # a column that comes out ahead is measuring something else —
            # inspection that was skipped, or noise from too short a run.
            if data["rps"] and reference["rps"] and data["rps"] > reference["rps"]:
                warnings.append(
                    "{}: {} measured faster than serving the same traffic with no "
                    "attachment ({} vs {} req/s). Inspection cannot speed traffic up, "
                    "so this is not a result: the run either did not inspect or was too "
                    "short to separate from noise.".format(
                        title, label, fmt(data["rps"]), fmt(reference["rps"])
                    )
                )

            # Fail-closed turns "cannot inspect" into a flood of cheap blocked
            # responses, which is fast for the same reason skipping inspection
            # is fast. Either way the traffic never reached the backend.
            if data["ok_ratio"] is not None and data["ok_ratio"] < 0.99:
                warnings.append(
                    "{}: only {:.1%} of {}'s requests reached the backend ({}); the rest "
                    "were blocked, so its timings are the cost of rejecting traffic, not "
                    "of inspecting and forwarding it.".format(
                        title,
                        data["ok_ratio"],
                        label,
                        ", ".join(
                            "{}x {}".format(count, code) for code, count in sorted(data["codes"].items())
                        ),
                    )
                )

            # An inspected column that inspected nothing is not a fast column.
            if data["inspecting_after"] == "no":
                warnings.append(
                    "{}: {} was no longer inspecting when the run finished — the agent "
                    "sheds inspection under this load — so its numbers are the cost of "
                    "passing traffic through, not of inspecting it.".format(title, label)
                )
            # Past its inspection capacity the daemon queues transactions and
            # eventually lets them through uninspected. That is a statement
            # about the load, not about the implementation.
            if data["rejected"]:
                notes.append(
                    "{}: {} offered more concurrent transactions than the daemon has "
                    "attachments; {:,} waited out the queue and went uninspected. Raise "
                    "the worker count or lower the load to measure inspection at this "
                    "rate.".format(title, label, data["rejected"])
                )

            if data["failures"]:
                warnings.append(
                    "{}: {} hit {} daemon failures during the run; each one opens the "
                    "fail-open window, so part of the run was not inspected.".format(
                        title, label, data["failures"]
                    )
                )

    if warnings:
        lines += ["> [!WARNING]"] + ["> " + item for item in dedupe(warnings)] + [""]
    if notes:
        lines += ["> [!NOTE]"] + ["> " + item for item in dedupe(notes)] + [""]

    print("\n".join(lines))


if __name__ == "__main__":
    main()
