package driver

// CompactJobPayloads switches job writers to the #3614 shapes: task payloads
// embedded as JSON in the job data (mkqdriver's `bodyJson`) and deliver /
// inbox activities embedded as JSON (`body` object / `bodyEscaped` /
// `bodyText`, internal/queue/activity_body.go).
//
// Readers accept both the old and the new shapes regardless of this value.
//
// **#3614 は 2 段に分けて出した。** 2.1.0 は読む側だけで、以前の形 (payload も
// 本文も base64) を書いていた。新しい形を書くのは 2.1.0 の次の版から。読む側を
// 先に出しておいたので、この版から 2.1.0 へ戻しても積まれた job を読める。
// #3614 より前の版は新しい形を読めない (docs/divergence/config.md §4-3)。
//
// false に戻すと、以前の形を書く。書く形を見るテストは、この定数で分岐して
// 期待を切り替える (`git grep -n "driver.CompactJobPayloads {"` で探せる。
// 名前が `...WritesTheShapeSelectedByCompactJobPayloads` のもの、
// `TestFramedPayload_InboxBodySurvivesRedisByteForByte`、
// `TestFramePayload_UsesTheVerbatimMarkOnlyWhenWritingCompactShapes`)。
const CompactJobPayloads = true
