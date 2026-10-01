package bubblegame

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// memRecords は記録の repository のメモリ上の代わり。repository 自体の振る舞い
// (SQL) は internal/repository の実 DB テストが見る。ここで見るのは見える範囲と形。
type memRecords struct {
	recs        map[string]*model.BubbleGameVersusRecord
	publicCalls []string
}

func (m *memRecords) SaveReport(repository.BubbleVersusMatchBase, int, repository.BubbleVersusReport) error {
	return nil
}
func (m *memRecords) Finish(repository.BubbleVersusMatchBase, time.Time, *string, string) error {
	return nil
}
func (m *memRecords) FindByID(id string) (*model.BubbleGameVersusRecord, error) {
	if r, ok := m.recs[id]; ok {
		return r, nil
	}
	return nil, repository.ErrNotFound
}
func (m *memRecords) ListByUser(userID string, publicOnly bool, untilID string, limit int) ([]*model.BubbleGameVersusRecord, error) {
	var out []*model.BubbleGameVersusRecord
	for _, r := range m.recs {
		if r.User1ID != userID && r.User2ID != userID {
			continue
		}
		if r.EndedAt == nil || (publicOnly && !(r.User1Public && r.User2Public)) {
			continue
		}
		if untilID != "" && r.ID >= untilID {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (m *memRecords) SetPublic(id, userID string, public bool) (bool, error) {
	m.publicCalls = append(m.publicCalls, id+":"+userID)
	r, ok := m.recs[id]
	if !ok {
		return false, nil
	}
	switch userID {
	case r.User1ID:
		r.User1Public = public
	case r.User2ID:
		r.User2Public = public
	default:
		return false, nil
	}
	return true, nil
}
func (m *memRecords) DeleteExpired(time.Time) (int64, error) { return 0, nil }

type blockPairs struct {
	pairs map[[2]string]bool
	err   error
}

func (b blockPairs) IsBlocked(blocker, blockee string) (bool, error) {
	return b.pairs[[2]string{blocker, blockee}], b.err
}

func endedRecord(id, u1, u2 string, pub1, pub2 bool) *model.BubbleGameVersusRecord {
	ended := time.Date(2026, 10, 1, 12, 5, 0, 0, time.UTC)
	score, frame, reason, ver := 10, 900, "gameOver", 4
	return &model.BubbleGameVersusRecord{
		ID: id, User1ID: u1, User2ID: u2, GameMode: "normal", Seed: "seed-" + id,
		StartedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), EndedAt: &ended,
		WinnerID: &u1, Reason: &reason,
		User1Score: &score, User1Frame: &frame, User1Reason: &reason, User1GameVersion: &ver,
		User1Logs:   datatypes.JSON(`[[1,0,5]]`),
		User1Public: pub1, User2Public: pub2,
		// 本物の repository は参加者を preload する。
		User1: &model.User{ID: u1, Username: u1}, User2: &model.User{ID: u2, Username: u2},
	}
}

func newRecordEnv(t *testing.T, blocks blockPairs, recs ...*model.BubbleGameVersusRecord) (*versusEnv, *memRecords) {
	t.Helper()
	e := newVersus(t)
	m := &memRecords{recs: map[string]*model.BubbleGameVersusRecord{}}
	for _, r := range recs {
		m.recs[r.ID] = r
	}
	e.h.SetRecords(m, blocks)
	return e, m
}

func decodeList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out
}

func listIDs(list []map[string]any) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r["id"].(string))
	}
	return out
}

func TestVersusHistory(t *testing.T) {
	e, _ := newRecordEnv(t, blockPairs{},
		endedRecord("m3", "alice", "bob", true, true),     // 公開
		endedRecord("m2", "alice", "bob", true, false),    // 片方だけ公開
		endedRecord("m1", "carol", "alice", false, false), // 非公開
	)

	rec := post(e.h.History, `{}`, vAlice)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	own := decodeList(t, rec.Body.Bytes())
	assert.Equal(t, []string{"m3", "m2", "m1"}, listIDs(own), "自分の履歴は非公開も含む")
	assert.Nil(t, own[0]["user1Logs"], "一覧に記録は載せない")
	assert.NotContains(t, own[0], "seed", "一覧にシードは載せない")
	assert.Equal(t, true, own[0]["isPublic"])
	assert.Equal(t, false, own[1]["isPublic"], "片方だけでは公開ではない")

	rec = post(e.h.History, `{"userId":"alice"}`, vCarol)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"m3"}, listIDs(decodeList(t, rec.Body.Bytes())), "他人の履歴は両者が公開にしたものだけ")

	rec = post(e.h.History, `{"userId":"alice","untilId":"m3","limit":1}`, vAlice)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"m2"}, listIDs(decodeList(t, rec.Body.Bytes())))

	rec = post(e.h.History, `{"userId":"nobody"}`, vAlice)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_USER", errorCode(t, rec.Body.Bytes()))
	rec = post(e.h.History, `{"untilId":"m\u0000"}`, vAlice)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らないカーソルは 400")
	rec = post(e.h.History, `{"limit":0}`, vAlice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 参加者のどちらかとブロック関係 (どちらの向きでも) にある閲覧者には見せない。
func TestVersusHistory_Blocks(t *testing.T) {
	for name, pair := range map[string][2]string{
		"viewer blocks player": {"carol", "bob"},
		"player blocks viewer": {"bob", "carol"},
		"viewer blocks target": {"carol", "alice"},
		"target blocks viewer": {"alice", "carol"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := newRecordEnv(t, blockPairs{pairs: map[[2]string]bool{pair: true}},
				endedRecord("m1", "alice", "bob", true, true))
			rec := post(e.h.History, `{"userId":"alice"}`, vCarol)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Empty(t, decodeList(t, rec.Body.Bytes()))
			rec = post(e.h.ShowRecord, matchBody("m1"), vCarol)
			assert.Equal(t, http.StatusNotFound, rec.Code)
		})
	}
}

func TestVersusRecord(t *testing.T) {
	private := endedRecord("m1", "alice", "bob", true, false)
	public := endedRecord("m2", "alice", "bob", true, true)
	running := endedRecord("m3", "alice", "bob", true, true)
	running.EndedAt = nil
	e, _ := newRecordEnv(t, blockPairs{}, private, public, running)

	rec := post(e.h.ShowRecord, matchBody("m1"), vBob)
	require.Equal(t, http.StatusOK, rec.Code, "参加者は非公開でも見られる")
	got := decode(t, rec.Body.Bytes())
	assert.Equal(t, "seed-m1", got["seed"])
	assert.Equal(t, []any{[]any{float64(1), float64(0), float64(5)}}, got["user1Logs"])
	assert.Nil(t, got["user2Logs"], "届いていない側の記録は null")
	assert.Nil(t, got["user2Result"])
	r1 := got["user1Result"].(map[string]any)
	assert.Equal(t, float64(10), r1["score"])
	assert.Equal(t, float64(4), r1["gameVersion"])
	assert.Equal(t, "alice", got["winnerId"])
	assert.Equal(t, "2026-10-01T12:05:00.000Z", got["endedAt"])
	assert.Equal(t, "alice", got["user1"].(map[string]any)["id"])

	rec = post(e.h.ShowRecord, matchBody("m1"), vCarol)
	assert.Equal(t, http.StatusNotFound, rec.Code, "非公開は参加者以外に見せない")
	assert.Equal(t, "NO_SUCH_MATCH", errorCode(t, rec.Body.Bytes()))
	rec = post(e.h.ShowRecord, matchBody("m2"), vCarol)
	assert.Equal(t, http.StatusOK, rec.Code, "両者が公開にしたものは見られる")
	rec = post(e.h.ShowRecord, matchBody("m3"), vAlice)
	assert.Equal(t, http.StatusNotFound, rec.Code, "終局していない対局は記録として見せない")
	rec = post(e.h.ShowRecord, matchBody("none"), vAlice)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ブロックを判定できなければ見せない (fail-closed)。
func TestVersusRecord_BlockCheckError(t *testing.T) {
	e, _ := newRecordEnv(t, blockPairs{err: errors.New("db down")}, endedRecord("m1", "alice", "bob", true, true))
	rec := post(e.h.ShowRecord, matchBody("m1"), vCarol)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	rec = post(e.h.ShowRecord, matchBody("m1"), vAlice)
	assert.Equal(t, http.StatusOK, rec.Code, "参加者はブロックを見ない")
}

func TestVersusSetPublic(t *testing.T) {
	e, m := newRecordEnv(t, blockPairs{}, endedRecord("m1", "alice", "bob", false, false))

	rec := post(e.h.SetPublic, `{"matchId":"m1","isPublic":true}`, vBob)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, m.recs["m1"].User2Public)
	assert.False(t, m.recs["m1"].User1Public, "相手の意思は変えない")

	rec = post(e.h.SetPublic, `{"matchId":"m1","isPublic":true}`, vCarol)
	assert.Equal(t, http.StatusNotFound, rec.Code, "参加していない対局は変えられない")
	rec = post(e.h.SetPublic, `{"matchId":"m1"}`, vAlice)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "isPublic は必須")
	rec = post(e.h.SetPublic, `{"matchId":"m-1","isPublic":true}`, vAlice)
	assert.Equal(t, http.StatusNotFound, rec.Code, "形の違う matchId は引く前に 404")
	assert.Equal(t, []string{"m1:bob", "m1:carol"}, m.publicCalls)
}

// 1 ページ分すべてが見せない対局でも、引き直してその先の見える対局を返す。
// 空のページを返すと、クライアントはそこで終わりだと判断して古い対局に届かない。
func TestVersusHistory_RefillsSkippedRecords(t *testing.T) {
	recs := []*model.BubbleGameVersusRecord{endedRecord("m00", "alice", "dave", true, true)}
	for i := 1; i <= 25; i++ {
		recs = append(recs, endedRecord(fmt.Sprintf("m%02d", i), "alice", "bob", true, true))
	}
	// carol は bob をブロックしているので、alice と bob の対局は見えない。
	e, _ := newRecordEnv(t, blockPairs{pairs: map[[2]string]bool{{"carol", "bob"}: true}}, recs...)
	rec := post(e.h.History, `{"userId":"alice","limit":10}`, vCarol)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"m00"}, listIDs(decodeList(t, rec.Body.Bytes())))

	// limit 件に達したらそこで止める (次のページは最後の id から)。
	e2, _ := newRecordEnv(t, blockPairs{}, recs...)
	rec = post(e2.h.History, `{"userId":"alice","limit":10}`, vCarol)
	require.Equal(t, http.StatusOK, rec.Code)
	got := listIDs(decodeList(t, rec.Body.Bytes()))
	assert.Len(t, got, 10)
	assert.Equal(t, "m25", got[0])
}

// ブロックを判定できない (配線されていない) なら、参加者以外には見せない。
func TestVersusRecord_NoBlockCheckerIsClosed(t *testing.T) {
	e, _ := newRecordEnv(t, blockPairs{}, endedRecord("m1", "alice", "bob", true, true))
	e.h.SetRecords(e.h.records, nil)
	assert.Equal(t, http.StatusNotFound, post(e.h.ShowRecord, matchBody("m1"), vCarol).Code)
	assert.Equal(t, http.StatusOK, post(e.h.ShowRecord, matchBody("m1"), vAlice).Code, "参加者は見られる")
}

// 退会の処理中・凍結された参加者がいる対局は、公開でも参加者以外に見せない。
func TestVersusRecord_DeletedOrSuspendedParticipantHidesFromOthers(t *testing.T) {
	for name, mutate := range map[string]func(u *model.User){
		"deleted":   func(u *model.User) { u.IsDeleted = true },
		"suspended": func(u *model.User) { u.IsSuspended = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := endedRecord("m1", "alice", "bob", true, true)
			mutate(r.User2)
			e, _ := newRecordEnv(t, blockPairs{}, r)
			assert.Equal(t, http.StatusNotFound, post(e.h.ShowRecord, matchBody("m1"), vCarol).Code)
			rec := post(e.h.History, `{"userId":"alice"}`, vCarol)
			assert.Empty(t, decodeList(t, rec.Body.Bytes()))
			assert.Equal(t, http.StatusOK, post(e.h.ShowRecord, matchBody("m1"), vAlice).Code, "参加者は見られる")
		})
	}
}

// 引き直しの途中で limit 件に達したら、それ以上は足さない。
func TestVersusHistory_RefillStopsAtLimit(t *testing.T) {
	var recs []*model.BubbleGameVersusRecord
	for i := 1; i <= 30; i++ {
		opp := "dave"
		// 新しい方の 10 件のうち 7 件は bob との対局 (carol には見えない)。
		if i > 20 && i <= 27 {
			opp = "bob"
		}
		recs = append(recs, endedRecord(fmt.Sprintf("m%02d", i), "alice", opp, true, true))
	}
	e, _ := newRecordEnv(t, blockPairs{pairs: map[[2]string]bool{{"carol", "bob"}: true}}, recs...)
	rec := post(e.h.History, `{"userId":"alice","limit":10}`, vCarol)
	require.Equal(t, http.StatusOK, rec.Code)
	got := listIDs(decodeList(t, rec.Body.Bytes()))
	assert.Equal(t, []string{"m30", "m29", "m28", "m20", "m19", "m18", "m17", "m16", "m15", "m14"}, got)
}
