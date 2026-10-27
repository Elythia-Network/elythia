package admin_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IP の照会の sinceDays・limit・offset は integer として宣言した (#3869)。本家の
// ajv と同じく、整数を小数の形で書いた値も受け、範囲の端の値で引く。照合の
// テスト (TestEnforcedEndpointsMatchTheCorpus) の corpus は、ip が IP として
// 読めず、userId がリモートの利用者を指すので、ここまで届かない。

func TestIPAccounts_AcceptsIntegerFormsAtTheBounds(t *testing.T) {
	stub := &stubIPSearch{hasAny: true}
	h, _, _ := newIPSearchHandler(t, stub)

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1","sinceDays":3650.0,"limit":1e2,"offset":10000.0}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, 10000, stub.gotOffset)
	// 次のページの有無を見るため、1 件多く引く。
	assert.Equal(t, 101, stub.gotLimit)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.Equal(t, 3650, got.SinceDays)
	assert.Equal(t, 100, got.Limit)
	assert.Equal(t, 10000, got.Offset)
}

func TestIPRelated_AcceptsIntegerFormsAtTheBounds(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, _, _ := newRelatedHandler(t, stub)

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target","sinceDays":3650.0,"limit":1e2,"offset":10000.0}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "target", stub.gotIPsUser)
	got := decodeRelated(t, rec.Body.Bytes())
	assert.Equal(t, 3650, got.SinceDays)
	assert.Equal(t, 100, got.Limit)
	assert.Equal(t, 10000, got.Offset)
}

func TestIPLookupLog_AcceptsIntegerFormsAtTheBounds(t *testing.T) {
	repo := &stubAuditRepo{}
	h, _ := newAuditHandler(t, repo)

	rec := doPost(h.IPLookupLog, `{"limit":1e2,"offset":10000.0}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 10000, repo.gotOffset)
	assert.Equal(t, 101, repo.gotLimit)
}
