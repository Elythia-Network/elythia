package bubblegame

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/bubbleversus"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

type fakeUsers struct {
	users map[string]*model.User
	err   error
}

func (f fakeUsers) FindByID(id string) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, repository.ErrNotFound
}

type nopPub struct{}

func (nopPub) PublishInvited(string, *model.User, *bubbleversus.Match) {}
func (nopPub) PublishUser(string, string, any)                         {}
func (nopPub) PublishMatch(string, string, any)                        {}

var (
	vAlice = &model.User{ID: "alice", Username: "alice"}
	vBob   = &model.User{ID: "bob", Username: "bob"}
	vCarol = &model.User{ID: "carol", Username: "carol"}
)

type versusEnv struct {
	h     *VersusHandler
	svc   *bubbleversus.Service
	users *fakeUsers
	now   *time.Time
	mr    *miniredis.Miniredis
}

func newVersus(t *testing.T) *versusEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	// 落ちた Redis への再接続で待たないようにする。
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { rdb.Close() })
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := bubbleversus.NewService(rdb, nopPub{}, nil, gen)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return now })
	users := &fakeUsers{users: map[string]*model.User{"alice": vAlice, "bob": vBob, "carol": vCarol}}
	return &versusEnv{h: NewVersusHandler(svc, users), svc: svc, users: users, now: &now, mr: mr}
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	e, ok := decode(t, body)["error"].(map[string]any)
	require.True(t, ok, string(body))
	return e["code"].(string)
}

func (e *versusEnv) invite(t *testing.T) string {
	t.Helper()
	rec := post(e.h.Invite, `{"userId":"bob","gameMode":"square-friction"}`, vAlice)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decode(t, rec.Body.Bytes())["id"].(string)
}

func matchBody(id string) string { return fmt.Sprintf(`{"matchId":%q}`, id) }

func TestVersusInvite(t *testing.T) {
	e := newVersus(t)
	rec := post(e.h.Invite, `{"userId":"bob","gameMode":"square-friction"}`, vAlice)
	require.Equal(t, http.StatusOK, rec.Code)
	m := decode(t, rec.Body.Bytes())
	assert.Equal(t, "invited", m["status"])
	assert.Equal(t, "square-friction", m["gameMode"])
	assert.Nil(t, m["seed"], "the seed is decided on accept")
	assert.Equal(t, "alice", m["user1Id"])
	assert.Equal(t, "bob", m["user2"].(map[string]any)["id"])
	assert.Equal(t, "2026-09-29T12:00:00.000Z", m["createdAt"])
	assert.Nil(t, m["startAt"])

	for body, want := range map[string]string{
		`{}`:               "INVALID_PARAM",
		`{"userId":"bob"}`: "INVALID_PARAM",
		`[`:                "INVALID_PARAM",
		`{"userId":"nobody","gameMode":"normal"}`: "NO_SUCH_USER",
		`{"userId":"alice","gameMode":"normal"}`:  "TARGET_IS_YOURSELF",
		`{"userId":"bob","gameMode":"weird"}`:     "INVALID_GAME_MODE",
	} {
		rec := post(e.h.Invite, body, vAlice)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Equal(t, want, errorCode(t, rec.Body.Bytes()), body)
	}

	host := "remote.example"
	e.users.users["remote"] = &model.User{ID: "remote", Host: &host}
	rec = post(e.h.Invite, `{"userId":"remote","gameMode":"normal"}`, vAlice)
	assert.Equal(t, "TARGET_IS_REMOTE", errorCode(t, rec.Body.Bytes()))
	e.users.users["gone"] = &model.User{ID: "gone", IsSuspended: true}
	rec = post(e.h.Invite, `{"userId":"gone","gameMode":"normal"}`, vAlice)
	assert.Equal(t, "NO_SUCH_USER", errorCode(t, rec.Body.Bytes()))

	e.users.err = errors.New("db down")
	rec = post(e.h.Invite, `{"userId":"bob","gameMode":"normal"}`, vAlice)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "db down")
}

func TestVersusInvitationsAcceptShow(t *testing.T) {
	e := newVersus(t)
	mid := e.invite(t)

	rec := post(e.h.Invitations, `{}`, vBob)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, mid, list[0]["id"])
	assert.Equal(t, "alice", list[0]["user1"].(map[string]any)["username"])

	// 参加していない対局は「無い」と同じに見せる。
	rec = post(e.h.Show, matchBody(mid), vCarol)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_MATCH", errorCode(t, rec.Body.Bytes()))
	rec = post(e.h.Accept, matchBody(mid), vCarol)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = post(e.h.Accept, matchBody(mid), vBob)
	require.Equal(t, http.StatusOK, rec.Code)
	m := decode(t, rec.Body.Bytes())
	assert.Equal(t, "accepted", m["status"])
	assert.Len(t, m["seed"], 32)

	rec = post(e.h.Accept, matchBody(mid), vBob)
	assert.Equal(t, "INVALID_STATE", errorCode(t, rec.Body.Bytes()))

	rec = post(e.h.Show, matchBody(mid), vAlice)
	require.Equal(t, http.StatusOK, rec.Code)
	m = decode(t, rec.Body.Bytes())
	assert.Equal(t, false, m["user1Ready"])
	assert.Nil(t, m["user1Result"])

	// 形の違う ID は引かずに 404、欠けていれば 400。
	for body, want := range map[string]int{
		matchBody("NOT-AN-ID"):       http.StatusNotFound,
		`{"matchId":"abc\u0000def"}`: http.StatusNotFound,
		matchBody("zzzz"):            http.StatusNotFound,
		`{}`:                         http.StatusBadRequest,
	} {
		assert.Equal(t, want, post(e.h.Show, body, vAlice).Code, body)
	}
}

func TestVersusDeclineCancel(t *testing.T) {
	e := newVersus(t)
	mid := e.invite(t)
	assert.Equal(t, http.StatusNotFound, post(e.h.Decline, matchBody(mid), vAlice).Code, "only the invitee declines")
	assert.Equal(t, http.StatusNoContent, post(e.h.Decline, matchBody(mid), vBob).Code)
	assert.Equal(t, http.StatusNotFound, post(e.h.Show, matchBody(mid), vAlice).Code)

	mid = e.invite(t)
	assert.Equal(t, http.StatusNotFound, post(e.h.Cancel, matchBody(mid), vBob).Code, "only the inviter cancels an invitation")
	assert.Equal(t, http.StatusNoContent, post(e.h.Cancel, matchBody(mid), vAlice).Code)
	assert.Equal(t, http.StatusBadRequest, post(e.h.Cancel, `{}`, vAlice).Code)
	assert.Equal(t, http.StatusBadRequest, post(e.h.Decline, `{}`, vAlice).Code)
}

func TestVersusReport(t *testing.T) {
	e := newVersus(t)
	mid := e.invite(t)
	require.Equal(t, http.StatusOK, post(e.h.Accept, matchBody(mid), vBob).Code)

	report := func(u *model.User, body string) (int, map[string]any) {
		rec := post(e.h.Report, fmt.Sprintf(`{"matchId":%q,%s}`, mid, body), u)
		return rec.Code, decode(t, rec.Body.Bytes())
	}
	code, _ := report(vAlice, `"score":1,"frame":1,"reason":"gameOver","logs":[]`)
	assert.Equal(t, http.StatusBadRequest, code, "not started yet")

	ctx := t.Context()
	_, err := e.svc.Ready(ctx, "alice", mid, true)
	require.NoError(t, err)
	_, err = e.svc.Ready(ctx, "bob", mid, true)
	require.NoError(t, err)
	*e.now = e.now.Add(bubbleversus.Countdown)

	code, body := report(vAlice, `"score":1,"frame":1,"reason":"win","logs":[]`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "INVALID_REPORT", body["error"].(map[string]any)["code"])
	code, body = report(vAlice, `"score":1,"frame":1,"reason":"timeUp","logs":[]`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "NOT_YET", body["error"].(map[string]any)["code"])

	code, body = report(vBob, `"score":12,"frame":900,"reason":"gameOver","logs":[[40,0,200]]`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ended", body["status"])
	assert.Equal(t, "alice", body["winnerId"])
	assert.Equal(t, "gameOver", body["reason"])
	assert.NotNil(t, body["endedAt"])
	assert.Equal(t, map[string]any{"score": float64(12), "frame": float64(900), "reason": "gameOver"}, body["user2Result"])
	assert.NotContains(t, body, "logs")

	assert.Equal(t, http.StatusNotFound, post(e.h.Report, `{"matchId":"`+mid+`","reason":"gameOver"}`, vCarol).Code)
}

// 相手のユーザーが消えていても対局は見られる (user は null)。
func TestVersusPack_MissingUser(t *testing.T) {
	e := newVersus(t)
	mid := e.invite(t)
	delete(e.users.users, "bob")
	rec := post(e.h.Show, matchBody(mid), vAlice)
	require.Equal(t, http.StatusOK, rec.Code)
	m := decode(t, rec.Body.Bytes())
	assert.Nil(t, m["user2"])
	assert.Equal(t, "bob", m["user2Id"])

	e.users.err = errors.New("db down")
	assert.Equal(t, http.StatusInternalServerError, post(e.h.Show, matchBody(mid), vAlice).Code)
	assert.Equal(t, http.StatusInternalServerError, post(e.h.Invitations, `{}`, vBob).Code)
}

// Redis が落ちていれば 500 (「無い」に化けさせない)。
func TestVersus_RedisDown(t *testing.T) {
	e := newVersus(t)
	mid := e.invite(t)
	e.mr.Close()
	for name, h := range map[string]func() int{
		"show":        func() int { return post(e.h.Show, matchBody(mid), vAlice).Code },
		"invitations": func() int { return post(e.h.Invitations, `{}`, vBob).Code },
		"accept":      func() int { return post(e.h.Accept, matchBody(mid), vBob).Code },
		"decline":     func() int { return post(e.h.Decline, matchBody(mid), vBob).Code },
		"cancel":      func() int { return post(e.h.Cancel, matchBody(mid), vAlice).Code },
		"report":      func() int { return post(e.h.Report, `{"matchId":"`+mid+`","reason":"gameOver"}`, vAlice).Code },
		"invite":      func() int { return post(e.h.Invite, `{"userId":"carol","gameMode":"normal"}`, vAlice).Code },
	} {
		assert.Equal(t, http.StatusInternalServerError, h(), name)
	}
	// 形の違う ID は Redis を引かずに「無い」とする。
	assert.Equal(t, http.StatusNotFound, post(e.h.Show, matchBody("NOT-AN-ID"), vAlice).Code)
}

// ulid の ID (大文字を含む) でも対局を引ける。
func TestVersus_ULIDMatchID(t *testing.T) {
	e := newVersus(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	gen, err := id.NewGenerator("ulid")
	require.NoError(t, err)
	e.h = NewVersusHandler(bubbleversus.NewService(rdb, nopPub{}, nil, gen), e.users)
	mid := e.invite(t)
	require.Regexp(t, `[A-Z]`, mid)
	assert.Equal(t, http.StatusOK, post(e.h.Show, matchBody(mid), vAlice).Code)
}

type versionRecords struct{ versions []*int }

func (v *versionRecords) SaveReport(_ repository.BubbleVersusMatchBase, _ int, r repository.BubbleVersusReport) error {
	v.versions = append(v.versions, r.GameVersion)
	return nil
}

func (*versionRecords) Finish(repository.BubbleVersusMatchBase, time.Time, *string, string) error {
	return nil
}

// エンジンの版は記録まで届く (#3232)。範囲外は 400。
func TestVersusReport_GameVersion(t *testing.T) {
	e := newVersus(t)
	recs := &versionRecords{}
	e.svc.SetRecordStore(recs)
	mid := e.invite(t)
	require.Equal(t, http.StatusOK, post(e.h.Accept, matchBody(mid), vBob).Code)
	ctx := t.Context()
	_, err := e.svc.Ready(ctx, "alice", mid, true)
	require.NoError(t, err)
	_, err = e.svc.Ready(ctx, "bob", mid, true)
	require.NoError(t, err)
	*e.now = e.now.Add(bubbleversus.Countdown)

	rec := post(e.h.Report, fmt.Sprintf(`{"matchId":%q,"score":1,"frame":1,"reason":"gameOver","logs":[],"gameVersion":0}`, mid), vBob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_REPORT", errorCode(t, rec.Body.Bytes()))

	rec = post(e.h.Report, fmt.Sprintf(`{"matchId":%q,"score":1,"frame":1,"reason":"gameOver","logs":[],"gameVersion":4}`, mid), vBob)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = post(e.h.Report, fmt.Sprintf(`{"matchId":%q,"score":2,"frame":2,"reason":"opponentEnded","logs":[]}`, mid), vAlice)
	require.Equal(t, http.StatusOK, rec.Code, "終局後の opponentEnded は受ける")

	require.Len(t, recs.versions, 2)
	require.NotNil(t, recs.versions[0])
	assert.Equal(t, 4, *recs.versions[0])
	assert.Nil(t, recs.versions[1], "版を送らない報告は nil のまま")
}
