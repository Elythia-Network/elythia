// Package shapetest provides test helpers to assert that an actual API
// response matches the golden contract (the "Layer 3" runtime response check).
//
// Unlike the Layer 0 struct-reflection gate, this validates what a handler
// actually emits, so it covers ad-hoc map[string]any responses and catches
// handler populate bugs. Intended for use from handler _test.go files:
//
//	shapetest.Assert(t, "MeDetailedOnly", resp)
//
// It deliberately lives in its own package (imports testing) so handler test
// packages can share it without duplicating the validate-and-report loop.
package shapetest

import (
	"slices"
	"testing"

	"github.com/elythia-network/elythia/internal/entitycompat"
)

// Assert fails t with a clear message for each gated (HIGH/MED) shape drift
// between the actual response map and the named golden schema.
func Assert(t testing.TB, schemaName string, actual map[string]any) {
	t.Helper()
	for _, f := range entitycompat.ValidateResponse(schemaName, actual) {
		t.Errorf("response shape drift vs golden %s: %s [%s]: %s", schemaName, f.Field, f.Kind, f.Detail)
	}
}

// AssertExcept is Assert with the named fields excluded from the check.
//
// **golden が upstream の実装と食い違う箇所にだけ使う。** golden は upstream の
// 宣言された JSON schema から生成しているが (tools/shapediff)、upstream の
// 実装がその schema を満たしていない field がある。そこを golden に合わせると
// **upstream の frontend が期待する挙動から外れる**。
//
// 例: QueueJob.failedReason は schema 上 required だが、Bull の job は失敗する
// まで failedReason を持たないので upstream の packJobData は undefined を返す。
// frontend は `v-if="job.failedReason != null"` で行の有無を決めているため、
// 空文字を常に出すと成功した job にも「Failed reason」行が出てしまう (#2689)。
//
// 使うときは**呼び出し側に理由をコメントで残すこと**。golden は生成物なので
// 手で直さない (CLAUDE.md Section 0)。
func AssertExcept(t testing.TB, schemaName string, actual map[string]any, exceptFields ...string) {
	t.Helper()
	for _, f := range entitycompat.ValidateResponse(schemaName, actual) {
		if slices.Contains(exceptFields, f.Field) {
			continue
		}
		t.Errorf("response shape drift vs golden %s: %s [%s]: %s", schemaName, f.Field, f.Kind, f.Detail)
	}
}
