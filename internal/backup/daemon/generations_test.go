package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
)

var t0 = time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)

func dayN(n int) time.Time { return t0.AddDate(0, 0, n) }

func TestScanClassifiesGenerations(t *testing.T) {
	st := newMemStorage()
	ok := st.addGeneration(dayN(0), true, "ok")
	failed := st.addGeneration(dayN(1), true, "failed")
	garbage := st.addGeneration(dayN(2), true, "garbage")
	unverified := st.addGeneration(dayN(3), true, "")
	partial := st.addGeneration(dayN(4), false, "")
	st.set("generations/latest/meta.json", []byte("{}"))
	st.set("generations/"+ok, []byte("not inside a directory"))

	gens, err := Scan(context.Background(), st)
	require.NoError(t, err)
	require.Len(t, gens, 5)
	want := []struct {
		id       string
		complete bool
		verify   VerifyState
	}{
		{ok, true, VerifyOK}, {failed, true, VerifyFailed}, {garbage, true, VerifyUnreadable},
		{unverified, true, VerifyNone}, {partial, false, VerifyNone},
	}
	for i, w := range want {
		assert.Equal(t, w.id, gens[i].ID)
		assert.Equal(t, w.complete, gens[i].Complete, w.id)
		assert.Equal(t, w.verify, gens[i].Verify, w.id)
		assert.True(t, gens[i].Time.Equal(dayN(i)), w.id)
	}
	assert.Len(t, gens[0].Keys, 3)
	assert.Equal(t, int64(len("dump-"+ok))+int64(len(mustGet(t, st, backup.Key(ok, backup.MetaFile))))+int64(len(mustGet(t, st, backup.Key(ok, backup.VerifyFile)))), gens[0].Size)
}

func mustGet(t *testing.T, st *memStorage, key string) []byte {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.objects[key]
	require.True(t, ok, key)
	return b
}

func TestScanErrors(t *testing.T) {
	st := newMemStorage()
	st.addGeneration(dayN(0), true, "ok")
	st.listErr = errBoom
	_, err := Scan(context.Background(), st)
	require.ErrorIs(t, err, errBoom)
}

// 読めない meta.json / verify.json は、理由に関わらず使えない世代に倒す。
func TestScanTreatsUnreadableFilesAsUnusable(t *testing.T) {
	st := newMemStorage()
	id := st.addGeneration(dayN(0), true, "ok")
	st.getErr = errBoom
	gens, err := Scan(context.Background(), st)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	assert.False(t, gens[0].Complete, "meta.json that cannot be read")
	assert.Equal(t, VerifyUnreadable, gens[0].Verify)
	assert.False(t, gens[0].Usable(false))

	// meta.json はあるが、名指しする dump が無い世代は揃っていない。
	st.getErr = nil
	require.NoError(t, st.Delete(context.Background(), backup.Key(id, backup.DumpFile)))
	gens, err = Scan(context.Background(), st)
	require.NoError(t, err)
	assert.False(t, gens[0].Complete, "meta.json without its dump")
	assert.Equal(t, VerifyOK, gens[0].Verify)
}

func TestUsable(t *testing.T) {
	for _, tc := range []struct {
		g             Generation
		strict, loose bool
	}{
		{Generation{Complete: true, Verify: VerifyOK}, true, true},
		{Generation{Complete: true, Verify: VerifyNone}, false, true},
		{Generation{Complete: true, Verify: VerifyFailed}, false, false},
		{Generation{Complete: true, Verify: VerifyUnreadable}, false, false},
		{Generation{Complete: false, Verify: VerifyOK}, false, false},
	} {
		assert.Equal(t, tc.strict, tc.g.Usable(true), "%+v", tc.g)
		assert.Equal(t, tc.loose, tc.g.Usable(false), "%+v", tc.g)
	}
}

func ids(gens []Generation) []string {
	out := []string{}
	for _, g := range gens {
		out = append(out, g.ID)
	}
	return out
}

func gen(n int, complete bool, v VerifyState) Generation {
	return Generation{ID: backup.NewID(dayN(n)), Time: dayN(n), Complete: complete, Verify: v}
}

func TestPlanPrune(t *testing.T) {
	ok0, ok1, ok2, ok3 := gen(0, true, VerifyOK), gen(1, true, VerifyOK), gen(2, true, VerifyOK), gen(3, true, VerifyOK)
	failed := gen(4, true, VerifyFailed)
	partial := gen(5, false, VerifyNone)
	unverified := gen(6, true, VerifyNone)

	assert.Empty(t, PlanPrune([]Generation{ok0, ok1, ok2, ok3}, 0, true), "keep 0 keeps everything")
	assert.Empty(t, PlanPrune([]Generation{ok0, ok1}, 2, true), "exactly keep usable generations")
	assert.Empty(t, PlanPrune(nil, 2, true))
	assert.Equal(t, ids([]Generation{ok0, ok1}), ids(PlanPrune([]Generation{ok0, ok1, ok2, ok3}, 2, true)))

	// 新しい側の検証に落ちた世代・半端な世代・未検証の世代は数えない。
	// 使える世代が 2 個しかないので、何も消さない。
	gens := []Generation{ok0, ok1, failed, partial, unverified}
	assert.Empty(t, PlanPrune(gens, 3, true), "only broken generations newer: the old verified ones stay")
	assert.Equal(t, ids([]Generation{ok0}), ids(PlanPrune(gens, 1, true)), "window starts at the newest usable; newer broken ones stay")

	// 窓より古い壊れた世代は消す。
	old := []Generation{gen(0, false, VerifyNone), gen(1, true, VerifyFailed), ok2, ok3}
	assert.Equal(t, ids(old[:2]), ids(PlanPrune(old, 2, true)))

	// 検証を回さない設定では、未検証でも揃っていれば数える。
	assert.Equal(t, ids([]Generation{ok0, ok1, failed, partial}), ids(PlanPrune(gens, 1, false)))
	assert.Equal(t, ids([]Generation{ok0}), ids(PlanPrune(gens, 2, false)))
}

func TestLatest(t *testing.T) {
	gens := []Generation{gen(0, true, VerifyOK), gen(1, true, VerifyFailed), gen(2, false, VerifyNone)}
	assert.Equal(t, gens[0].ID, Latest(gens, func(g Generation) bool { return g.Usable(true) }).ID)
	assert.Equal(t, gens[1].ID, Latest(gens, func(g Generation) bool { return g.Complete }).ID)
	assert.Nil(t, Latest(gens, func(Generation) bool { return false }))
}

func TestDeleteGenerationRemovesMetaFirst(t *testing.T) {
	st := newMemStorage()
	id := st.addGeneration(dayN(0), true, "ok")
	other := st.addGeneration(dayN(1), true, "ok")
	gens, err := Scan(context.Background(), st)
	require.NoError(t, err)
	require.NoError(t, DeleteGeneration(context.Background(), st, gens[0]))
	require.NotEmpty(t, st.deleted)
	assert.Equal(t, backup.Key(id, backup.MetaFile), st.deleted[0], "meta.json goes first")
	assert.Len(t, st.deleted, 3)
	assert.Equal(t, []string{other}, st.ids())
}

func TestDeleteGenerationErrors(t *testing.T) {
	st := newMemStorage()
	st.addGeneration(dayN(0), true, "ok")
	gens, err := Scan(context.Background(), st)
	require.NoError(t, err)
	st.deleteErr = errBoom
	require.ErrorIs(t, DeleteGeneration(context.Background(), st, gens[0]), errBoom)

	// meta.json の無い世代でも、残りの削除の失敗を返す。
	partial := Generation{ID: gens[0].ID, Keys: []string{backup.Key(gens[0].ID, backup.DumpFile)}}
	require.ErrorIs(t, DeleteGeneration(context.Background(), st, partial), errBoom)
}
