package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cleanupSigCap(t *testing.T, hosts ...string) {
	t.Helper()
	for _, host := range hosts {
		testDB.Exec(`DELETE FROM "instance_signature_capability" WHERE host = ?`, host)
	}
}

func newSigCapRepo(t *testing.T, hosts ...string) InstanceSignatureCapabilityRepository {
	t.Helper()
	cleanupSigCap(t, hosts...)
	t.Cleanup(func() { cleanupSigCap(t, hosts...) })
	return NewInstanceSignatureCapabilityRepository(testDB)
}

func TestInstanceSignatureCapabilityRepository_RecordInboundAlg(t *testing.T) {
	const host = "sigcap-inbound.example"
	repo := newSigCapRepo(t, host)
	at := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, repo.RecordInboundAlg(host, model.SignatureAlgEd25519, at))

	row, err := repo.FindByHost(host)
	require.NoError(t, err)
	require.NotNil(t, row.InboundAlg)
	assert.Equal(t, model.SignatureAlgEd25519, *row.InboundAlg)
	require.NotNil(t, row.InboundObservedAt)
	assert.WithinDuration(t, at, *row.InboundObservedAt, time.Millisecond)
	assert.Nil(t, row.Ed25519DeclaredAt)
	assert.Nil(t, row.LDSignatureSeenAt)
	assert.Nil(t, row.Ed25519AcceptedAt)
}

// 同じ host への再観測は行を増やさず上書きする (host が PK)。
func TestInstanceSignatureCapabilityRepository_RecordInboundAlgOverwrites(t *testing.T) {
	const host = "sigcap-overwrite.example"
	repo := newSigCapRepo(t, host)
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	second := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, repo.RecordInboundAlg(host, model.SignatureAlgRSA, first))
	require.NoError(t, repo.RecordInboundAlg(host, model.SignatureAlgEd25519, second))

	row, err := repo.FindByHost(host)
	require.NoError(t, err)
	require.NotNil(t, row.InboundAlg)
	assert.Equal(t, model.SignatureAlgEd25519, *row.InboundAlg, "直近の観測で上書きされる")
	require.NotNil(t, row.InboundObservedAt)
	assert.WithinDuration(t, second, *row.InboundObservedAt, time.Millisecond)

	var count int64
	require.NoError(t, testDB.Model(&model.InstanceSignatureCapability{}).
		Where("host = ?", host).Count(&count).Error)
	assert.EqualValues(t, 1, count, "行は 1 本のまま")
}

// 本テーブルの肝。3 系統は独立したタイミングで書かれるので、ある系統の記録が
// 他系統の列を NULL で潰さないことを全順列で担保する。ここが壊れると「最後に
// 起きた観測だけが残る」テーブルに退化し、ラベル表示の根拠が失われる。
func TestInstanceSignatureCapabilityRepository_RecordsDoNotClobberEachOther(t *testing.T) {
	const host = "sigcap-noclobber.example"
	repo := newSigCapRepo(t, host)
	base := time.Now().UTC().Truncate(time.Millisecond)

	type record struct {
		name  string
		apply func(at time.Time) error
	}
	records := []record{
		{"inbound", func(at time.Time) error { return repo.RecordInboundAlg(host, model.SignatureAlgEd25519, at) }},
		{"ldSignature", func(at time.Time) error { return repo.RecordLDSignature(host, at) }},
		{"ed25519Accepted", func(at time.Time) error { return repo.RecordEd25519Accepted(host, at) }},
		{"ed25519Declared", func(at time.Time) error { return repo.RecordEd25519Declared(host, at) }},
	}

	// 適用順序が結果に影響しないことを見るため、開始位置をずらした 4 通りの
	// 巡回順で回す。どの順序でも最終的に 4 系統すべてが残らなければならない。
	for start := range records {
		t.Run(records[start].name+"-first", func(t *testing.T) {
			cleanupSigCap(t, host)
			for i := range records {
				rec := records[(start+i)%len(records)]
				require.NoError(t, rec.apply(base.Add(time.Duration(i)*time.Minute)), rec.name)
			}

			row, err := repo.FindByHost(host)
			require.NoError(t, err)
			require.NotNil(t, row.InboundAlg, "inboundAlg が残る")
			assert.Equal(t, model.SignatureAlgEd25519, *row.InboundAlg)
			assert.NotNil(t, row.InboundObservedAt, "inboundObservedAt が残る")
			assert.NotNil(t, row.LDSignatureSeenAt, "ldSignatureSeenAt が残る")
			assert.NotNil(t, row.Ed25519AcceptedAt, "ed25519AcceptedAt が残る")
			assert.NotNil(t, row.Ed25519DeclaredAt, "ed25519DeclaredAt が残る")
		})
	}
}

func TestInstanceSignatureCapabilityRepository_RecordLDSignature(t *testing.T) {
	const host = "sigcap-ld.example"
	repo := newSigCapRepo(t, host)
	at := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, repo.RecordLDSignature(host, at))

	row, err := repo.FindByHost(host)
	require.NoError(t, err)
	require.NotNil(t, row.LDSignatureSeenAt)
	assert.WithinDuration(t, at, *row.LDSignatureSeenAt, time.Millisecond)
	assert.True(t, row.SupportsLDSignature())
}

func TestInstanceSignatureCapabilityRepository_RecordEd25519Accepted(t *testing.T) {
	const host = "sigcap-accepted.example"
	repo := newSigCapRepo(t, host)
	at := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, repo.RecordEd25519Accepted(host, at))

	row, err := repo.FindByHost(host)
	require.NoError(t, err)
	require.NotNil(t, row.Ed25519AcceptedAt)
	assert.WithinDuration(t, at, *row.Ed25519AcceptedAt, time.Millisecond)
	assert.True(t, row.SupportsEd25519())
}

func TestInstanceSignatureCapabilityRepository_RecordEd25519Declared(t *testing.T) {
	const host = "sigcap-declared.example"
	repo := newSigCapRepo(t, host)
	at := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, repo.RecordEd25519Declared(host, at))

	row, err := repo.FindByHost(host)
	require.NoError(t, err)
	require.NotNil(t, row.Ed25519DeclaredAt)
	assert.WithinDuration(t, at, *row.Ed25519DeclaredAt, time.Millisecond)
	assert.True(t, row.SupportsEd25519())
}

func TestInstanceSignatureCapabilityRepository_FindManyByHosts(t *testing.T) {
	const hostA, hostB, hostC = "sigcap-many-a.example", "sigcap-many-b.example", "sigcap-many-c.example"
	repo := newSigCapRepo(t, hostA, hostB, hostC)
	at := time.Now().UTC()

	require.NoError(t, repo.RecordInboundAlg(hostA, model.SignatureAlgEd25519, at))
	require.NoError(t, repo.RecordInboundAlg(hostB, model.SignatureAlgRSA, at))

	// hostC は未観測。要求した host のうち行がある分だけが返る。
	rows, err := repo.FindManyByHosts([]string{hostA, hostB, hostC})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byHost := map[string]*model.InstanceSignatureCapability{}
	for _, row := range rows {
		byHost[row.Host] = row
	}
	require.Contains(t, byHost, hostA)
	require.Contains(t, byHost, hostB)
	assert.NotContains(t, byHost, hostC)
	assert.True(t, byHost[hostA].SupportsEd25519())
	assert.False(t, byHost[hostB].SupportsEd25519(), "RSA のみの host は Ed25519 非対応")
}

func TestInstanceSignatureCapabilityRepository_FindManyByHostsEmpty(t *testing.T) {
	repo := NewInstanceSignatureCapabilityRepository(testDB)
	rows, err := repo.FindManyByHosts(nil)
	require.NoError(t, err)
	assert.Nil(t, rows)
}

func TestInstanceSignatureCapabilityRepository_FindByHostNotFound(t *testing.T) {
	repo := NewInstanceSignatureCapabilityRepository(testDB)
	_, err := repo.FindByHost("sigcap-missing.example")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// host / alg が空の呼び出しは no-op。観測経路が host を解決できなかった場合に
// 空 host の行を作らないための防御 (best-effort フックなので error にはしない)。
func TestInstanceSignatureCapabilityRepository_RecordIgnoresEmptyInput(t *testing.T) {
	repo := newSigCapRepo(t, "")
	at := time.Now().UTC()

	require.NoError(t, repo.RecordInboundAlg("", model.SignatureAlgEd25519, at))
	require.NoError(t, repo.RecordInboundAlg("sigcap-empty-alg.example", "", at))
	require.NoError(t, repo.RecordLDSignature("", at))
	require.NoError(t, repo.RecordEd25519Accepted("", at))
	require.NoError(t, repo.RecordEd25519Declared("", at))

	var count int64
	require.NoError(t, testDB.Model(&model.InstanceSignatureCapability{}).
		Where("host IN ?", []string{"", "sigcap-empty-alg.example"}).Count(&count).Error)
	assert.EqualValues(t, 0, count, "空入力では行を作らない")
}

func TestColumnValue_UnknownColumn(t *testing.T) {
	row := &model.InstanceSignatureCapability{Host: "x.example"}
	assert.Nil(t, columnValue(row, "no-such-column"))
}
