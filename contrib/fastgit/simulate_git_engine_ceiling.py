#!/usr/bin/env python3
"""Sensitivity model for replacing `git log -p` with an embedded Git engine.

This is not a benchmark of code that does not exist.  It models a finite stream
of file-diff jobs flowing through two bounded stages:

    Git object/tree/diff production -> detector work

Producer workers remain backpressured until a detector slot accepts their job,
matching betterleaks' bounded semgroup handoff more closely than an unbounded
queue.  The default workload uses checked-in empirical patch/added-byte
weights; `--synthetic` uses correlated lognormal weights for sensitivity runs.
"""

from __future__ import annotations

import argparse
import heapq
import math
import random
import statistics
from collections import deque
from dataclasses import dataclass
from pathlib import Path
from typing import Callable


class Sim:
    """Tiny deterministic discrete-event core derived from simple-simulation."""

    def __init__(self) -> None:
        self.now = 0.0
        self._events: list[tuple[float, int, Callable[[], None]]] = []
        self._seq = 0

    def after(self, delay: float, callback: Callable[[], None]) -> None:
        heapq.heappush(self._events, (self.now + delay, self._seq, callback))
        self._seq += 1

    def run(self) -> None:
        while self._events:
            self.now, _, callback = heapq.heappop(self._events)
            callback()


class Resource:
    """FIFO pool whose waiters represent pipeline backpressure."""

    def __init__(self, capacity: int) -> None:
        self.capacity = capacity
        self.in_use = 0
        self.waiting: deque[Callable[[], None]] = deque()

    def acquire(self, callback: Callable[[], None]) -> None:
        if self.in_use < self.capacity:
            self.in_use += 1
            callback()
        else:
            self.waiting.append(callback)

    def release(self) -> None:
        if self.waiting:
            self.waiting.popleft()()
        else:
            self.in_use -= 1


@dataclass(frozen=True)
class Scenario:
    name: str
    producer_ms: float
    detector_ms: float


def _normalized_lognormal(rng: random.Random, jobs: int, sigma: float) -> list[float]:
    if sigma == 0:
        return [1.0 / jobs] * jobs
    raw = [rng.lognormvariate(-0.5 * sigma * sigma, sigma) for _ in range(jobs)]
    total = sum(raw)
    return [value / total for value in raw]


def load_work(path: Path) -> list[tuple[float, float]]:
    """Load `<patch_bytes> <added_bytes>` or `<commit> <patch> <added>` rows."""
    work: list[tuple[float, float]] = []
    for line_number, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        fields = line.split()
        if len(fields) == 2:
            patch_bytes, added_bytes = fields
        elif len(fields) == 3:
            _, patch_bytes, added_bytes = fields
        else:
            raise ValueError(f"{path}:{line_number}: expected 2 or 3 integers")
        work.append((float(patch_bytes), float(added_bytes)))
    if not work or sum(patch for patch, _ in work) <= 0:
        raise ValueError(f"{path}: no producer work")
    return work


def _makespan(weights: list[float], slots: int) -> float:
    """FIFO list-scheduling makespan for normalized unit work."""
    available = [0.0] * slots
    heapq.heapify(available)
    for weight in weights:
        start = heapq.heappop(available)
        heapq.heappush(available, start + weight)
    return max(available)


def _calibrate(weights: list[float], isolated_ms: float, slots: int) -> list[float]:
    """Scale job weights so this finite stage alone completes in isolated_ms."""
    if isolated_ms == 0:
        return [0.0] * len(weights)
    unit_makespan = _makespan(weights, slots)
    return [weight * isolated_ms / unit_makespan for weight in weights]


def run_pipeline(
    scenario: Scenario,
    producer_speedup: float,
    *,
    jobs: int,
    producer_slots: int,
    detector_slots: int,
    sigma: float,
    seed: int,
    empirical_work: list[tuple[float, float]] | None = None,
) -> float:
    """Return finite-workload completion time in milliseconds.

    `producer_ms` and `detector_ms` are the target isolated elapsed times for
    their finite stages at the configured slot counts. Per-job weights are
    calibrated so each stage reproduces that target before composition. In
    synthetic mode, a shared size component makes large diffs expensive in
    both stages while independent noise prevents perfect correlation.
    """
    if empirical_work is not None:
        jobs = len(empirical_work)
        patch_total = sum(patch for patch, _ in empirical_work)
        added_total = sum(added for _, added in empirical_work)
        producer_weights = [patch / patch_total for patch, _ in empirical_work]
        detector_weights = (
            [added / added_total for _, added in empirical_work]
            if added_total
            else [1.0 / jobs] * jobs
        )
    else:
        rng = random.Random(seed)
        shared = _normalized_lognormal(rng, jobs, sigma)
        producer_noise = _normalized_lognormal(rng, jobs, sigma * 0.35)
        detector_noise = _normalized_lognormal(rng, jobs, sigma * 0.35)
        producer_weights = [0.8 * shared[i] + 0.2 * producer_noise[i] for i in range(jobs)]
        detector_weights = [0.8 * shared[i] + 0.2 * detector_noise[i] for i in range(jobs)]

    producer_work = [
        work / producer_speedup
        for work in _calibrate(producer_weights, scenario.producer_ms, producer_slots)
    ]
    detector_work = _calibrate(detector_weights, scenario.detector_ms, detector_slots)

    sim = Sim()
    producers = Resource(producer_slots)
    detectors = Resource(detector_slots)

    def submit(job: int) -> None:
        def start_producing() -> None:
            def produced() -> None:
                # Hold the producer slot until the bounded detector stage
                # accepts the job. This models scheduler backpressure.
                def start_detecting() -> None:
                    producers.release()

                    def detected() -> None:
                        detectors.release()

                    sim.after(detector_work[job], detected)

                detectors.acquire(start_detecting)

            sim.after(producer_work[job], produced)

        producers.acquire(start_producing)

    for job in range(jobs):
        submit(job)
    sim.run()
    return sim.now


def median_wall(scenario: Scenario, speedup: float, args: argparse.Namespace) -> float:
    if args.empirical_work is not None:
        return run_pipeline(
            scenario,
            speedup,
            jobs=args.jobs,
            producer_slots=args.producer_slots,
            detector_slots=args.detector_slots,
            sigma=args.sigma,
            seed=0,
            empirical_work=args.empirical_work,
        )
    samples = [
        run_pipeline(
            scenario,
            speedup,
            jobs=args.jobs,
            producer_slots=args.producer_slots,
            detector_slots=args.detector_slots,
            sigma=args.sigma,
            seed=seed,
            empirical_work=args.empirical_work,
        )
        for seed in range(args.seeds)
    ]
    return statistics.median(samples)


def validate(args: argparse.Namespace) -> None:
    one = Scenario("one-job", 20.0, 30.0)
    assert math.isclose(
        run_pipeline(
            one,
            1.0,
            jobs=1,
            producer_slots=1,
            detector_slots=1,
            sigma=0,
            seed=0,
            empirical_work=None,
        ),
        50.0,
    )

    producer_only = Scenario("producer-only", 100.0, 0.0)
    baseline = run_pipeline(
        producer_only, 1.0, jobs=1000, producer_slots=10, detector_slots=40, sigma=0, seed=0,
        empirical_work=None,
    )
    twice_as_fast = run_pipeline(
        producer_only, 2.0, jobs=1000, producer_slots=10, detector_slots=40, sigma=0, seed=0,
        empirical_work=None,
    )
    assert math.isclose(baseline / twice_as_fast, 2.0, rel_tol=1e-9)

    detector_only = Scenario("detector-only", 0.0, 100.0)
    slow = run_pipeline(
        detector_only, 1.0, jobs=1000, producer_slots=10, detector_slots=40, sigma=0, seed=0,
        empirical_work=None,
    )
    fast = run_pipeline(
        detector_only, 100.0, jobs=1000, producer_slots=10, detector_slots=40, sigma=0, seed=0,
        empirical_work=None,
    )
    assert math.isclose(slow, fast, rel_tol=1e-9)

    # For a zero-variance finite pipeline, completion must lie between the
    # slower stage and fully serialized sum, up to one job of drain time.
    balanced = Scenario("balanced", 100.0, 100.0)
    wall = run_pipeline(
        balanced, 1.0, jobs=1000, producer_slots=10, detector_slots=40, sigma=0, seed=0,
        empirical_work=None,
    )
    assert 100.0 <= wall <= 200.2


def print_sweep(scenarios: list[Scenario], args: argparse.Namespace) -> None:
    speedups = [1.0, 1.25, 1.5, 2.0, 3.0, 4.0, 6.0, 10.0, 20.0, 1000.0]
    header = "scenario".ljust(22) + "".join(f"{s:>9g}x" for s in speedups)
    print(header)
    print("-" * len(header))
    for scenario in scenarios:
        baseline = median_wall(scenario, 1.0, args)
        results = [baseline / median_wall(scenario, speedup, args) for speedup in speedups]
        print(scenario.name.ljust(22) + "".join(f"{result:>10.3f}" for result in results))
        print(
            f"  modeled baseline={baseline:.1f} ms; "
            f"producer={scenario.producer_ms:.0f} ms, detector={scenario.detector_ms:.0f} ms"
        )


def print_hard_ceilings(args: argparse.Namespace) -> None:
    print("conditional source-stage Amdahl bounds (assumes no shared-core relief or fusion)")
    source_fraction = args.producer_ms / args.full_ms
    for speedup in [1.25, 1.36, 1.5, 2.0, 2.58, 3.0, 4.0, 6.0, 10.0, math.inf]:
        removable = source_fraction if math.isinf(speedup) else source_fraction * (1.0 - 1.0 / speedup)
        label = "infinite" if math.isinf(speedup) else f"{speedup:g}x"
        print(f"  engine={label:>8}: max E2E reduction={100.0 * removable:>5.2f}%")

    print("\nconditional SPDK-only bounds within the source stage")
    for storage_fraction in [0.1, 0.3, 0.6]:
        reductions = []
        for storage_speedup in [1.02, 1.096]:
            reduction = source_fraction * storage_fraction * (1.0 - 1.0 / storage_speedup)
            reductions.append(f"{100.0 * reduction:.3f}%")
        print(
            f"  storage share={storage_fraction:>3.0%}: "
            f"QD1-like 1.02x={reductions[0]}, high-QD 1.096x={reductions[1]}"
        )


def print_variance_sensitivity(args: argparse.Namespace) -> None:
    scenario = Scenario("balanced", args.detector_ms, args.detector_ms)
    print("\nlarge-diff sensitivity at 4x producer speed (balanced stages)")
    for sigma in [0.0, 0.6, 1.2, 1.8]:
        varied = argparse.Namespace(**vars(args))
        varied.sigma = sigma
        varied.empirical_work = None
        baseline = median_wall(scenario, 1.0, varied)
        improved = median_wall(scenario, 4.0, varied)
        print(f"  sigma={sigma:>3.1f}: wall speedup={baseline / improved:.3f}x")


def print_candidate_cache_sensitivity(args: argparse.Namespace) -> None:
    """Show an intentionally optimistic cross-stage fusion sensitivity.

    Reuse is applied to the entire detector-stage budget as though detector
    cost were byte-linear and every repeated byte avoided all detector work.
    This is an upper-envelope experiment, not an expected cache hit or speedup.
    """
    current = Scenario("current", args.producer_ms, args.detector_ms)
    baseline = median_wall(current, 1.0, args)
    print("\noptimistic detector-candidate reuse sensitivity")
    print("  (assumes repeated-byte fraction removes the same detector-time fraction)")
    print("reuse".ljust(10) + "".join(f"{speedup:>12g}x engine" for speedup in [1.0, 2.0, 4.0, 1000.0]))
    for reuse in [0.0, 0.25, 0.479]:
        fused = Scenario(
            f"reuse {reuse:.1%}",
            args.producer_ms,
            args.detector_ms * (1.0 - reuse),
        )
        results = [baseline / median_wall(fused, speedup, args) for speedup in [1.0, 2.0, 4.0, 1000.0]]
        print(f"{reuse:>7.1%}   " + "".join(f"{result:>12.3f}" for result in results))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--jobs", type=int, default=3461)
    parser.add_argument("--producer-slots", type=int, default=10)
    parser.add_argument("--detector-slots", type=int, default=40)
    parser.add_argument("--producer-ms", type=float, default=652.0)
    # 3.36 s makes the empirical finite-workload pipeline reproduce the
    # measured 3.9575 s scan. The unobserved detector-only stage is uncertain;
    # sweep 3.305..3.958 s when testing sensitivity.
    parser.add_argument("--detector-ms", type=float, default=3360.0)
    parser.add_argument("--full-ms", type=float, default=3957.5)
    # A moderate synthetic fallback. The checked-in writeup uses empirical
    # per-file weights; larger values are swept below to expose sensitivity to
    # straggling large diffs rather than pretending this guess is measured.
    parser.add_argument("--sigma", type=float, default=0.6)
    parser.add_argument("--seeds", type=int, default=11)
    parser.add_argument(
        "--work-file",
        type=Path,
        default=Path(__file__).with_name("betterleaks_file_work_20260710.txt"),
        help="empirical per-file rows: <patch_bytes> <added_bytes>",
    )
    parser.add_argument(
        "--synthetic",
        action="store_true",
        help="ignore --work-file and use the synthetic lognormal workload",
    )
    args = parser.parse_args()
    args.empirical_work = None if args.synthetic else load_work(args.work_file)

    validate(args)
    print("validation: PASS")
    if args.empirical_work:
        print(f"workload: {len(args.empirical_work)} empirical file diffs from {args.work_file}")
    else:
        print(f"workload: {args.jobs} synthetic correlated lognormal file diffs")
    print("values are end-to-end wall-speedup, not producer-speedup\n")
    print_hard_ceilings(args)
    print()
    scenarios = [
        Scenario("current stage budgets", args.producer_ms, args.detector_ms),
        Scenario("balanced stages", args.detector_ms, args.detector_ms),
        Scenario("producer 2x detector", args.detector_ms * 2, args.detector_ms),
        Scenario("producer 4x detector", args.detector_ms * 4, args.detector_ms),
    ]
    print_sweep(scenarios, args)
    print_variance_sensitivity(args)
    print_candidate_cache_sensitivity(args)


if __name__ == "__main__":
    main()
