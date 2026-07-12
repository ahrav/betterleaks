#!/usr/bin/env python3
"""Analyze paired filesystem benchmark blocks without third-party packages."""

import argparse
import hashlib
import json
import math
import os
import random
import statistics
import tempfile
from statistics import NormalDist


SCHEMA_VERSION = 1


def load_json(path):
    with open(path, encoding="utf-8") as stream:
        return json.load(stream)


def load_jsonl(path):
    rows = []
    with open(path, encoding="utf-8") as stream:
        for line_number, line in enumerate(stream, 1):
            if not line.strip():
                continue
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError as error:
                raise ValueError(f"{path}:{line_number}: {error}") from error
    return rows


def file_sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        while chunk := stream.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def _beta_continued_fraction(a, b, x):
    max_iterations = 300
    epsilon = 3e-14
    floor = 1e-300
    qab = a + b
    qap = a + 1.0
    qam = a - 1.0
    c = 1.0
    d = 1.0 - qab * x / qap
    if abs(d) < floor:
        d = floor
    d = 1.0 / d
    result = d
    for iteration in range(1, max_iterations + 1):
        m2 = 2 * iteration
        numerator = iteration * (b - iteration) * x / ((qam + m2) * (a + m2))
        d = 1.0 + numerator * d
        if abs(d) < floor:
            d = floor
        c = 1.0 + numerator / c
        if abs(c) < floor:
            c = floor
        d = 1.0 / d
        result *= d * c
        numerator = -(a + iteration) * (qab + iteration) * x / ((a + m2) * (qap + m2))
        d = 1.0 + numerator * d
        if abs(d) < floor:
            d = floor
        c = 1.0 + numerator / c
        if abs(c) < floor:
            c = floor
        d = 1.0 / d
        delta = d * c
        result *= delta
        if abs(delta - 1.0) <= epsilon:
            return result
    raise ArithmeticError("incomplete beta did not converge")


def regularized_incomplete_beta(a, b, x):
    if not 0.0 <= x <= 1.0:
        raise ValueError("x must be in [0,1]")
    if x in (0.0, 1.0):
        return x
    front = math.exp(
        math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b)
        + a * math.log(x) + b * math.log1p(-x)
    )
    if x < (a + 1.0) / (a + b + 2.0):
        return front * _beta_continued_fraction(a, b, x) / a
    return 1.0 - front * _beta_continued_fraction(b, a, 1.0 - x) / b


def student_t_cdf(value, degrees_freedom):
    if degrees_freedom <= 0:
        raise ValueError("degrees_freedom must be positive")
    if value == 0:
        return 0.5
    x = degrees_freedom / (degrees_freedom + value * value)
    tail = 0.5 * regularized_incomplete_beta(degrees_freedom / 2.0, 0.5, x)
    return 1.0 - tail if value > 0 else tail


def student_t_ppf(probability, degrees_freedom):
    if not 0.0 < probability < 1.0:
        raise ValueError("probability must be in (0,1)")
    if probability == 0.5:
        return 0.0
    if probability < 0.5:
        return -student_t_ppf(1.0 - probability, degrees_freedom)
    low, high = 0.0, 1.0
    while student_t_cdf(high, degrees_freedom) < probability:
        high *= 2.0
        if high > 1e6:
            raise ArithmeticError("student t quantile did not bracket")
    for _ in range(100):
        middle = (low + high) / 2.0
        if student_t_cdf(middle, degrees_freedom) < probability:
            low = middle
        else:
            high = middle
    return (low + high) / 2.0


def percentile(sorted_values, probability):
    if not sorted_values:
        return None
    if len(sorted_values) == 1:
        return sorted_values[0]
    index = probability * (len(sorted_values) - 1)
    low = math.floor(index)
    high = math.ceil(index)
    if low == high:
        return sorted_values[low]
    weight = index - low
    return sorted_values[low] * (1.0 - weight) + sorted_values[high] * weight


def paired_ci(log_ratios, alpha):
    if not log_ratios:
        return None
    mean_log = statistics.fmean(log_ratios)
    result = {"point_ratio": math.exp(mean_log), "n_blocks": len(log_ratios)}
    if len(log_ratios) < 2:
        result.update({"lower_ratio": None, "upper_ratio": None, "log_sd": None})
        return result
    log_sd = statistics.stdev(log_ratios)
    critical = student_t_ppf(1.0 - alpha / 2.0, len(log_ratios) - 1)
    half_width = critical * log_sd / math.sqrt(len(log_ratios))
    result.update({
        "lower_ratio": math.exp(mean_log - half_width),
        "upper_ratio": math.exp(mean_log + half_width),
        "log_sd": log_sd,
        "critical_t": critical,
    })
    return result


def bootstrap_ci(log_ratios, alpha, resamples, rng):
    if not log_ratios:
        return None
    estimates = []
    for _ in range(resamples):
        estimates.append(math.exp(statistics.fmean(rng.choice(log_ratios) for _ in log_ratios)))
    estimates.sort()
    return {
        "lower_ratio": percentile(estimates, alpha / 2.0),
        "upper_ratio": percentile(estimates, 1.0 - alpha / 2.0),
        "resamples": resamples,
    }


def coefficient_of_variation(values):
    if len(values) < 2:
        return None
    mean = statistics.fmean(values)
    return statistics.stdev(values) / mean if mean else None


def metric_value(row, metric):
    if metric == "wall":
        value = row.get("wall_ns", 0)
    elif metric == "cpu":
        usage = row.get("wait_resource_usage") or {}
        value = usage.get("user_cpu_ns", 0) + usage.get("system_cpu_ns", 0)
    elif metric == "rss":
        procfs = row.get("procfs") or {}
        usage = row.get("wait_resource_usage") or {}
        value = max(procfs.get("peak_aggregate_rss_bytes", 0), usage.get("max_rss_bytes", 0))
    elif metric == "page_cache_harm":
        scanner = row.get("scanner_metrics") or {}
        resources = scanner.get("resources") or {}
        value = resources.get("page_cache_harm_bytes")
    else:
        raise ValueError(f"unknown metric {metric}")
    if value is None or value <= 0:
        return None
    return float(value)


def candidate_failures(rows):
    failures = {}
    for row in rows:
        if row.get("block_validity") != "valid":
            continue
        candidate = row.get("candidate_id")
        for entry in row.get("validity_ledger") or []:
            if entry.get("severity") == "error" and entry.get("owner") == "candidate":
                failures.setdefault(candidate, set()).add(entry.get("code", "unknown"))
    return {candidate: sorted(codes) for candidate, codes in failures.items()}


def classified_validity(ledger):
    validity = "valid"
    for entry in ledger or []:
        if entry.get("severity") != "error":
            continue
        if entry.get("owner") in ("infrastructure", "harness"):
            return "invalid_infrastructure"
        validity = "invalid_candidate"
    return validity


def valid_rows_by_block(rows):
    blocks = {}
    for row in rows:
        if row.get("block_validity") != "valid" or row.get("validity") != "valid":
            continue
        key = (row["stratum_id"], row["block_index"])
        candidate = row["candidate_id"]
        if candidate in blocks.setdefault(key, {}):
            raise ValueError(f"duplicate valid row for {key} candidate {candidate}")
        blocks[key][candidate] = row
    return blocks


def valid_sha256(value):
    return (
        isinstance(value, str)
        and len(value) == 64
        and all(character in "0123456789abcdef" for character in value)
    )


def normalized_artifacts(value):
    if not isinstance(value, list):
        return None
    normalized = []
    for item in value:
        if not isinstance(item, dict):
            return None
        path = item.get("path")
        digest = item.get("sha256")
        if not isinstance(path, str) or not path.startswith("/") or not valid_sha256(digest):
            return None
        normalized.append((os.path.normpath(path), digest))
    return tuple(sorted(normalized))


def config_paths_from_args(arguments):
    paths = []
    index = 0
    while index < len(arguments):
        argument = arguments[index]
        if argument in ("--config", "-c"):
            if index + 1 >= len(arguments) or not arguments[index + 1].strip():
                paths.append(None)
            else:
                index += 1
                paths.append(arguments[index])
        elif argument.startswith("--config=") or argument.startswith("-c="):
            paths.append(argument.split("=", 1)[1] or None)
        index += 1
    return paths


def schedule_sha256(blocks):
    canonical = []
    for block_index, sequence_id, candidates in blocks:
        canonical.append("\t" + "\t".join(
            [str(block_index), str(sequence_id), *candidates]
        ) + "\n")
    return hashlib.sha256("".join(canonical).encode()).hexdigest()


def experiment_integrity(plan, rows, blocks, failures, expected_plan_sha256=None):
    expected_candidates = {item["id"] for item in plan["candidates"]}
    expected_signatures = {
        item["id"]: item.get("metrics_candidate") for item in plan["candidates"]
    }
    strata_by_id = {item["id"]: item for item in plan["strata"]}
    expected_strata = set(strata_by_id)
    evidence_roles = {item["id"]: item["evidence_role"] for item in plan["strata"]}
    horizon = plan["fixed_horizon_blocks"]
    issues_by_code = {}

    def add_issue(code, message):
        issue = issues_by_code.setdefault(code, {"code": code, "count": 0, "examples": []})
        issue["count"] += 1
        if len(issue["examples"]) < 5:
            issue["examples"].append(message)

    pins_required = plan.get("phase") in ("pilot", "confirmation")
    expected_binary = plan.get("binary_sha256")
    if pins_required and not valid_sha256(expected_binary):
        add_issue("binary_pin_missing", "pilot and confirmation plans require binary_sha256")

    expected_artifacts = normalized_artifacts(plan.get("artifacts"))
    if pins_required and expected_artifacts is None:
        add_issue("artifact_pins_missing", "pilot and confirmation plans require an artifacts array")
        expected_artifacts = ()
    elif expected_artifacts is None:
        expected_artifacts = ()
    artifact_paths = [path for path, _ in expected_artifacts]
    if len(artifact_paths) != len(set(artifact_paths)):
        add_issue("duplicate_artifact_pin", "plan contains duplicate artifact paths")
    pinned_paths = set(artifact_paths)

    if pins_required:
        hooks = list(plan.get("pre_run_hooks") or [])
        for stratum in plan["strata"]:
            hooks.extend(stratum.get("prepare_hooks") or [])
        for hook in hooks:
            command = hook.get("command") or []
            executable = os.path.normpath(command[0]) if command else None
            if executable not in pinned_paths:
                add_issue(
                    "helper_pin_missing",
                    f"hook {hook.get('id')!r} executable {executable!r} is not pinned in artifacts",
                )

        argument_sets = [plan.get("common_args") or []]
        argument_sets.extend(item.get("args") or [] for item in plan["candidates"])
        argument_sets.extend(item.get("scanner_args") or [] for item in plan["strata"])
        argument_sets.extend(hook.get("command") or [] for hook in hooks)
        config_paths = []
        for arguments in argument_sets:
            config_paths.extend(config_paths_from_args(arguments))
        environments = [plan.get("environment") or {}]
        environments.extend(item.get("environment") or {} for item in plan["candidates"])
        environments.extend(hook.get("environment") or {} for hook in hooks)
        for environment in environments:
            for key in ("BETTERLEAKS_CONFIG", "GITLEAKS_CONFIG"):
                if environment.get(key):
                    config_paths.append(environment[key])
        for path in config_paths:
            normalized = os.path.normpath(path) if isinstance(path, str) and path.startswith("/") else None
            if normalized not in pinned_paths:
                add_issue("config_pin_missing", f"config {path!r} is not pinned in artifacts")

    observed_binary_digests = set()
    observed_manifest_digests = {stratum_id: set() for stratum_id in expected_strata}
    block_attempts = {}

    for index, row in enumerate(rows):
        if row.get("schema_version") != SCHEMA_VERSION:
            add_issue("result_schema_mismatch", f"row {index} has unsupported schema_version")
        if row.get("study_id") != plan["study_id"]:
            add_issue("study_id_mismatch", f"row {index} study_id does not match plan")
        if row.get("phase") != plan["phase"]:
            add_issue("phase_mismatch", f"row {index} phase does not match plan")
        expected_validity = classified_validity(row.get("validity_ledger"))
        if row.get("validity") != expected_validity:
            add_issue(
                "row_validity_mismatch",
                f"row {index} validity {row.get('validity')!r}, ledger classifies as {expected_validity!r}",
            )
        if expected_plan_sha256 is not None and row.get("plan_sha256") != expected_plan_sha256:
            add_issue("plan_digest_mismatch", f"row {index} plan_sha256 does not match plan bytes")
        row_binary = row.get("binary_sha256")
        observed_binary_digests.add(row_binary)
        if valid_sha256(expected_binary) and row_binary != expected_binary:
            add_issue("binary_digest_mismatch", f"row {index} binary_sha256 does not match plan")
        if pins_required and normalized_artifacts(row.get("artifacts")) != expected_artifacts:
            add_issue("artifact_pin_mismatch", f"row {index} artifacts do not match plan")
        if row.get("candidate_id") not in expected_candidates:
            add_issue("unexpected_candidate", f"row {index} has candidate {row.get('candidate_id')!r}")
        elif plan.get("require_scanner_metrics") is True:
            expected_signature = expected_signatures[row["candidate_id"]]
            scanner_metrics = row.get("scanner_metrics")
            if row.get("metrics_candidate_expected") != expected_signature:
                add_issue("metrics_candidate_mismatch", f"row {index} declared candidate signature does not match plan")
            if (
                row.get("validity") == "valid"
                and (
                    not isinstance(scanner_metrics, dict)
                    or scanner_metrics.get("candidate") != expected_signature
                )
            ):
                add_issue("scanner_candidate_mismatch", f"row {index} scanner signature does not match plan")
        if row.get("stratum_id") not in expected_strata:
            add_issue("unexpected_stratum", f"row {index} has stratum {row.get('stratum_id')!r}")
        else:
            stratum_id = row["stratum_id"]
            observed_manifest_digests[stratum_id].add(row.get("manifest_sha256"))
            if row.get("evidence_role") != evidence_roles[stratum_id]:
                add_issue("evidence_role_mismatch", f"row {index} evidence_role does not match plan")
            expected_manifest = strata_by_id[stratum_id].get("manifest_sha256")
            if valid_sha256(expected_manifest) and row.get("manifest_sha256") != expected_manifest:
                add_issue("manifest_digest_mismatch", f"row {index} manifest_sha256 does not match plan stratum")
        block_index = row.get("block_index")
        if not isinstance(block_index, int) or not 0 <= block_index < horizon:
            add_issue("block_index_out_of_range", f"row {index} has block_index {block_index!r}")
        elif row.get("stratum_id") in expected_strata:
            attempt = row.get("block_attempt")
            if not isinstance(attempt, int) or attempt < 0:
                add_issue("block_attempt_invalid", f"row {index} has block_attempt {attempt!r}")
            else:
                key = (row["stratum_id"], block_index)
                attempt_rows = block_attempts.setdefault(key, {}).setdefault(attempt, [])
                attempt_rows.append(row)

    infrastructure_replacements = 0
    for stratum_id in sorted(expected_strata):
        for block_index in range(horizon):
            attempts = block_attempts.get((stratum_id, block_index), {})
            indexes = sorted(attempts)
            if indexes and indexes != list(range(indexes[-1] + 1)):
                add_issue(
                    "block_attempt_sequence_invalid",
                    f"stratum {stratum_id} block {block_index} attempts {indexes} are not contiguous from zero",
                )
            valid_attempts = []
            for attempt in indexes:
                attempt_rows = attempts[attempt]
                states = {row.get("block_validity") for row in attempt_rows}
                if len(states) != 1 or next(iter(states)) not in ("valid", "invalid_infrastructure"):
                    add_issue(
                        "block_attempt_validity_inconsistent",
                        f"stratum {stratum_id} block {block_index} attempt {attempt} has states {sorted(map(str, states))}",
                    )
                    continue
                state = next(iter(states))
                if state == "valid":
                    valid_attempts.append(attempt)
                    continue
                infrastructure_replacements += 1
                if not any(row.get("validity") == "invalid_infrastructure" for row in attempt_rows):
                    add_issue(
                        "infrastructure_replacement_unjustified",
                        f"stratum {stratum_id} block {block_index} attempt {attempt} has no infrastructure-invalid row",
                    )
            if len(valid_attempts) > 1 or (valid_attempts and valid_attempts[0] != indexes[-1]):
                add_issue(
                    "block_attempt_sequence_invalid",
                    f"stratum {stratum_id} block {block_index} valid attempts {valid_attempts} are not the single final attempt",
                )
    replacement_cap = plan.get("max_infrastructure_replacement_blocks")
    if not isinstance(replacement_cap, int) or replacement_cap < 0:
        add_issue(
            "infrastructure_replacement_cap_missing",
            "plan has no valid max_infrastructure_replacement_blocks",
        )
    elif infrastructure_replacements > replacement_cap:
        add_issue(
            "infrastructure_replacement_cap_exceeded",
            f"observed {infrastructure_replacements} replacement blocks, cap is {replacement_cap}",
        )

    if len(observed_binary_digests) > 1:
        add_issue("binary_digest_inconsistent", "result rows contain more than one binary_sha256")
    for stratum_id, digests in observed_manifest_digests.items():
        if len(digests) > 1:
            add_issue("manifest_digest_inconsistent", f"stratum {stratum_id} contains more than one manifest_sha256")
        if pins_required and not valid_sha256(strata_by_id[stratum_id].get("manifest_sha256")):
            add_issue("manifest_pin_missing", f"stratum {stratum_id} has no manifest_sha256 pin")
        if pins_required and not valid_sha256(strata_by_id[stratum_id].get("schedule_sha256")):
            add_issue("schedule_pin_missing", f"stratum {stratum_id} has no schedule_sha256 pin")

    valid_block_rows = {}
    for row in rows:
        if row.get("block_validity") == "valid":
            valid_block_rows.setdefault((row.get("stratum_id"), row.get("block_index")), []).append(row)
    candidate_count = len(expected_candidates)
    expected_balance = horizon // candidate_count
    for stratum_id in sorted(expected_strata):
        position_counts = {candidate: [0] * candidate_count for candidate in expected_candidates}
        carryover_counts = {
            previous: {candidate: 0 for candidate in expected_candidates}
            for previous in expected_candidates
        }
        balanced_blocks = 0
        observed_schedule = []
        for block_index in range(horizon):
            block_rows = valid_block_rows.get((stratum_id, block_index), [])
            candidates = [row.get("candidate_id") for row in block_rows]
            positions = [row.get("sequence_position") for row in block_rows]
            sequence_ids = {row.get("sequence_id") for row in block_rows}
            if (
                len(block_rows) != candidate_count
                or set(candidates) != expected_candidates
                or set(positions) != set(range(candidate_count))
                or len(sequence_ids) != 1
            ):
                add_issue(
                    "invalid_balanced_block",
                    f"stratum {stratum_id} block {block_index} does not contain one complete ordered candidate sequence",
                )
                continue
            ordered = [None] * candidate_count
            for row in block_rows:
                ordered[row["sequence_position"]] = row["candidate_id"]
                position_counts[row["candidate_id"]][row["sequence_position"]] += 1
            for previous, candidate in zip(ordered, ordered[1:]):
                carryover_counts[previous][candidate] += 1
            observed_schedule.append((block_index, next(iter(sequence_ids)), ordered))
            balanced_blocks += 1
        expected_schedule = strata_by_id[stratum_id].get("schedule_sha256")
        if balanced_blocks == horizon and valid_sha256(expected_schedule):
            if schedule_sha256(observed_schedule) != expected_schedule:
                add_issue("schedule_digest_mismatch", f"stratum {stratum_id} rows do not match the pinned schedule")
        if plan["phase"] != "calibration" and balanced_blocks == horizon:
            for candidate in sorted(expected_candidates):
                if any(count != expected_balance for count in position_counts[candidate]):
                    add_issue("position_imbalance", f"stratum {stratum_id} candidate {candidate} positions are not balanced")
                for following in sorted(expected_candidates - {candidate}):
                    if carryover_counts[candidate][following] != expected_balance:
                        add_issue(
                            "carryover_imbalance",
                            f"stratum {stratum_id} transition {candidate}->{following} is not balanced",
                        )

    incomplete = {}
    for candidate in sorted(expected_candidates):
        if candidate in failures:
            continue
        missing = []
        missing_count = 0
        for stratum_id in sorted(expected_strata):
            for block_index in range(horizon):
                if candidate not in blocks.get((stratum_id, block_index), {}):
                    missing_count += 1
                    if len(missing) < 20:
                        missing.append({"stratum_id": stratum_id, "block_index": block_index})
        if missing_count:
            incomplete[candidate] = {"count": missing_count, "examples": missing}
    return {
        "issues": [issues_by_code[code] for code in sorted(issues_by_code)],
        "incomplete_candidates": incomplete,
    }


def paired_metric(blocks, stratum_id, candidate, baseline, metric):
    ratios = []
    baseline_values = []
    for (observed_stratum, _), candidates in sorted(blocks.items()):
        if observed_stratum != stratum_id:
            continue
        if candidate not in candidates or baseline not in candidates:
            continue
        candidate_value = metric_value(candidates[candidate], metric)
        baseline_value = metric_value(candidates[baseline], metric)
        if candidate_value is None or baseline_value is None:
            continue
        ratios.append(math.log(candidate_value / baseline_value))
        baseline_values.append(baseline_value)
    return ratios, baseline_values


def round_up(value, multiple):
    return int(math.ceil(value / multiple) * multiple)


def sample_size(log_sd, plan, alpha_per_comparison):
    if log_sd is None:
        return None
    analysis = plan["analysis"]
    effect = abs(math.log1p(-analysis["minimum_worthwhile_effect_pct"] / 100.0))
    z_alpha = NormalDist().inv_cdf(1.0 - alpha_per_comparison / 2.0)
    z_power = NormalDist().inv_cdf(analysis["power"])
    raw = ((z_alpha + z_power) * log_sd / effect) ** 2
    guarded_sd = log_sd * analysis["pilot_variance_inflation"]
    guarded = ((z_alpha + z_power) * guarded_sd / effect) ** 2
    candidate_count = len(plan["candidates"])
    cycle = candidate_count if candidate_count % 2 == 0 else 2 * candidate_count
    required = max(analysis["minimum_confirmation_blocks"], math.ceil(guarded))
    return {
        "paired_log_sd": log_sd,
        "variance_inflation": analysis["pilot_variance_inflation"],
        "raw_blocks": math.ceil(raw),
        "guarded_blocks": math.ceil(guarded),
        "balanced_cycle": cycle,
        "planned_blocks": round_up(required, cycle),
    }


def stratified_bootstrap(metric_logs, alpha, resamples, rng):
    usable = [values for values in metric_logs.values() if values]
    if not usable:
        return None
    point_log = statistics.fmean(statistics.fmean(values) for values in usable)
    estimates = []
    for _ in range(resamples):
        stratum_means = [statistics.fmean(rng.choice(values) for _ in values) for values in usable]
        estimates.append(math.exp(statistics.fmean(stratum_means)))
    estimates.sort()
    return {
        "point_ratio": math.exp(point_log),
        "lower_ratio": percentile(estimates, alpha / 2.0),
        "upper_ratio": percentile(estimates, 1.0 - alpha / 2.0),
        "strata": len(usable),
        "resamples": resamples,
    }


def analyze(plan, rows, expected_plan_sha256=None, expected_results_sha256=None):
    baseline = plan["baseline_candidate"]
    candidate_ids = [candidate["id"] for candidate in plan["candidates"] if candidate["id"] != baseline]
    comparisons = plan["analysis"]["winner_claims"] * max(1, len(candidate_ids))
    alpha_per_comparison = plan["analysis"]["alpha_familywise"] / comparisons
    blocks = valid_rows_by_block(rows)
    failures = candidate_failures(rows)
    integrity = experiment_integrity(plan, rows, blocks, failures, expected_plan_sha256)
    provenance_issue_codes = {
        "artifact_pin_mismatch", "artifact_pins_missing", "binary_digest_inconsistent",
        "binary_digest_mismatch", "binary_pin_missing", "config_pin_missing",
        "duplicate_artifact_pin", "helper_pin_missing", "manifest_digest_inconsistent",
        "manifest_digest_mismatch", "manifest_pin_missing", "schedule_digest_mismatch",
        "schedule_pin_missing",
    }
    provenance_pins_bound = not any(
        issue["code"] in provenance_issue_codes for issue in integrity["issues"]
    )
    baseline_usable = baseline not in failures and baseline not in integrity["incomplete_candidates"]
    candidate_signatures_bound = all(
        isinstance(item.get("metrics_candidate"), str)
        and bool(item["metrics_candidate"].strip())
        for item in plan["candidates"]
    )
    confirmation_guards_enabled = (
        plan.get("require_scanner_metrics") is True
        and all(not stratum.get("skip_live_guard", False) for stratum in plan["strata"])
        and candidate_signatures_bound
        and provenance_pins_bound
    )
    study_claim_eligible = (
        plan["phase"] == "confirmation"
        and not integrity["issues"]
        and baseline_usable
        and confirmation_guards_enabled
    )
    strata_by_id = {item["id"]: item for item in plan["strata"]}
    performance_strata_by_class = {}
    for stratum_id, stratum in strata_by_id.items():
        if stratum["evidence_role"] == "performance":
            performance_strata_by_class.setdefault(stratum["workload_class"], set()).add(stratum_id)
    resamples = plan["analysis"]["bootstrap_resamples"]
    seed = int(plan["seed"])
    stratum_results = []
    logs_by_class = {}

    for stratum_id, stratum in strata_by_id.items():
        if stratum["evidence_role"] != "performance":
            continue
        for candidate in candidate_ids:
            metrics = {}
            for metric in ("wall", "cpu", "rss", "page_cache_harm"):
                logs, baseline_values = paired_metric(blocks, stratum_id, candidate, baseline, metric)
                if not logs:
                    metrics[metric] = None
                    continue
                rng = random.Random(f"{seed}:{stratum_id}:{candidate}:{metric}")
                metrics[metric] = paired_ci(logs, alpha_per_comparison)
                metrics[metric]["bootstrap"] = bootstrap_ci(logs, alpha_per_comparison, resamples, rng)
                if metric == "wall":
                    metrics[metric]["baseline_cv"] = coefficient_of_variation(baseline_values)
                    metrics[metric]["sample_size"] = sample_size(metrics[metric].get("log_sd"), plan, alpha_per_comparison)
                logs_by_class.setdefault(stratum["workload_class"], {}).setdefault(candidate, {}).setdefault(metric, {})[stratum_id] = logs
            stratum_results.append({
                "stratum_id": stratum_id,
                "workload_class": stratum["workload_class"],
                "candidate_id": candidate,
                "disqualified": candidate in failures,
                "metrics": metrics,
            })

    class_results = []
    rules = plan["analysis"]
    mwe_ratio = 1.0 - rules["minimum_worthwhile_effect_pct"] / 100.0
    guard_ratio = 1.0 + rules["equivalence_margin_pct"] / 100.0
    resource_thresholds = {
        "cpu": 1.0 - rules["cpu_material_improvement_pct"] / 100.0,
        "rss": 1.0 - rules["rss_material_improvement_pct"] / 100.0,
        "page_cache_harm": 1.0 - rules["page_cache_material_improvement_pct"] / 100.0,
    }
    for workload_class, candidates in sorted(logs_by_class.items()):
        for candidate, metric_logs in sorted(candidates.items()):
            aggregates = {}
            for metric, by_stratum in metric_logs.items():
                rng = random.Random(f"{seed}:{workload_class}:{candidate}:{metric}:class")
                aggregates[metric] = stratified_bootstrap(by_stratum, alpha_per_comparison, resamples, rng)
                expected_metric_strata = performance_strata_by_class[workload_class]
                aggregates[metric]["complete"] = (
                    set(by_stratum) == expected_metric_strata
                    and all(len(values) == plan["fixed_horizon_blocks"] for values in by_stratum.values())
                )
            wall = aggregates.get("wall")
            wall_strata = [
                item["metrics"]["wall"] for item in stratum_results
                if item["workload_class"] == workload_class and item["candidate_id"] == candidate
                and item["metrics"]["wall"] is not None
            ]
            wall_complete = bool(wall) and wall["complete"]
            no_material_regression = wall_complete and all(
                item.get("upper_ratio") is not None and item["upper_ratio"] <= guard_ratio
                for item in wall_strata
            )
            performance_route = wall_complete and (
                wall["point_ratio"] <= mwe_ratio and wall["upper_ratio"] < 1.0
                and no_material_regression
            )
            resource_wins = []
            if wall_complete and wall["upper_ratio"] <= guard_ratio and no_material_regression:
                for metric, threshold in resource_thresholds.items():
                    aggregate = aggregates.get(metric)
                    if aggregate and aggregate["complete"] and aggregate["upper_ratio"] <= threshold:
                        resource_wins.append(metric)
            disqualified = candidate in failures
            claim_eligible = (
                study_claim_eligible
                and candidate not in integrity["incomplete_candidates"]
            )
            class_results.append({
                "workload_class": workload_class,
                "candidate_id": candidate,
                "claim_eligible": claim_eligible,
                "disqualified": disqualified,
                "failure_codes": failures.get(candidate, []),
                "metrics": aggregates,
                "no_material_regression": no_material_regression,
                "performance_route": claim_eligible and performance_route and not disqualified,
                "resource_route": claim_eligible and bool(resource_wins) and not disqualified,
                "resource_wins": resource_wins,
                "advances": claim_eligible and (performance_route or bool(resource_wins)) and not disqualified,
            })

    return {
        "schema_version": SCHEMA_VERSION,
        "study_id": plan["study_id"],
        "phase": plan["phase"],
        "plan_sha256": expected_plan_sha256,
        "results_sha256": expected_results_sha256,
        "claim_eligible": study_claim_eligible,
        "baseline_candidate": baseline,
        "familywise_alpha": plan["analysis"]["alpha_familywise"],
        "comparisons": comparisons,
        "alpha_per_comparison": alpha_per_comparison,
        "minimum_worthwhile_effect_pct": plan["analysis"]["minimum_worthwhile_effect_pct"],
        "confirmation_guards_enabled": confirmation_guards_enabled,
        "provenance_pins_bound": provenance_pins_bound,
        "candidate_signatures_bound": candidate_signatures_bound,
        "candidate_failures": failures,
        "experiment_integrity": integrity,
        "strata": stratum_results,
        "workload_classes": class_results,
    }


def write_json_atomic(path, value):
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".filescan-analysis-", dir=directory)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--plan", required=True)
    parser.add_argument("--results", required=True)
    parser.add_argument("--output")
    args = parser.parse_args()
    plan = load_json(args.plan)
    plan_sha256 = file_sha256(args.plan)
    results_sha256 = file_sha256(args.results)
    rows = load_jsonl(args.results)
    analysis = analyze(plan, rows, plan_sha256, results_sha256)
    if args.output:
        write_json_atomic(args.output, analysis)
    print(json.dumps(analysis, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
