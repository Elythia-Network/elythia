# Signed delivery receiver fixture

This isolated benchmark receiver verifies the signed HTTP request and body
admission with `internal/activitypub`. It reconciles each accepted request
against the exact expected `(Activity ID, destination Inbox, canonical body
SHA-256)` tuple. It does not call the production inbox or change delivery
behavior.

The fixture accepts only public keys supplied by the existing synthetic
sender fixture. It never fetches actors or keys. The caller may pass a
`-keys` JSON file at startup or post keys to `/keys` after the stack starts:

```json
{
  "keys": {
    "https://sender.example/users/bench#main-key": "-----BEGIN PUBLIC KEY-----\n..."
  }
}
```

After the synthetic notes and expected Activity bodies are known, post the
expected manifest to `/expect`:

```json
{
  "deliveries": [
    {
      "activityId": "https://sender.example/notes/n1/activity",
      "inbox": "http://deliveryreceiver/inbox/follower-1",
      "bodySha256": "64 lowercase or uppercase hexadecimal characters"
    }
  ]
}
```

`bodySha256` is SHA-256 of the JSON activity after decoding and re-encoding it
as compact JSON with sorted object keys and HTML escaping disabled. The
manifest must contain at least one tuple; duplicate tuples, non-HTTP(S) inbox
URLs, or invalid digests are rejected. A tuple is unique by both Activity ID
and Inbox, so one expected delivery cannot cover another inbox. A matching
signature with a different body is counted as a content mismatch, not a valid
delivery or duplicate.

`GET /stats` returns the per-run summary for saving as `delivery.json` in a
`benchcompare` run directory. `POST /reset` clears observations and the
manifest but retains public keys. `GET /healthz` is a readiness endpoint.
Signed arrivals received before the expected manifest are buffered and shown
as pending; they are not called valid until reconciliation completes.

The `/keys` and `/expect` setup endpoints are unauthenticated and are for the
private benchmark network only. Do not publish this fixture's port. The
receiver does not fetch URLs from the manifest, so inbox entries do not cause
outbound requests. The 4 MiB activity cap and bounded setup JSON bodies limit
fixture input.
