package admin_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **列でないキーが repo へ届かないこと (handler まで通して見る)。**
//
// ヘルパー単体のテストだけでは、handler がそれを**呼んでいる**ことを固定
// できない (変異検証で実測: 呼び出しを外しても他のテストは全部緑だった)。
func TestUpdateMeta_UnknownFieldsNeverReachTheRepo(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"name":"ok","nope":1,"DROP TABLE meta":2}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.NotNil(t, metaRepo.LastUpdateFields)
	assert.Contains(t, metaRepo.LastUpdateFields, "name")
	assert.NotContains(t, metaRepo.LastUpdateFields, "nope", "列でないキーが UPDATE に載っている")
	assert.NotContains(t, metaRepo.LastUpdateFields, "DROP TABLE meta")
}

// **alias は残る。** 変換が終わってから絞ること (手前に置くと正当な入力を落とす)。
func TestUpdateMeta_AliasesSurviveTheFilter(t *testing.T) {
	for name, tc := range map[string]struct{ body, column string }{
		"tosUrl":       {`{"tosUrl":"https://t.example"}`, "termsOfServiceUrl"},
		"summalyProxy": {`{"summalyProxy":"https://p.example"}`, "urlPreviewSummaryProxyUrl"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			rec := doPost(h.UpdateMeta, tc.body, nil)
			require.Equal(t, http.StatusNoContent, rec.Code)
			assert.Contains(t, metaRepo.LastUpdateFields, tc.column, "alias を落としている")
		})
	}
}
