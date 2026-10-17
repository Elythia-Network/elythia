"""Classify signed-delivery receiver stats for outbound benchmark output."""
from __future__ import annotations

from typing import Any

COUNT_FIELDS = (
    "expected",
    "attempts",
    "verified",
    "pending",
    "valid",
    "unique",
    "duplicates",
    "invalid",
    "contentMismatch",
    "unexpected",
    "unexpectedAttempts",
    "missing",
)


def classify_delivery(
    stats: dict[str, Any],
    *,
    benchmark_expected: int | None = None,
    timed_out: bool = False,
    preparation_error: str | None = None,
) -> tuple[str, list[str]]:
    """Return correctness status and reasons, matching benchcompare semantics."""
    reasons: list[str] = []
    count_reasons: list[str] = []
    counts: dict[str, int] = {}

    finalized = stats.get("finalized")
    if not isinstance(finalized, bool):
        count_reasons.append("finalized flag is missing or not boolean")
    elif not finalized:
        reasons.append("manifest is not finalized")

    for field in COUNT_FIELDS:
        value = stats.get(field)
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            count_reasons.append(f"{field} count is missing or invalid")
        else:
            counts[field] = value

    if count_reasons:
        reasons.extend(count_reasons)
    else:
        expected = counts["expected"]
        attempts = counts["attempts"]
        verified = counts["verified"]
        pending = counts["pending"]
        valid = counts["valid"]
        unique = counts["unique"]
        duplicates = counts["duplicates"]
        invalid = counts["invalid"]
        content_mismatch = counts["contentMismatch"]
        unexpected = counts["unexpected"]
        unexpected_attempts = counts["unexpectedAttempts"]
        missing = counts["missing"]

        if pending > 0:
            reasons.append(f"{pending} arrivals are pending")
        if expected == 0:
            reasons.append("expected delivery count is zero")
        if benchmark_expected is not None and expected != benchmark_expected:
            count_reasons.append(
                f"receiver expected {expected} != benchmark expected {benchmark_expected}"
            )
        if missing > 0:
            reasons.append(f"{missing} expected deliveries are missing")
        if invalid > 0:
            reasons.append(f"{invalid} invalid deliveries")
        if content_mismatch > 0:
            reasons.append(f"{content_mismatch} content mismatches")
        if unexpected > 0:
            reasons.append(f"{unexpected} unexpected delivery pairs")

        if not (
            valid <= attempts
            and invalid <= attempts - valid
            and unexpected_attempts == attempts - valid - invalid
        ):
            count_reasons.append(
                "attempts "
                f"{attempts} != valid {valid} + invalid {invalid} "
                f"+ unexpected attempts {unexpected_attempts}"
            )
        if content_mismatch > invalid:
            count_reasons.append(
                f"content mismatch {content_mismatch} exceeds invalid {invalid}"
            )
        if not (
            valid <= verified
            and content_mismatch <= verified - valid
            and unexpected_attempts == verified - valid - content_mismatch
        ):
            count_reasons.append(
                "verified "
                f"{verified} != valid {valid} + content mismatch {content_mismatch} "
                f"+ unexpected attempts {unexpected_attempts}"
            )
        if duplicates > valid or unique + duplicates != valid:
            count_reasons.append(
                f"valid {valid} != unique {unique} + duplicates {duplicates}"
            )
        if unique > expected:
            count_reasons.append(f"unique {unique} exceeds expected {expected}")
        elif missing != expected - unique:
            count_reasons.append(
                f"missing {missing} != expected {expected} - unique {unique}"
            )
        if unexpected > unexpected_attempts:
            count_reasons.append(
                f"unexpected pairs {unexpected} exceeds unexpected attempts "
                f"{unexpected_attempts}"
            )
        reasons.extend(count_reasons)

    if timed_out:
        reasons.append("delivery drain timed out")
    if preparation_error:
        reasons.append(f"delivery preparation/finalization failed: {preparation_error}")

    if preparation_error and len(counts) != len(COUNT_FIELDS):
        return "incomplete", reasons
    if not reasons:
        return "valid", []
    if count_reasons or (
        counts.get("expected") == 0
        or counts.get("invalid", 0) > 0
        or counts.get("contentMismatch", 0) > 0
        or counts.get("unexpected", 0) > 0
    ):
        return "invalid", reasons
    return "incomplete", reasons


def throughput_result(
    stats: dict[str, Any],
    drain_seconds: float,
    *,
    benchmark_expected: int,
    timed_out: bool = False,
    preparation_error: str | None = None,
) -> dict[str, Any]:
    """Keep raw receiver counts and emit throughput only for complete valid runs."""
    status, reasons = classify_delivery(
        stats,
        benchmark_expected=benchmark_expected,
        timed_out=timed_out,
        preparation_error=preparation_error,
    )
    is_valid = status == "valid" and drain_seconds > 0
    unique = stats.get("unique") if is_valid else None
    return {
        "delivery": stats,
        "deliveryStatus": status,
        "deliveryReasons": reasons,
        "throughput_jobs": unique,
        "throughput_jobs_per_sec": unique / drain_seconds if unique is not None else None,
    }
