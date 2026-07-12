import importlib.util
import math
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location(
    "filescan_analyze", Path(__file__).with_name("analyze.py")
)
ANALYZE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ANALYZE)

BINARY_SHA256 = "a" * 64
MANIFEST_SHA256 = "b" * 64


def synthetic_schedule_sha256():
    blocks = []
    for block in range(12):
        candidates = ["baseline", "candidate"] if block % 2 == 0 else ["candidate", "baseline"]
        blocks.append((block, block % 2, candidates))
    return ANALYZE.schedule_sha256(blocks)


def test_plan(phase="confirmation"):
    return {
        "study_id": "synthetic-v1",
        "phase": phase,
        "seed": 123,
        "binary_sha256": BINARY_SHA256,
        "artifacts": [],
        "fixed_horizon_blocks": 12,
        "max_infrastructure_replacement_blocks": 2,
        "require_scanner_metrics": True,
        "baseline_candidate": "baseline",
        "candidates": [
            {"id": "baseline", "metrics_candidate": "baseline/parallel"},
            {"id": "candidate", "metrics_candidate": "buffered/parallel"},
        ],
        "strata": [{
            "id": "source-tree",
            "workload_class": "many-small",
            "evidence_role": "performance",
            "manifest_sha256": MANIFEST_SHA256,
            "schedule_sha256": synthetic_schedule_sha256(),
        }],
        "analysis": {
            "minimum_worthwhile_effect_pct": 5.0,
            "alpha_familywise": 0.05,
            "power": 0.9,
            "winner_claims": 4,
            "pilot_variance_inflation": 1.5,
            "minimum_confirmation_blocks": 12,
            "equivalence_margin_pct": 2.0,
            "cpu_material_improvement_pct": 10.0,
            "rss_material_improvement_pct": 20.0,
            "page_cache_material_improvement_pct": 20.0,
            "bootstrap_resamples": 1000,
        },
    }


def result_row(block, candidate, ratio=1.0, candidate_error=False):
    baseline_wall = 100_000_000 + block * 1_000_000
    value = int(baseline_wall * ratio)
    ledger = []
    validity = "valid"
    if candidate_error:
        validity = "invalid_candidate"
        ledger.append({
            "code": "digest_mismatch",
            "severity": "error",
            "owner": "candidate",
        })
    baseline_first = block % 2 == 0
    position = 0 if (candidate == "baseline") == baseline_first else 1
    row = {
        "schema_version": 1,
        "study_id": "synthetic-v1",
        "phase": "confirmation",
        "binary_sha256": BINARY_SHA256,
        "artifacts": [],
        "manifest_sha256": MANIFEST_SHA256,
        "stratum_id": "source-tree",
        "evidence_role": "performance",
        "block_index": block,
        "block_attempt": 0,
        "sequence_id": block % 2,
        "sequence_position": position,
        "candidate_id": candidate,
        "metrics_candidate_expected": "baseline/parallel" if candidate == "baseline" else "buffered/parallel",
        "block_validity": "valid",
        "validity": validity,
        "wall_ns": value,
        "wait_resource_usage": {
            "user_cpu_ns": value // 2,
            "system_cpu_ns": value // 4,
            "max_rss_bytes": value,
        },
        "procfs": {"peak_aggregate_rss_bytes": value},
        "scanner_metrics": {
            "candidate": "baseline/parallel" if candidate == "baseline" else "buffered/parallel",
            "resources": {"page_cache_harm_bytes": value},
        },
        "validity_ledger": ledger,
    }
    if candidate_error:
        row["scanner_metrics"] = None
    return row


def paired_rows(candidate_error=False):
    rows = []
    for block in range(12):
        rows.append(result_row(block, "baseline"))
        rows.append(result_row(
            block,
            "candidate",
            ratio=0.8 + (block % 3 - 1) * 0.002,
            candidate_error=candidate_error and block == 0,
        ))
    return rows


class DistributionTests(unittest.TestCase):
    def test_student_t_quantile(self):
        self.assertAlmostEqual(
            ANALYZE.student_t_ppf(0.975, 10),
            2.228138852,
            places=8,
        )

    def test_paired_log_ratio_interval(self):
        ratios = [0.80, 0.82, 0.79, 0.81, 0.805, 0.815]
        interval = ANALYZE.paired_ci([math.log(value) for value in ratios], 0.05)
        expected = math.exp(sum(math.log(value) for value in ratios) / len(ratios))
        self.assertAlmostEqual(interval["point_ratio"], expected, places=12)
        self.assertLess(interval["lower_ratio"], interval["point_ratio"])
        self.assertGreater(interval["upper_ratio"], interval["point_ratio"])
        self.assertEqual(interval["n_blocks"], len(ratios))

    def test_pilot_sizing_is_guarded_and_balanced(self):
        sizing = ANALYZE.sample_size(0.08, test_plan("pilot"), 0.0125)
        self.assertGreaterEqual(sizing["guarded_blocks"], sizing["raw_blocks"])
        self.assertGreaterEqual(sizing["planned_blocks"], 12)
        self.assertEqual(sizing["planned_blocks"] % 2, 0)


class WinnerRuleTests(unittest.TestCase):
    def test_analysis_binds_exact_plan_and_results_digests(self):
        result = ANALYZE.analyze(test_plan(), paired_rows(), "c" * 64, "d" * 64)
        self.assertEqual(result["plan_sha256"], "c" * 64)
        self.assertEqual(result["results_sha256"], "d" * 64)

    def test_confirmation_candidate_advances(self):
        result = ANALYZE.analyze(test_plan(), paired_rows())
        winner = result["workload_classes"][0]
        self.assertTrue(result["claim_eligible"])
        self.assertTrue(winner["performance_route"])
        self.assertTrue(winner["advances"])
        self.assertFalse(winner["disqualified"])
        self.assertLess(winner["metrics"]["wall"]["upper_ratio"], 1.0)

    def test_pilot_never_advances(self):
        rows = paired_rows()
        for row in rows:
            row["phase"] = "pilot"
        result = ANALYZE.analyze(test_plan("pilot"), rows)
        winner = result["workload_classes"][0]
        self.assertFalse(result["claim_eligible"])
        self.assertFalse(winner["performance_route"])
        self.assertFalse(winner["resource_route"])
        self.assertFalse(winner["advances"])

    def test_incomplete_confirmation_never_advances(self):
        result = ANALYZE.analyze(test_plan(), paired_rows()[:-1])
        winner = result["workload_classes"][0]
        self.assertIn("candidate", result["experiment_integrity"]["incomplete_candidates"])
        self.assertFalse(winner["claim_eligible"])
        self.assertFalse(winner["advances"])

    def test_plan_digest_mismatch_never_advances(self):
        result = ANALYZE.analyze(test_plan(), paired_rows(), "a" * 64)
        winner = result["workload_classes"][0]
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("plan_digest_mismatch", codes)
        self.assertFalse(winner["claim_eligible"])
        self.assertFalse(winner["advances"])

    def test_binary_digest_mismatch_never_advances(self):
        rows = paired_rows()
        rows[0]["binary_sha256"] = "c" * 64
        result = ANALYZE.analyze(test_plan(), rows)
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("binary_digest_mismatch", codes)
        self.assertIn("binary_digest_inconsistent", codes)
        self.assertFalse(result["provenance_pins_bound"])
        self.assertFalse(result["claim_eligible"])

    def test_manifest_digest_mismatch_never_advances(self):
        rows = paired_rows()
        rows[0]["manifest_sha256"] = "c" * 64
        result = ANALYZE.analyze(test_plan(), rows)
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("manifest_digest_mismatch", codes)
        self.assertIn("manifest_digest_inconsistent", codes)
        self.assertFalse(result["claim_eligible"])

    def test_missing_or_mismatched_artifact_pins_never_advance(self):
        plan = test_plan()
        del plan["artifacts"]
        result = ANALYZE.analyze(plan, paired_rows())
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("artifact_pins_missing", codes)
        self.assertFalse(result["claim_eligible"])

        rows = paired_rows()
        rows[0]["artifacts"] = [{"path": "/tmp/other", "sha256": "c" * 64}]
        result = ANALYZE.analyze(test_plan(), rows)
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("artifact_pin_mismatch", codes)
        self.assertFalse(result["claim_eligible"])

    def test_missing_helper_and_config_pins_never_advance(self):
        plan = test_plan()
        plan["pre_run_hooks"] = [{"id": "warm", "command": ["/tmp/cachewarm"]}]
        plan["common_args"] = ["--config=/tmp/config.toml"]
        result = ANALYZE.analyze(plan, paired_rows())
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("helper_pin_missing", codes)
        self.assertIn("config_pin_missing", codes)
        self.assertFalse(result["claim_eligible"])

    def test_balanced_but_unpinned_schedule_never_advances(self):
        rows = paired_rows()
        for row in rows:
            if row["block_index"] in (0, 1):
                row["sequence_position"] = 1 - row["sequence_position"]
                row["sequence_id"] = 1 - row["sequence_id"]
        result = ANALYZE.analyze(test_plan(), rows)
        codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("schedule_digest_mismatch", codes)
        self.assertNotIn("position_imbalance", codes)
        self.assertNotIn("carryover_imbalance", codes)
        self.assertFalse(result["claim_eligible"])

    def test_candidate_owned_error_disqualifies(self):
        result = ANALYZE.analyze(test_plan(), paired_rows(candidate_error=True))
        winner = result["workload_classes"][0]
        issue_codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertTrue(result["claim_eligible"])
        self.assertNotIn("scanner_candidate_mismatch", issue_codes)
        self.assertTrue(winner["disqualified"])
        self.assertIn("digest_mismatch", winner["failure_codes"])
        self.assertFalse(winner["advances"])

    def test_replaced_infrastructure_attempt_without_metrics_is_eligible(self):
        rows = paired_rows()
        failed_attempt = result_row(0, "baseline")
        failed_attempt["block_attempt"] = 0
        failed_attempt["validity"] = "invalid_infrastructure"
        failed_attempt["block_validity"] = "invalid_infrastructure"
        failed_attempt["scanner_metrics"] = None
        failed_attempt["validity_ledger"] = [{
            "code": "procfs_sampling_failed",
            "severity": "error",
            "owner": "infrastructure",
        }]
        for row in rows:
            if row["block_index"] == 0:
                row["block_attempt"] = 1
        rows.insert(0, failed_attempt)

        result = ANALYZE.analyze(test_plan(), rows)
        issue_codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertTrue(result["claim_eligible"])
        self.assertNotIn("scanner_candidate_mismatch", issue_codes)
        self.assertTrue(result["workload_classes"][0]["advances"])

    def test_infrastructure_replacement_cap_is_rechecked(self):
        rows = paired_rows()
        failed_attempts = []
        for block in range(3):
            for row in rows:
                if row["block_index"] == block:
                    row["block_attempt"] = 1
            failed = result_row(block, "baseline")
            failed["validity"] = "invalid_infrastructure"
            failed["block_validity"] = "invalid_infrastructure"
            failed["scanner_metrics"] = None
            failed["validity_ledger"] = [{
                "code": "procfs_sampling_failed",
                "severity": "error",
                "owner": "infrastructure",
            }]
            failed_attempts.append(failed)

        result = ANALYZE.analyze(test_plan(), [*failed_attempts, *rows])
        issue_codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("infrastructure_replacement_cap_exceeded", issue_codes)
        self.assertFalse(result["claim_eligible"])

    def test_recorded_validity_must_match_ledger_ownership(self):
        rows = paired_rows()
        rows[0]["validity_ledger"] = [{
            "code": "procfs_sampling_failed",
            "severity": "error",
            "owner": "infrastructure",
        }]
        result = ANALYZE.analyze(test_plan(), rows)
        issue_codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("row_validity_mismatch", issue_codes)
        self.assertFalse(result["claim_eligible"])

    def test_partial_resource_metric_cannot_win_multi_stratum_class(self):
        plan = test_plan()
        second = dict(plan["strata"][0])
        second["id"] = "second-tree"
        plan["strata"].append(second)

        rows = []
        for stratum_id in ("source-tree", "second-tree"):
            for block in range(12):
                baseline = result_row(block, "baseline")
                candidate = result_row(block, "candidate", ratio=1.0)
                baseline["stratum_id"] = stratum_id
                candidate["stratum_id"] = stratum_id
                if stratum_id == "source-tree":
                    candidate["procfs"]["peak_aggregate_rss_bytes"] //= 2
                    candidate["wait_resource_usage"]["max_rss_bytes"] //= 2
                else:
                    for row in (baseline, candidate):
                        row["procfs"]["peak_aggregate_rss_bytes"] = 0
                        row["wait_resource_usage"]["max_rss_bytes"] = 0
                rows.extend((baseline, candidate))

        result = ANALYZE.analyze(plan, rows)
        decision = result["workload_classes"][0]
        self.assertTrue(result["claim_eligible"])
        self.assertFalse(decision["metrics"]["rss"]["complete"])
        self.assertNotIn("rss", decision["resource_wins"])
        self.assertFalse(decision["resource_route"])
        self.assertFalse(decision["advances"])

    def test_invalid_attempt_still_checks_plan_expected_signature(self):
        rows = paired_rows()
        rows[0]["validity"] = "invalid_infrastructure"
        rows[0]["block_validity"] = "invalid_infrastructure"
        rows[0]["scanner_metrics"] = None
        rows[0]["metrics_candidate_expected"] = "wrong/parallel"
        result = ANALYZE.analyze(test_plan(), rows)
        issue_codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertIn("metrics_candidate_mismatch", issue_codes)
        self.assertNotIn("scanner_candidate_mismatch", issue_codes)

    def test_baseline_failure_without_metrics_makes_baseline_unusable(self):
        rows = paired_rows()
        baseline_failure = rows[0]
        baseline_failure["validity"] = "invalid_candidate"
        baseline_failure["scanner_metrics"] = None
        baseline_failure["validity_ledger"] = [{
            "code": "scanner_metrics_missing",
            "severity": "error",
            "owner": "candidate",
        }]

        result = ANALYZE.analyze(test_plan(), rows)
        issue_codes = {item["code"] for item in result["experiment_integrity"]["issues"]}
        self.assertFalse(result["claim_eligible"])
        self.assertIn("baseline", result["candidate_failures"])
        self.assertNotIn("scanner_candidate_mismatch", issue_codes)
        self.assertFalse(result["workload_classes"][0]["claim_eligible"])
        self.assertFalse(result["workload_classes"][0]["advances"])

    def test_confirmation_without_metrics_guard_never_advances(self):
        plan = test_plan()
        plan["require_scanner_metrics"] = False
        result = ANALYZE.analyze(plan, paired_rows())
        self.assertFalse(result["confirmation_guards_enabled"])
        self.assertFalse(result["claim_eligible"])
        self.assertFalse(result["workload_classes"][0]["advances"])

    def test_confirmation_with_skipped_live_guard_never_advances(self):
        plan = test_plan()
        plan["strata"][0]["skip_live_guard"] = True
        result = ANALYZE.analyze(plan, paired_rows())
        self.assertFalse(result["confirmation_guards_enabled"])
        self.assertFalse(result["claim_eligible"])
        self.assertFalse(result["workload_classes"][0]["advances"])

    def test_confirmation_with_unbound_candidate_never_advances(self):
        plan = test_plan()
        del plan["candidates"][1]["metrics_candidate"]
        result = ANALYZE.analyze(plan, paired_rows())
        self.assertFalse(result["candidate_signatures_bound"])
        self.assertFalse(result["claim_eligible"])
        self.assertFalse(result["workload_classes"][0]["advances"])


if __name__ == "__main__":
    unittest.main()
