package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFeedWindowHandler wires a feed for author u whose notes n_old (2026-01-01)
// and n_new (2026-03-01) sit on either side of a 2026-02-01 boundary, with now
// fixed at 2026-04-01.
func newFeedWindowHandler(u *model.User) *feedHandler {
	h := newFeedTestHandler([]*model.Note{
		{ID: "n_new", UserID: u.ID, Text: strp("new"), Visibility: model.NoteVisibilityPublic},
		{ID: "n_old", UserID: u.ID, Text: strp("old"), Visibility: model.NoteVisibilityPublic},
	})
	h.users = stubFeedUsers{users: map[string]*model.User{u.Username: u}}
	dates := map[string]time.Time{
		"n_new": time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		"n_old": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	h.parseTime = func(id string) (time.Time, error) { return dates[id], nil }
	h.now = func() time.Time { return time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) }
	return h
}

// upstream 2026.10.0 FeedService: makeNotesHiddenBefore /
// makeNotesFollowersOnlyBefore の境界より古い note はフィードに出さない。
// 絶対時刻 (正の値) と相対秒 (0 以下) の両方を確かめる。
func TestFeed_HidesNotesByAuthorTimeWindow(t *testing.T) {
	boundary := int(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix())
	// now (4/1) の 45 日前 = 2/15。n_old (1/1) は超過し、n_new (3/1) は範囲内。
	relative := -45 * 24 * 60 * 60
	cases := []struct {
		name string
		user *model.User
	}{
		{"hiddenBefore absolute", &model.User{ID: "u1", Username: "alice", MakeNotesHiddenBefore: intp(boundary)}},
		{"hiddenBefore relative", &model.User{ID: "u1", Username: "alice", MakeNotesHiddenBefore: intp(relative)}},
		{"followersOnlyBefore absolute", &model.User{ID: "u1", Username: "alice", MakeNotesFollowersOnlyBefore: intp(boundary)}},
		{"followersOnlyBefore relative", &model.User{ID: "u1", Username: "alice", MakeNotesFollowersOnlyBefore: intp(relative)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFeedWindowHandler(tc.user)
			for _, render := range []func(echo.Context, string) error{h.RSS, h.Atom, h.JSON} {
				rec := doFeedReq(t, render, "alice")
				require.Equal(t, http.StatusOK, rec.Code)
				body := rec.Body.String()
				assert.Contains(t, body, "https://example.test/notes/n_new", "境界より新しい note は出す")
				assert.NotContains(t, body, "https://example.test/notes/n_old", "境界より古い note は出さない")
			}
		})
	}
}

// 設定が無ければ古い note も出る (フィルタが常に落とすわけではないことの対照)。
func TestFeed_NoTimeWindowKeepsAllNotes(t *testing.T) {
	h := newFeedWindowHandler(&model.User{ID: "u1", Username: "alice"})
	body := doFeedReq(t, h.RSS, "alice").Body.String()
	assert.Contains(t, body, "https://example.test/notes/n_new")
	assert.Contains(t, body, "https://example.test/notes/n_old")
}

// 作成時刻が読めない note は、期間設定のある著者なら隠す側に倒す。
func TestFeed_UnparseableDateFailsClosedUnderTimeWindow(t *testing.T) {
	h := newFeedWindowHandler(&model.User{ID: "u1", Username: "alice", MakeNotesHiddenBefore: intp(1)})
	h.parseTime = func(string) (time.Time, error) { return time.Time{}, assert.AnError }
	body := doFeedReq(t, h.RSS, "alice").Body.String()
	assert.NotContains(t, body, "/notes/n_new")
	assert.NotContains(t, body, "/notes/n_old")
}
