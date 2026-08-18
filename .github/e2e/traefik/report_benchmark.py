#!/usr/bin/env python3
"""Renders the oha result files written by run-benchmark.sh as a markdown report."""
import json
import os
import sys

SCENARIOS = [
    ("get", "GET /", "no request body"),
    ("post", "POST /form", "with request body inspection"),
]


def load(result_dir, name):
    with open(os.path.join(result_dir, name + ".json")) as handle:
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
    }


def pct_delta(before, after):
    """Relative change from before to after, as a signed percentage string."""
    if before is None or after is None or before == 0:
        return "n/a"
    return "{:+.1f}%".format((after - before) / before * 100)


def fmt(value, unit=""):
    if value is None:
        return "n/a"
    return "{:.2f}{}".format(value, unit)


def main():
    result_dir, requests, concurrency = sys.argv[1], sys.argv[2], sys.argv[3]

    lines = [
        "## traefik attachment benchmark",
        "",
        "Same traefik instance and same backend on both entrypoints; the only "
        "difference is the open-appsec middleware, so the delta is the cost of "
        "inspection. The agent was verified to be actively inspecting (an attack "
        "request returned 403) before measuring.",
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

    degraded = []
    saturated = []
    for key, title, note in SCENARIOS:
        before = load(result_dir, key + "-baseline")
        after = load(result_dir, key + "-appsec")

        lines += [
            "### {} ({})".format(title, note),
            "",
            "| Metric | Before (no attachment) | After (open-appsec) | Change |",
            "| --- | ---: | ---: | ---: |",
            "| Throughput | {} req/s | {} req/s | {} |".format(
                fmt(before["rps"]), fmt(after["rps"]), pct_delta(before["rps"], after["rps"])
            ),
        ]
        for label, metric in (
            ("Latency mean", "mean"),
            ("Latency p50", "p50"),
            ("Latency p95", "p95"),
            ("Latency p99", "p99"),
        ):
            lines.append(
                "| {} | {} | {} | {} |".format(
                    label,
                    fmt(before[metric], " ms"),
                    fmt(after[metric], " ms"),
                    pct_delta(before[metric], after[metric]),
                )
            )
        lines += [
            "| Success rate | {:.1%} | {:.1%} | |".format(
                before["success_rate"], after["success_rate"]
            ),
            "",
        ]

        if after["success_rate"] < 1.0:
            degraded.append(
                "{}: success rate with the attachment is {:.1%}".format(title, after["success_rate"])
            )

        # Once the agent's inspection pipeline saturates, tail latency grows by
        # orders of magnitude while the mean stays reasonable. Call that out so
        # a saturated run is not misread as a per-request cost.
        if (
            before["p99"] is not None
            and after["p99"] is not None
            and before["p99"] > 0
            and after["p99"] / before["p99"] >= 10
        ):
            saturated.append(
                "{}: p99 is {:.0f}x the baseline ({} vs {}), which means inspection is "
                "saturated at this concurrency rather than costing that much per "
                "request; lower the concurrency or raise the attachment worker count "
                "to measure the per-request cost.".format(
                    title,
                    after["p99"] / before["p99"],
                    fmt(after["p99"], " ms"),
                    fmt(before["p99"], " ms"),
                )
            )

    if degraded:
        lines += ["> [!WARNING]"] + ["> " + item for item in degraded] + [""]
    if saturated:
        lines += ["> [!NOTE]"] + ["> " + item for item in saturated] + [""]

    print("\n".join(lines))


if __name__ == "__main__":
    main()
