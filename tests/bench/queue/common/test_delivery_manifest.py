"""Focused tests for the optional signed-delivery queue fixture."""

from __future__ import annotations

import hashlib
import json
import unittest

from delivery_manifest import build_deliveries, created_note_id


class DeliveryManifestTests(unittest.TestCase):
    def test_extracts_note_id_after_create_response_is_captured(self) -> None:
        self.assertEqual(created_note_id(b'{"createdNote":{"id":"n1"}}'), "n1")
        self.assertIsNone(created_note_id(b""))

    def test_builds_one_canonical_body_tuple_per_inbox(self) -> None:
        body = b'{"object":{"z":1.0,"a":"<synthetic>"},"id":"https://sender/notes/n1/activity"}'

        actual = build_deliveries(body, [
            "http://receiver/inbox/1",
            "http://receiver/inbox/2",
        ])

        canonical = b'{"id":"https://sender/notes/n1/activity","object":{"a":"<synthetic>","z":1.0}}'
        expected_digest = hashlib.sha256(canonical).hexdigest()
        self.assertEqual(
            actual,
            [
                {
                    "activityId": "https://sender/notes/n1/activity",
                    "inbox": "http://receiver/inbox/1",
                    "bodySha256": expected_digest,
                },
                {
                    "activityId": "https://sender/notes/n1/activity",
                    "inbox": "http://receiver/inbox/2",
                    "bodySha256": expected_digest,
                },
            ],
        )

    def test_rejects_activity_without_id(self) -> None:
        with self.assertRaisesRegex(ValueError, "activity id"):
            build_deliveries(b'{"type":"Create"}', ["http://receiver/inbox/1"])

    def test_rejects_duplicate_inboxes(self) -> None:
        body = b'{"id":"https://sender/notes/n1/activity"}'
        with self.assertRaisesRegex(ValueError, "duplicate inbox"):
            build_deliveries(body, ["http://receiver/inbox/1", "http://receiver/inbox/1"])


if __name__ == "__main__":
    unittest.main()
