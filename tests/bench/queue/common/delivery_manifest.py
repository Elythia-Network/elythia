"""Build expected tuples for the optional signed-delivery fixture."""
from __future__ import annotations

import hashlib
import json
from typing import Any


class _JSONNumber(str):
    """Preserve the original JSON number token as Go's json.Number does."""


def created_note_id(create_response: bytes) -> str | None:
    """Extract a note ID from a successful create response after timing."""
    try:
        payload = json.loads(create_response)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return None
    note = payload.get("createdNote") if isinstance(payload, dict) else None
    note_id = note.get("id") if isinstance(note, dict) else None
    return note_id if isinstance(note_id, str) and note_id else None


def _canonical_json(value: Any) -> str:
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, _JSONNumber):
        return str(value)
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False).replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    if isinstance(value, list):
        return "[" + ",".join(_canonical_json(item) for item in value) + "]"
    if isinstance(value, dict):
        fields = []
        for key in sorted(value):
            encoded_key = _canonical_json(key)
            fields.append(encoded_key + ":" + _canonical_json(value[key]))
        return "{" + ",".join(fields) + "}"
    raise ValueError(f"unsupported activity JSON value: {type(value).__name__}")


def build_deliveries(activity_body: bytes, inboxes: list[str]) -> list[dict[str, str]]:
    """Return one (activity ID, inbox, canonical body digest) tuple per inbox."""
    try:
        activity = json.loads(
            activity_body,
            parse_int=_JSONNumber,
            parse_float=_JSONNumber,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"invalid activity JSON: {exc}") from exc
    if not isinstance(activity, dict) or not isinstance(activity.get("id"), str) or not activity["id"].strip():
        raise ValueError("activity id is missing")
    if not inboxes:
        raise ValueError("delivery inbox list is empty")
    if len(set(inboxes)) != len(inboxes):
        raise ValueError("duplicate inbox in delivery manifest")

    canonical = _canonical_json(activity).encode("utf-8")
    body_sha256 = hashlib.sha256(canonical).hexdigest()
    return [
        {"activityId": activity["id"], "inbox": inbox, "bodySha256": body_sha256}
        for inbox in inboxes
    ]
