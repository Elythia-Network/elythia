package federation

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeCapabilityLookup implements SignatureCapabilityLookup for the
// signatureCapability response tests.
type fakeCapabilityLookup struct {
	rows      map[string]*model.InstanceSignatureCapability
	err       error
	bulkCalls int
	lastHosts []string
}

func (f *fakeCapabilityLookup) FindByHost(host string) (*model.InstanceSignatureCapability, error) {
	if f.err != nil {
		return nil, f.err
	}
	row, ok := f.rows[host]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return row, nil
}

func (f *fakeCapabilityLookup) FindManyByHosts(hosts []string) ([]*model.InstanceSignatureCapability, error) {
	f.bulkCalls++
	f.lastHosts = hosts
	if f.err != nil {
		return nil, f.err
	}
	out := []*model.InstanceSignatureCapability{}
	for _, host := range hosts {
		if row, ok := f.rows[host]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func ptrStr(s string) *string { return &s }

// 未配線なら signatureCapability は常に null。既存デプロイ / 最小構成でも
// レスポンス shape が壊れないこと。
func TestShowInstance_SignatureCapabilityUnwired(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Contains(t, resp, "signatureCapability", "field 自体は常に存在する")
	assert.Nil(t, resp["signatureCapability"])
}

// 観測が無い host も null。行が無いことと取得に失敗したことを区別しない。
func TestShowInstance_SignatureCapabilityNotObserved(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{rows: map[string]*model.InstanceSignatureCapability{}})

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["signatureCapability"])
}

func TestShowInstance_SignatureCapabilityFull(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	declared := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	accepted := time.Date(2026, 8, 2, 3, 4, 5, 678_000_000, time.UTC)
	observed := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	ldSeen := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{rows: map[string]*model.InstanceSignatureCapability{
		"alpha.example": {
			Host:              "alpha.example",
			Ed25519DeclaredAt: ptrTime(declared),
			Ed25519AcceptedAt: ptrTime(accepted),
			InboundAlg:        ptrStr(model.SignatureAlgEd25519),
			InboundObservedAt: ptrTime(observed),
			LDSignatureSeenAt: ptrTime(ldSeen),
		},
	}})

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	cap, ok := resp["signatureCapability"].(map[string]any)
	require.True(t, ok, "signatureCapability は object")
	assert.Equal(t, true, cap["ed25519"])
	assert.Equal(t, true, cap["ldSignature"])
	assert.Equal(t, model.SignatureAlgEd25519, cap["inboundAlgorithm"])
	// 時刻は instance の他 field と同じ toISOString() 形式 (#1948-10)。
	assert.Equal(t, "2026-08-01T00:00:00.000Z", cap["ed25519DeclaredAt"])
	assert.Equal(t, "2026-08-02T03:04:05.678Z", cap["ed25519AcceptedAt"])
	assert.Equal(t, "2026-08-03T00:00:00.000Z", cap["inboundObservedAt"])
	assert.Equal(t, "2026-08-04T00:00:00.000Z", cap["ldSignatureSeenAt"])
}

// RSA しか観測していない相手は ed25519=false。ここが true に転ぶと
// 「Ed25519 対応」ラベルが全サーバーに出て意味を失う。
func TestShowInstance_SignatureCapabilityRSAOnly(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{rows: map[string]*model.InstanceSignatureCapability{
		"alpha.example": {
			Host:              "alpha.example",
			InboundAlg:        ptrStr(model.SignatureAlgRSA),
			InboundObservedAt: ptrTime(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)),
		},
	}})

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	cap := resp["signatureCapability"].(map[string]any)
	assert.Equal(t, false, cap["ed25519"])
	assert.Equal(t, false, cap["ldSignature"])
	assert.Equal(t, model.SignatureAlgRSA, cap["inboundAlgorithm"])
	assert.Nil(t, cap["ed25519DeclaredAt"])
	assert.Nil(t, cap["ed25519AcceptedAt"])
	assert.Nil(t, cap["ldSignatureSeenAt"])
}

// 宣言だけでも Ed25519 対応とみなす (まだ実際の署名を観測していない相手)。
func TestShowInstance_SignatureCapabilityDeclaredOnly(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{rows: map[string]*model.InstanceSignatureCapability{
		"alpha.example": {
			Host:              "alpha.example",
			Ed25519DeclaredAt: ptrTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)),
		},
	}})

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	cap := resp["signatureCapability"].(map[string]any)
	assert.Equal(t, true, cap["ed25519"])
	assert.Nil(t, cap["inboundAlgorithm"], "未観測の inboundAlgorithm は null")
}

// lookup が error でもレスポンスは 200 のまま null に degrade する。表示用の
// メタデータで一覧全体を落とさない。
func TestShowInstance_SignatureCapabilityLookupError(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{err: errors.New("db down")})

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["signatureCapability"])
}

// 一覧は host の数によらず bulk lookup 1 回。ここが崩れると 1 ページで
// limit 回のクエリが飛ぶ (N+1)。
func TestInstances_SignatureCapabilityUsesSingleBulkLookup(t *testing.T) {
	h, repo := newHandler(t)
	for _, host := range []string{"a.example", "b.example", "c.example"} {
		seedInstance(t, repo, host)
	}
	lookup := &fakeCapabilityLookup{rows: map[string]*model.InstanceSignatureCapability{
		"a.example": {Host: "a.example", Ed25519DeclaredAt: ptrTime(time.Now())},
	}}
	h.SetSignatureCapabilityLookup(lookup)

	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, 1, lookup.bulkCalls, "host 数によらず 1 クエリ")
	assert.ElementsMatch(t, []string{"a.example", "b.example", "c.example"}, lookup.lastHosts)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 3)
	byHost := map[string]any{}
	for _, row := range resp {
		byHost[row["host"].(string)] = row["signatureCapability"]
	}
	cap, ok := byHost["a.example"].(map[string]any)
	require.True(t, ok, "観測のある host は object")
	assert.Equal(t, true, cap["ed25519"])
	assert.Nil(t, byHost["b.example"], "未観測 host は null")
	assert.Nil(t, byHost["c.example"])
}

func TestInstances_SignatureCapabilityUnwired(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "a.example")

	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	require.Contains(t, resp[0], "signatureCapability")
	assert.Nil(t, resp[0]["signatureCapability"])
}

func TestInstances_SignatureCapabilityLookupError(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "a.example")
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{err: errors.New("db down")})

	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Nil(t, resp[0]["signatureCapability"])
}

// 空一覧では lookup を呼ばない (無駄な IN () クエリを撃たない)。
func TestInstances_SignatureCapabilitySkipsEmptyPage(t *testing.T) {
	h, _ := newHandler(t)
	lookup := &fakeCapabilityLookup{}
	h.SetSignatureCapabilityLookup(lookup)

	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, lookup.bulkCalls)
}

// federation/stats は公開エンドポイントなので capability を引かない (常に null)。
func TestStats_SignatureCapabilityAlwaysNull(t *testing.T) {
	h, repo := newHandler(t)
	inst := seedInstance(t, repo, "a.example")
	inst.FollowersCount = 3
	inst.FollowingCount = 2
	lookup := &fakeCapabilityLookup{rows: map[string]*model.InstanceSignatureCapability{
		"a.example": {Host: "a.example", Ed25519DeclaredAt: ptrTime(time.Now())},
	}}
	h.SetSignatureCapabilityLookup(lookup)

	rec := postBody(h.Stats, `{"limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	for _, key := range []string{"topSubInstances", "topPubInstances"} {
		rows, _ := resp[key].([]any)
		for _, raw := range rows {
			row := raw.(map[string]any)
			assert.Nil(t, row["signatureCapability"], "%s は capability を出さない", key)
		}
	}
	assert.Zero(t, lookup.bulkCalls, "公開エンドポイントで追加クエリを撃たない")
}

func TestSignatureCapabilityToMap_Nil(t *testing.T) {
	assert.Nil(t, signatureCapabilityToMap(nil))
}

func TestLookupCapability_EmptyHost(t *testing.T) {
	h, _ := newHandler(t)
	h.SetSignatureCapabilityLookup(&fakeCapabilityLookup{})
	assert.Nil(t, h.lookupCapability(""))
}
