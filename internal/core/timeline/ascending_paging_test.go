package timeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// #2720: sinceId 単独指定の昇順ページング。
//
// 2 つの問題があった。
//
//  1. filterAndSort が常に DESC で先頭 limit 件を返すので、cursor の直後では
//     なく**最新 N 件**が返っていた (upstream は ASC で最古 N 件)
//  2. fallbackRange の境界が最古になり、DB fallback が Redis と同じ範囲を
//     引き直して**重複**を返していた
//
// 1 を直すと ids が ASC になり、境界が最新になるので 2 も同時に消える。

// TestHybridTimeline_AscendingReturnsOldestFirst は hybrid の昇順を固定する。
//
// hybrid は home / local の 2 クエリを Go 側でマージするので、向きを自分で
// 持たなければならない。無条件 DESC で並べてから truncate すると、cursor の
// 直後ではなく**最新 N 件**を返す。
//
// **shouldFallbackToDB により sinceId 系は必ずこの経路を通る**ので、ここが
// 昇順に対応していないと修正が hybrid で成り立たない。
//
// 実害は順序だけではない。frontend の paginator は fetchNewer で sinceId に
// 手持ちの最新 ID を渡すので、最新側から返すと間の note が二度と取得されない。
func TestHybridTimeline_AscendingReturnsOldestFirst(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	testRedis.FlushAll(context.Background())

	// **home / local に別々の集合を返す repo を使う。** MockNoteRepository の
	// ListHomeTimeline は userID を捨てるので、素の mock だと 2 クエリが同じ
	// 集合を返し「和集合から最古 N 件を切る」を検証できない。
	base := "aqm0000000000000000"
	ids := []string{base + "1", base + "2", base + "3", base + "4", base + "5"}
	repo := &splitTimelineRepo{
		MockNoteRepository: testutil.NewMockNoteRepository(),
		homeNotes: []*model.Note{
			{ID: ids[1], UserID: "u1", Visibility: model.NoteVisibilityPublic},
			{ID: ids[3], UserID: "u1", Visibility: model.NoteVisibilityPublic},
		},
		localNotes: []*model.Note{
			{ID: ids[2], UserID: "other", Visibility: model.NoteVisibilityPublic},
			{ID: ids[4], UserID: "other", Visibility: model.NoteVisibilityPublic},
		},
	}

	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	svc := NewService(fanout, repo, testutil.NewMockFollowingRepository())
	viewer := &model.User{ID: "u1"}

	// **limit を和集合より小さくする。** 同数だと最古 N と最新 N が区別
	// できず、truncate の向きを取り違える変異が素通りする。
	got, err := svc.HybridTimeline(context.Background(), viewer, "", ids[0], 2, TimelineFilter{})
	require.NoError(t, err)
	gotIDs := make([]string, 0, len(got))
	for _, n := range got {
		gotIDs = append(gotIDs, n.ID)
	}
	assert.Equal(t, []string{ids[1], ids[2]}, gotIDs,
		"home / local の和集合から**最古 N 件**を昇順で返す (最新 N 件ではない)")
}

// TestHomeTimeline_AscendingDoesNotSkipTheGap は shouldFallbackToDB が守る
// ものを固定する。
//
// **Redis が cursor 直後の範囲を持っていないとき、Redis の結果を使うと
// その範囲を飛ばす。** upstream は
// `sinceId != null && sinceId < oldestNoteId` で DB へ丸ごと倒す。
//
// 昇順では Redis の結果は全て sinceId より新しいので、この条件は常に成立する
// (= sinceId 単独のページングは必ず DB が処理する)。upstream の
// FanoutTimelineService.get も sinceId 単独では ASC で返すので、実装として
// 揃っている。
func TestHomeTimeline_AscendingDoesNotSkipTheGap(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	testRedis.FlushAll(context.Background())

	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	noteRepo := testutil.NewMockNoteRepository()
	svc := NewService(fanout, noteRepo, testutil.NewMockFollowingRepository())
	viewer := &model.User{ID: "u1"}
	name := HomeTimelineName(viewer.ID)

	ids := []string{"aqj00000000000000001", "aqj00000000000000002", "aqj00000000000000003", "aqj00000000000000004"}
	for _, id := range ids {
		noteRepo.Notes[id] = &model.Note{ID: id, UserID: "u1", Visibility: model.NoteVisibilityPublic}
	}
	// **Redis には 0003 / 0004 しか無い。** 0002 は DB にしかない。
	require.NoError(t, fanout.Push(context.Background(), name, ids[2], 100))
	require.NoError(t, fanout.Push(context.Background(), name, ids[3], 100))

	got, err := svc.HomeTimeline(context.Background(), viewer, "", ids[0], 5, TimelineFilter{})
	require.NoError(t, err)

	gotIDs := make([]string, 0, len(got))
	seen := make(map[string]int, len(got))
	for _, n := range got {
		gotIDs = append(gotIDs, n.ID)
		seen[n.ID]++
	}
	assert.Contains(t, gotIDs, ids[1],
		"cursor 直後の 0002 を飛ばしてはいけない (Redis が持っていないだけ)")
	for id, n := range seen {
		assert.Equal(t, 1, n, "同じ note が 2 回返っている: %s (order=%v)", id, gotIDs)
	}
}

// TestResolve_AscendingPreservesInputOrder は ephemeral merge が入力の向きを
// 壊さないことを固定する。無条件 DESC で並べ直すと昇順ページングが壊れる。
func TestResolve_AscendingPreservesInputOrder(t *testing.T) {
	svc, _ := newMergeService(t, []string{"aqk00000000000000002"})
	svc.SetEphemeralLookup(&stubEphemeral{notes: map[string]*model.Note{
		"aqk00000000000000003": ephNote("aqk00000000000000003"),
	}})

	// ASC の ids を渡す (昇順ページングで filterAndSort が返す形)。
	ids := []string{"aqk00000000000000002", "aqk00000000000000003"}
	notes, _, err := svc.resolve(context.Background(), ids)
	require.NoError(t, err)

	gotIDs := make([]string, 0, len(notes))
	for _, n := range notes {
		gotIDs = append(gotIDs, n.ID)
	}
	assert.Equal(t, ids, gotIDs, "入力 ids の順序を保つ (DESC に並べ直さない)")
}
