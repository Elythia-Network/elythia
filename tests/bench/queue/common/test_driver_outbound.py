"""Tests for verified-delivery throughput reporting."""

from __future__ import annotations

import importlib
import json
import os
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from delivery_result import classify_delivery, throughput_result


def load_driver():
    """Load the driver without requiring its network-only runtime packages."""
    httpx_stub = types.ModuleType("httpx")
    httpx_stub.post = lambda *args, **kwargs: None
    queue_probe_stub = types.ModuleType("queue_probe")
    queue_probe_stub.DriverKind = str
    queue_probe_stub.make_probe = lambda *args, **kwargs: None
    environment = {
        "REDIS_MKQ_HOST": "redis-mkq",
        "REDIS_TS_HOST": "redis-ts",
    }
    with patch.dict(os.environ, environment), patch.dict(
        sys.modules,
        {"httpx": httpx_stub, "queue_probe": queue_probe_stub},
    ):
        return importlib.import_module("driver_outbound")


driver = load_driver()


class ImmediateProbe:
    def depth(self) -> int:
        return 0


class OutboundDeliveryTests(unittest.TestCase):
    def run_delivery(self, final_stats: dict[str, object]) -> dict[str, object]:
        info = {
            "follower_count": 1,
            "delivery_enabled": True,
            "delivery_receiver_url": "http://receiver",
            "url": "https://sender",
            "token": "synthetic-token",
        }

        def post_notes(
            url: str,
            token: str,
            count: int,
            done: object,
            elapsed: dict[str, float],
            post_count: dict[str, int],
            responses: list[bytes],
        ) -> None:
            elapsed["v"] = 0.1
            post_count["v"] = count
            responses.extend([
                b'{"createdNote":{"id":"note-1"}}',
                b'{"createdNote":{"id":"note-2"}}',
            ])
            done.set()

        with (
            patch.object(driver, "OUTBOUND_NOTES", 2),
            patch.object(driver, "make_probe", return_value=ImmediateProbe()),
            patch.object(driver, "prepare_receiver"),
            patch.object(
                driver.httpx,
                "post",
                return_value=types.SimpleNamespace(raise_for_status=lambda: None),
            ),
            patch.object(driver, "receiver_stats", return_value={"verified": 4}),
            patch.object(driver, "post_notes_async", side_effect=post_notes),
            patch.object(driver, "finalize_delivery", return_value=(final_stats, None)),
        ):
            return driver.run_stack("mkq", info)

    def test_throughput_uses_unique_receipts_not_duplicate_attempts(self) -> None:
        result = self.run_delivery({
            "finalized": True,
            "expected": 2,
            "attempts": 4,
            "verified": 4,
            "pending": 0,
            "valid": 4,
            "unique": 2,
            "duplicates": 2,
            "invalid": 0,
            "contentMismatch": 0,
            "unexpected": 0,
            "unexpectedAttempts": 0,
            "missing": 0,
        })

        self.assertAlmostEqual(
            result["throughput_jobs_per_sec"],
            2 / result["drain_seconds"],
        )
        self.assertEqual(result.get("throughput_jobs"), 2)
        self.assertEqual(result["delivery"]["verified"], 4)
        self.assertEqual(result["delivery"]["unique"], 2)
        self.assertEqual(result["delivery"]["duplicates"], 2)

    def test_missing_unique_receipt_suppresses_throughput_and_keeps_counts(self) -> None:
        result = self.run_delivery({
            "finalized": True,
            "expected": 2,
            "attempts": 2,
            "verified": 2,
            "pending": 0,
            "valid": 2,
            "unique": 1,
            "duplicates": 1,
            "invalid": 0,
            "contentMismatch": 0,
            "unexpected": 0,
            "unexpectedAttempts": 0,
            "missing": 1,
        })

        self.assertIsNone(result.get("throughput_jobs"))
        self.assertIsNone(result["throughput_jobs_per_sec"])
        self.assertEqual(result.get("deliveryStatus"), "incomplete")
        self.assertTrue(result.get("deliveryReasons"))
        self.assertEqual(result["delivery"]["missing"], 1)

    def test_content_mismatch_is_invalid_and_keeps_raw_counts(self) -> None:
        result = self.run_delivery({
            "finalized": True,
            "expected": 2,
            "attempts": 2,
            "verified": 2,
            "pending": 0,
            "valid": 1,
            "unique": 1,
            "duplicates": 0,
            "invalid": 1,
            "contentMismatch": 1,
            "unexpected": 0,
            "unexpectedAttempts": 0,
            "missing": 1,
        })

        self.assertIsNone(result["throughput_jobs_per_sec"])
        self.assertEqual(result["deliveryStatus"], "invalid")
        self.assertEqual(result["delivery"]["contentMismatch"], 1)
        self.assertIn("1 content mismatches", result["deliveryReasons"])

    def test_receiver_setup_failure_is_incomplete_with_no_throughput(self) -> None:
        info = {
            "follower_count": 1,
            "delivery_enabled": True,
            "delivery_receiver_url": "http://receiver",
            "url": "https://sender",
            "token": "synthetic-token",
        }
        with (
            patch.object(driver, "make_probe", return_value=ImmediateProbe()),
            patch.object(driver, "prepare_receiver", side_effect=RuntimeError("not ready")),
        ):
            result = driver.run_stack("mkq", info)

        self.assertEqual(result["deliveryStatus"], "incomplete")
        self.assertIsNone(result["throughput_jobs"])
        self.assertIsNone(result["throughput_jobs_per_sec"])
        self.assertIn(
            "delivery receiver setup failed: RuntimeError: not ready",
            result["deliveryReasons"],
        )

    def test_unavailable_stats_after_finalization_error_are_incomplete(self) -> None:
        result = throughput_result(
            {"finalized": False},
            1.0,
            benchmark_expected=2,
            preparation_error="receiver stats unavailable",
        )

        self.assertEqual(result["deliveryStatus"], "incomplete")
        self.assertIsNone(result["throughput_jobs"])
        self.assertIsNone(result["throughput_jobs_per_sec"])

    def test_timeout_suppresses_throughput(self) -> None:
        result = throughput_result(
            {
                "finalized": True,
                "expected": 2,
                "attempts": 2,
                "verified": 2,
                "pending": 0,
                "valid": 2,
                "unique": 2,
                "duplicates": 0,
                "invalid": 0,
                "contentMismatch": 0,
                "unexpected": 0,
                "unexpectedAttempts": 0,
                "missing": 0,
            },
            1.0,
            benchmark_expected=2,
            timed_out=True,
        )

        self.assertEqual(result["deliveryStatus"], "incomplete")
        self.assertIn("delivery drain timed out", result["deliveryReasons"])
        self.assertIsNone(result["throughput_jobs"])
        self.assertIsNone(result["throughput_jobs_per_sec"])

    def test_benchmark_expected_mismatch_is_invalid(self) -> None:
        result = throughput_result(
            {
                "finalized": True,
                "expected": 2,
                "attempts": 2,
                "verified": 2,
                "pending": 0,
                "valid": 2,
                "unique": 2,
                "duplicates": 0,
                "invalid": 0,
                "contentMismatch": 0,
                "unexpected": 0,
                "unexpectedAttempts": 0,
                "missing": 0,
            },
            1.0,
            benchmark_expected=3,
        )

        self.assertEqual(result["deliveryStatus"], "invalid")
        self.assertIn(
            "receiver expected 2 != benchmark expected 3",
            result["deliveryReasons"],
        )
        self.assertIsNone(result["throughput_jobs"])
        self.assertIsNone(result["throughput_jobs_per_sec"])

    def test_shared_delivery_classification_cases(self) -> None:
        cases_path = Path(__file__).with_name("delivery_classification_cases.json")
        cases = json.loads(cases_path.read_text(encoding="utf-8"))
        for case in cases:
            with self.subTest(case=case["name"]):
                status, reasons = classify_delivery(
                    case["stats"],
                    benchmark_expected=case.get("benchmarkExpected"),
                    timed_out=case.get("timedOut", False),
                )
                self.assertEqual(status, case["expectedStatus"])
                self.assertEqual(reasons, case["expectedReasons"])

    def test_delivery_artifact_flattens_raw_stats_and_unique_rate(self) -> None:
        artifact = driver.delivery_artifact({
            "delivery": {"expected": 2, "unique": 2, "verified": 3, "duplicates": 1},
            "deliveryStatus": "valid",
            "deliveryReasons": [],
            "throughput_jobs": 2,
            "throughput_jobs_per_sec": 4.0,
            "drain_seconds": 0.5,
        })

        self.assertEqual(artifact["expected"], 2)
        self.assertEqual(artifact["verified"], 3)
        self.assertEqual(artifact["throughputJobs"], 2)
        self.assertEqual(artifact["throughputJobsPerSecond"], 4.0)
        self.assertEqual(artifact["drainSeconds"], 0.5)


if __name__ == "__main__":
    unittest.main()
