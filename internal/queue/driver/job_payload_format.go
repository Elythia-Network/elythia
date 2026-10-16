package driver

// CompactJobPayloads switches job writers to the #3614 shapes: task payloads
// embedded as JSON in the job data (mkqdriver's `bodyJson`) and deliver /
// inbox activities embedded as JSON (`body` object / `bodyEscaped` /
// `bodyText`, internal/queue/activity_body.go).
//
// Readers accept both the old and the new shapes regardless of this value.
//
// **#3614 は 2 段に分けて出す。** この版は読む側だけで、積む job は以前の形
// (payload も本文も base64) のまま。新しい形を書くのは、次のリリースでここを
// true にしてから。読む側を先に出しておけば、書く側を有効にした版から 1 つ前の
// 版 (この版) へ戻しても、積まれた job を読める。#3614 より前の版は新しい形を
// 読めない (docs/divergence/config.md §4-3)。
//
// 有効にするときの手順:
//   - ここを true にする
//   - `TestCompactJobPayloadsAreNotWrittenYet` (job_payload_format_test.go) を
//     消す
//   - 書く形を見るテストは定数に合わせて期待を切り替えるので、そのまま残す。
//     true にした後は新しい形を書いているか (切り替えが効いているか) を見る:
//     `TestClient_EnqueueWritesTheShapeSelectedByCompactJobPayloads`、
//     `TestActivityBody_WritesTheShapeSelectedByCompactJobPayloads`
//     (internal/queue)、
//     `TestFramedPayload_WritesTheShapeSelectedByCompactJobPayloads`、
//     `TestScheduler_RegisterWritesTheShapeSelectedByCompactJobPayloads`、
//     `TestFramedPayload_InboxBodySurvivesRedisByteForByte` の current
//     (internal/queue/driver/mkqdriver)
//   - docs を更新する: docs/divergence/config.md §4-3 (「読む側」「書く側」の
//     箇条と「書く側を有効にするリリースの注意」)、docs/divergence/api.md の
//     `data` の行、docs/plugins/compatibility.md の `{type, body}` の記述。
//     リリースノートに「書く側を有効にするリリースの注意」を載せる
const CompactJobPayloads = false
