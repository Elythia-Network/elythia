package antennas

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// zsetMembers は antenna の zset に残っている ID を返す。
func zsetMembers(t *testing.T, key string) []string {
	t.Helper()
	ids, err := testRedis.Client.ZRange(context.Background(), key, 0, -1).Result()
	require.NoError(t, err)
	return ids
}

// wirePrimary は prune の primary 確認を noteRepo に配線する。配線しないと
// prune は一切走らない (fail-safe)。
func wirePrimary(h *Handler, noteRepo *testutil.MockNoteRepository) {
	h.svc.SetPrimaryNoteExistence(noteRepo)
}

// TestNotes_PrunesDanglingIDs は #2719 の自己修復を固定する。
//
// antenna には TTL が無く、押し出しは新規 push に依存する (pushNote が毎回
// ZRemRangeByRank で上限 200 を維持する)。マッチが止まった antenna では
// 宙吊り ID が残り続ける。
func TestNotes_PrunesDanglingIDs(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	wirePrimary(h, noteRepo)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	alive, gone1, gone2 := "n_alive", "n_gone1", "n_gone2"
	noteRepo.Notes[alive] = &model.Note{ID: alive, UserID: "alice"}

	for _, id := range []string{alive, gone1, gone2} {
		seedAntennaNote(t, "antennaTimeline:a1", id)
	}
	require.Len(t, zsetMembers(t, "antennaTimeline:a1"), 3)

	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	assert.ElementsMatch(t, []string{alive}, zsetMembers(t, "antennaTimeline:a1"),
		"DB から引けない ID は zset から除かれる")
}

// TestNotes_KeepsRowsPresentOnPrimary は #2719 のレビューで見つかった穴を
// 固定する。
//
// mk-go はリードレプリカを対応しており、読み取りはレプリカに振られる。
// 複製前の行は引けないので、そこで prune すると生きている note の ID が
// 恒久的に消える。primary で確かめてから消す。
func TestNotes_KeepsRowsPresentOnPrimary(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	// noteRepo (= レプリカ相当) には無いが primary には在る、という状況を作る。
	h.svc.SetPrimaryNoteExistence(&stubPrimary{existing: map[string]bool{"n_lag": true}})
	noteRepo.Notes["n_seen"] = &model.Note{ID: "n_seen", UserID: "alice"}

	seedAntennaNote(t, "antennaTimeline:a1", "n_seen")
	seedAntennaNote(t, "antennaTimeline:a1", "n_lag")
	seedAntennaNote(t, "antennaTimeline:a1", "n_gone")

	c, _ := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))

	assert.ElementsMatch(t, []string{"n_seen", "n_lag"}, zsetMembers(t, "antennaTimeline:a1"),
		"primary に在る行は消さない")
}

// stubPrimary は primary 側の存在確認を差し替える。
type stubPrimary struct{ existing map[string]bool }

func (s *stubPrimary) ExistingNoteIDsOnPrimary(ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		if s.existing[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// TestNotes_DoesNotPruneFilteredNotes は**最重要の不変条件**を固定する。
//
// visibility / mute / block / hardMute / blockedHost / suspended で落ちた note は
// **生きている**。filter 後の集合で差分を取ると、消してはいけない ID を消す
// (#2718 が `ApplyFilter` の前の resolved から境界を取ったのと同じ理由)。
//
// **このテストは順序変異を捕まえない。** primary 確認が「filter で落ちた note は
// DB に在る」と答えるので、順序を誤っても結果的に守られる。順序そのものは
// TestNotes_PruneOrderIsIndependentOfPrimaryCheck が固定する。ここは
// 「primary 確認があれば filter 後でも壊れない」ことの記録。
func TestNotes_DoesNotPruneFilteredNotes(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	wirePrimary(h, noteRepo)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}

	mine, blocked, followersOnly := "n_mine", "n_blocked", "n_followers"
	noteRepo.Notes[mine] = &model.Note{ID: mine, UserID: "alice", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes[blocked] = &model.Note{ID: blocked, UserID: "bob", Visibility: model.NoteVisibilityPublic}
	// alice がフォローしていない carol の followers 限定 note。FilterVisible で落ちる。
	noteRepo.Notes[followersOnly] = &model.Note{ID: followersOnly, UserID: "carol", Visibility: model.NoteVisibilityFollowers}

	// LoadMuteBlockSets は `ListBlockerIDs(viewer.ID)` (= 閲覧者をブロックして
	// いる側) を見るので、**bob が alice をブロック**する向きで作る。
	blocking := testutil.NewMockBlockingRepository()
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "bob", BlockeeID: "alice"}))
	h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), blocking, testutil.NewMockChannelMutingRepository())

	for _, id := range []string{mine, blocked, followersOnly} {
		seedAntennaNote(t, "antennaTimeline:a1", id)
	}

	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, mine, "自分の public note は返る (レスポンスが空でない)")
	assert.NotContains(t, body, blocked, "block で落ちる")
	assert.NotContains(t, body, followersOnly, "visibility で落ちる")

	assert.ElementsMatch(t, []string{mine, blocked, followersOnly},
		zsetMembers(t, "antennaTimeline:a1"),
		"filter で落ちた note は生きているので zset から消してはいけない")
}

// TestNotes_AllDanglingRecoversOnNextFetch は、issue が挙げた「行き止まり」が
// どこまで解消するかを固定する。
//
// overFetch 窓が全部宙吊りだと**そのレスポンスは空のまま**で、prune が効くのは
// 次回以降のフェッチ。1 回目で枠が空き、2 回目で奥の実在 note が返る。
func TestNotes_AllDanglingRecoversOnNextFetch(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	wirePrimary(h, noteRepo)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}

	// zset は辞書順で並ぶので、ID の接頭辞で新旧を作る。
	deep := "a_deep"
	noteRepo.Notes[deep] = &model.Note{ID: deep, UserID: "alice"}
	seedAntennaNote(t, "antennaTimeline:a1", deep)
	// deep より「新しい」宙吊りを overFetch (limit*2) 以上積んで押し出す。
	for i := 0; i < 25; i++ {
		seedAntennaNote(t, "antennaTimeline:a1", fmt.Sprintf("z_gone%02d", i))
	}

	c1, rec1 := newReq(t, `{"antennaId":"a1","limit":10}`)
	setUser(c1, "alice")
	require.NoError(t, h.Notes(c1))
	assert.Equal(t, "[]", strings.TrimSpace(rec1.Body.String()), "1 回目は空 (行き止まり)")

	c2, rec2 := newReq(t, `{"antennaId":"a1","limit":10}`)
	setUser(c2, "alice")
	require.NoError(t, h.Notes(c2))
	assert.Contains(t, rec2.Body.String(), deep, "prune で枠が空き、次のフェッチで奥の note が返る")
}

// TestNotes_PruneOrderIsIndependentOfPrimaryCheck は、prune を filter の前に
// 置く不変条件を primary 確認から**独立に**固定する。
//
// primary 確認は「本当に DB に無いか」を見るので、順序を誤って filter 後の
// 集合を渡しても結果的に守られる。ただしそれは primary への無駄な問い合わせを
// 増やすし、確認を外した瞬間に破壊的になる。ここでは primary が「どれも無い」と
// 答える状況を作り、**順序だけ**を判定対象にする。
func TestNotes_PruneOrderIsIndependentOfPrimaryCheck(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	// primary は何も返さない = 「引けなかったものは全部消してよい」と答える。
	h.svc.SetPrimaryNoteExistence(&stubPrimary{})

	mine, blocked, followersOnly := "n_mine", "n_blocked", "n_followers"
	noteRepo.Notes[mine] = &model.Note{ID: mine, UserID: "alice", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes[blocked] = &model.Note{ID: blocked, UserID: "bob", Visibility: model.NoteVisibilityPublic}
	// alice がフォローしていない carol の followers 限定 note。FilterVisible で落ちる。
	noteRepo.Notes[followersOnly] = &model.Note{ID: followersOnly, UserID: "carol", Visibility: model.NoteVisibilityFollowers}
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))

	blocking := testutil.NewMockBlockingRepository()
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "bob", BlockeeID: "alice"}))
	h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), blocking, testutil.NewMockChannelMutingRepository())

	for _, id := range []string{mine, blocked, followersOnly} {
		seedAntennaNote(t, "antennaTimeline:a1", id)
	}

	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	// 到達の確認。500 で返っても zset は無傷なので、これが無いと空振りする。
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), mine)

	assert.ElementsMatch(t, []string{mine, blocked, followersOnly}, zsetMembers(t, "antennaTimeline:a1"),
		"filter で落ちた note を prune 対象にしていない (順序が正しい)")
}
