package shapetest_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
)

// recorder captures t.Errorf so a test can assert on what Assert reported.
type recorder struct {
	testing.TB
	errs int
}

func (r *recorder) Errorf(string, ...any) { r.errs++ }
func (r *recorder) Helper()               {}

// AssertExcept が「名指しした field だけ」を見逃すこと (#2689 review MED-5)。
//
// 例外は golden が upstream の実装と食い違う箇所に使うためのもので、
// **他の drift まで一緒に握り潰したら意味が無い**。素通しに退化していないか
// を固定する。
func TestAssertExcept_OnlySkipsNamedFields(t *testing.T) {
	// QueueJob の required を 2 つ欠いた応答。
	partial := map[string]any{
		"id": "1", "name": "ap:inbox", "timestamp": float64(1), "attempts": float64(0),
		"delay": float64(0), "isFailed": false, "stacktrace": []any{},
		"data": map[string]any{}, "opts": map[string]any{}, "progress": float64(0),
		// failedReason と returnValue を欠く。
	}

	r := &recorder{TB: t}
	shapetest.AssertExcept(r, "QueueJob", partial, "failedReason", "returnValue")
	if r.errs != 0 {
		t.Errorf("名指しした 2 field は見逃すこと: %d 件報告された", r.errs)
	}

	// 名指ししていない required を欠いたら報告すること。
	r2 := &recorder{TB: t}
	missingID := map[string]any{}
	for k, v := range partial {
		missingID[k] = v
	}
	delete(missingID, "id")
	shapetest.AssertExcept(r2, "QueueJob", missingID, "failedReason", "returnValue")
	if r2.errs == 0 {
		t.Error("名指ししていない field の欠落は報告すること (素通しに退化している)")
	}

	// 例外を渡さなければ Assert と同じ。
	r3 := &recorder{TB: t}
	shapetest.AssertExcept(r3, "QueueJob", partial)
	if r3.errs == 0 {
		t.Error("例外を渡さなければ欠落を報告すること")
	}
}

// Assert は例外なしの AssertExcept と同じであること。
//
// **このパッケージにテストが無かったので CI の coverage 対象外だった**
// (対象は `_test.go` を持つパッケージだけ)。AssertExcept を足したときに
// 初めて対象になったので、Assert 側も固定しておく。
func TestAssert_ReportsDriftAndPassesCleanResponse(t *testing.T) {
	full := map[string]any{
		"id": "1", "name": "ap:inbox", "timestamp": float64(1), "attempts": float64(0),
		"delay": float64(0), "isFailed": false, "stacktrace": []any{},
		"data": map[string]any{}, "opts": map[string]any{}, "progress": float64(0),
		"failedReason": "", "returnValue": map[string]any{},
	}
	r := &recorder{TB: t}
	shapetest.Assert(r, "QueueJob", full)
	if r.errs != 0 {
		t.Errorf("golden を満たす応答は報告しないこと: %d 件", r.errs)
	}

	partial := map[string]any{}
	for k, v := range full {
		partial[k] = v
	}
	delete(partial, "failedReason")
	r2 := &recorder{TB: t}
	shapetest.Assert(r2, "QueueJob", partial)
	if r2.errs == 0 {
		t.Error("required 欄の欠落を報告すること")
	}
}
