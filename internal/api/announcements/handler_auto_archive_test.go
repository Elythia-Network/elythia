package announcements_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家 2026.10.1 (#17704) のお知らせの自動アーカイブ (#3809)。

const (
	createInvalidAutoArchiveAtID = "2a892bd5-487d-46a2-a5fe-3d85ad51defe"
	updateInvalidAutoArchiveAtID = "01b83d7b-2fd5-4d7c-86c4-d03144d16355"
)

func decodeError(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Error
}

func onlyItem(t *testing.T, repo map[string]*model.Announcement) *model.Announcement {
	t.Helper()
	require.Len(t, repo, 1)
	for _, a := range repo {
		return a
	}
	return nil
}

func TestAdminCreate_AutoArchiveAtStoresFutureTime(t *testing.T) {
	h, repo := newTestHandler(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	h.SetClock(func() time.Time { return now })
	at := now.Add(time.Hour)
	bc := &fakeBroadcastPub{}
	h.SetBroadcastPublisher(bc)

	rec := doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":%d}`, at.UnixMilli()), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	a := onlyItem(t, repo.Items)
	require.NotNil(t, a.AutoArchiveAt)
	assert.True(t, a.AutoArchiveAt.Equal(at))
	assert.True(t, a.IsActive)
	assert.Equal(t, []string{"announcementCreated"}, bc.events, "期限前に作ったものは本家と同じく流す")

	// 本家の create の応答は AnnouncementEntityService.pack で、autoArchiveAt を持たない。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotContains(t, resp, "autoArchiveAt")
	assert.NotContains(t, resp, "isActive")
}

func TestAdminCreate_AutoArchiveAtNullMeansNoExpiry(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":null}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a := onlyItem(t, repo.Items)
	assert.Nil(t, a.AutoArchiveAt)
	assert.True(t, a.IsActive)
}

func TestAdminCreate_AutoArchiveAtRejectsPastNowAndNaN(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for name, ms := range map[string]int64{
		"past":    now.Add(-time.Second).UnixMilli(),
		"now":     now.UnixMilli(),
		"invalid": 8_640_000_000_000_001,
	} {
		t.Run(name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			h.SetClock(func() time.Time { return now })
			rec := doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":%d}`, ms), nil)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			e := decodeError(t, rec.Body.Bytes())
			assert.Equal(t, "INVALID_AUTO_ARCHIVE_AT", e["code"])
			assert.Equal(t, createInvalidAutoArchiveAtID, e["id"])
			assert.Equal(t, "Invalid auto archive date.", e["message"])
			assert.Empty(t, repo.Items)
		})
	}
}

func TestAdminCreate_AutoArchiveAtAcceptsLargestDate(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":8640000000000000}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a := onlyItem(t, repo.Items)
	require.NotNil(t, a.AutoArchiveAt)
	assert.Equal(t, int64(8_640_000_000_000_000), a.AutoArchiveAt.UnixMilli())
}

func TestAdminCreate_AutoArchiveAtRejectsNonInteger(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":"tomorrow"}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_PARAM", decodeError(t, rec.Body.Bytes())["code"])
	assert.Empty(t, repo.Items)
}

// 検査から INSERT までの間に期限が来たら、本家と同じく非アクティブとして作り、
// announcementCreated を流さない。
func TestAdminCreate_ExpiredDuringCreateDoesNotPublish(t *testing.T) {
	h, repo := newTestHandler(t)
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := start.Add(time.Millisecond)
	// 1 回のリクエストで時計を 2 回読む (検査と isActive)。検査では期限の前、
	// isActive の判定では期限ちょうどを返す。
	calls := 0
	h.SetClock(func() time.Time {
		calls++
		if calls%2 == 1 {
			return start
		}
		return at
	})
	bc := &fakeBroadcastPub{}
	h.SetBroadcastPublisher(bc)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":%d}`, at.UnixMilli()), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a := onlyItem(t, repo.Items)
	assert.False(t, a.IsActive)
	assert.Empty(t, bc.events, "作成の時点で非アクティブなら broadcast しない")

	rec = doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"userId":"u1","autoArchiveAt":%d}`, at.UnixMilli()), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, pub.calls, "作成の時点で非アクティブなら main にも流さない")
	assert.Len(t, repo.Items, 2)
}

func TestAdminUpdate_AutoArchiveAt(t *testing.T) {
	t.Run("sets a time, including a past one", func(t *testing.T) {
		h, repo := newTestHandler(t)
		repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
		rec := doPost(h.AdminUpdate, `{"id":"a1","autoArchiveAt":1000}`, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		require.NotNil(t, repo.Items["a1"].AutoArchiveAt)
		assert.Equal(t, int64(1000), repo.Items["a1"].AutoArchiveAt.UnixMilli())
		assert.True(t, repo.Items["a1"].IsActive, "update は isActive を変えない")
	})
	t.Run("null clears", func(t *testing.T) {
		h, repo := newTestHandler(t)
		at := time.Now().Add(time.Hour)
		repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &at}
		rec := doPost(h.AdminUpdate, `{"id":"a1","autoArchiveAt":null}`, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Nil(t, repo.Items["a1"].AutoArchiveAt)
	})
	t.Run("omitted keeps", func(t *testing.T) {
		h, repo := newTestHandler(t)
		at := time.Now().Add(time.Hour)
		repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &at}
		rec := doPost(h.AdminUpdate, `{"id":"a1","title":"new"}`, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		require.NotNil(t, repo.Items["a1"].AutoArchiveAt)
		assert.True(t, repo.Items["a1"].AutoArchiveAt.Equal(at))
	})
	t.Run("zero is accepted", func(t *testing.T) {
		h, repo := newTestHandler(t)
		repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
		rec := doPost(h.AdminUpdate, `{"id":"a1","autoArchiveAt":0}`, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		require.NotNil(t, repo.Items["a1"].AutoArchiveAt)
	})
	for name, body := range map[string]string{
		"negative": `{"id":"a1","autoArchiveAt":-1}`,
		"invalid":  `{"id":"a1","autoArchiveAt":8640000000000001}`,
		// 本家はお知らせを引く前に検査するので、無い id でもこちらのエラーになる。
		"before lookup": `{"id":"missing","autoArchiveAt":-1}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
			rec := doPost(h.AdminUpdate, body, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			e := decodeError(t, rec.Body.Bytes())
			assert.Equal(t, "INVALID_AUTO_ARCHIVE_AT", e["code"])
			assert.Equal(t, updateInvalidAutoArchiveAtID, e["id"])
			assert.Nil(t, repo.Items["a1"].AutoArchiveAt)
		})
	}
	t.Run("rejects non-integer as INVALID_PARAM", func(t *testing.T) {
		h, repo := newTestHandler(t)
		repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
		rec := doPost(h.AdminUpdate, `{"id":"a1","autoArchiveAt":1.5}`, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "INVALID_PARAM", decodeError(t, rec.Body.Bytes())["code"])
		assert.Nil(t, repo.Items["a1"].AutoArchiveAt)
	})
}

// admin/announcements/list: isActive は期限を過ぎていれば false、autoArchiveAt は
// ISO 文字列か null。status の絞り込みも期限を見る。
func TestAdminList_AutoArchiveAt(t *testing.T) {
	h, repo := newTestHandler(t)
	now := time.Now()
	future := now.Add(time.Hour).UTC().Truncate(time.Millisecond)
	past := now.Add(-time.Hour).UTC().Truncate(time.Millisecond)
	repo.Items["future"] = &model.Announcement{ID: "future", IsActive: true, AutoArchiveAt: &future}
	repo.Items["expired"] = &model.Announcement{ID: "expired", IsActive: true, AutoArchiveAt: &past}
	repo.Items["plain"] = &model.Announcement{ID: "plain", IsActive: true}

	list := func(status string) map[string]map[string]any {
		rec := doPost(h.AdminList, `{"status":"`+status+`"}`, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		out := map[string]map[string]any{}
		for _, r := range rows {
			out[r["id"].(string)] = r
		}
		return out
	}

	all := list("all")
	require.Len(t, all, 3)
	assert.Equal(t, true, all["future"]["isActive"])
	assert.Equal(t, future.Format("2006-01-02T15:04:05.000Z"), all["future"]["autoArchiveAt"])
	assert.Equal(t, false, all["expired"]["isActive"], "期限を過ぎたものは isActive=false として返す")
	assert.Equal(t, past.Format("2006-01-02T15:04:05.000Z"), all["expired"]["autoArchiveAt"])
	assert.Equal(t, true, all["plain"]["isActive"])
	require.Contains(t, all["plain"], "autoArchiveAt")
	assert.Nil(t, all["plain"]["autoArchiveAt"])

	active := list("active")
	assert.Contains(t, active, "future")
	assert.Contains(t, active, "plain")
	assert.NotContains(t, active, "expired")

	archived := list("archived")
	assert.Equal(t, []string{"expired"}, keys(archived))
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 本家は引数の形 (ajv) を先に検査するので、icon / display の誤りは autoArchiveAt の
// 誤りやお知らせの有無より先に INVALID_PARAM になる (#3809 レビュー)。
func TestAdminUpdate_ParamShapeIsCheckedBeforeAutoArchiveAt(t *testing.T) {
	for _, body := range []string{
		`{"id":"a1","icon":"bad","autoArchiveAt":-1}`,
		`{"id":"a1","display":"bad","autoArchiveAt":-1}`,
		`{"id":"missing","icon":"bad"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, repo := newTestHandler(t)
			repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
			rec := doPost(h.AdminUpdate, body, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "INVALID_PARAM", decodeError(t, rec.Body.Bytes())["code"])
		})
	}
}

type fakeArchiveScheduler struct {
	got []string // "<id> <unix ms>"
	err error
}

func (s *fakeArchiveScheduler) ScheduleAnnouncementArchive(id string, at time.Time) error {
	s.got = append(s.got, fmt.Sprintf("%s %d", id, at.UnixMilli()))
	return s.err
}

// 作成で期限があれば、その時刻のジョブを積む (#3994。本家と同じ)。期限が無い・
// 作成の時点で非アクティブなら積まない。
func TestAdminCreate_SchedulesAutoArchive(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := now.Add(time.Hour)

	h, repo := newTestHandler(t)
	h.SetClock(func() time.Time { return now })
	sched := &fakeArchiveScheduler{}
	h.SetArchiveScheduler(sched)
	rec := doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":%d}`, at.UnixMilli()), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a := onlyItem(t, repo.Items)
	assert.Equal(t, []string{fmt.Sprintf("%s %d", a.ID, at.UnixMilli())}, sched.got)

	rec = doPost(h.AdminCreate, `{"title":"t","text":"x","imageUrl":null}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, sched.got, 1, "no expiry, no job")

	// 積めなくても作成は成功する (起動時の処理と日次の保険が落とす)。
	h, repo = newTestHandler(t)
	h.SetClock(func() time.Time { return now })
	h.SetArchiveScheduler(&fakeArchiveScheduler{err: errors.New("redis down")})
	rec = doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":%d}`, at.UnixMilli()), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, repo.Items, 1)
}

// 作成の途中で期限が来て非アクティブで作ったものには積まない。
func TestAdminCreate_DoesNotScheduleInactive(t *testing.T) {
	h, repo := newTestHandler(t)
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := start.Add(time.Millisecond)
	calls := 0
	h.SetClock(func() time.Time {
		calls++
		if calls%2 == 1 {
			return start
		}
		return at
	})
	sched := &fakeArchiveScheduler{}
	h.SetArchiveScheduler(sched)
	rec := doPost(h.AdminCreate, fmt.Sprintf(`{"title":"t","text":"x","imageUrl":null,"autoArchiveAt":%d}`, at.UnixMilli()), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, onlyItem(t, repo.Items).IsActive)
	assert.Empty(t, sched.got)
}

// 更新では、期限か isActive が変わり、更新後にアクティブで期限があるときだけ積む
// (本家 AnnouncementService.update と同じ条件)。
func TestAdminUpdate_SchedulesAutoArchive(t *testing.T) {
	old := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	newAt := old.Add(time.Hour)
	for _, tc := range []struct {
		name   string
		before model.Announcement
		body   string
		want   []string
	}{
		{"sets a time", model.Announcement{ID: "a1", IsActive: true}, fmt.Sprintf(`{"id":"a1","autoArchiveAt":%d}`, newAt.UnixMilli()), []string{fmt.Sprintf("a1 %d", newAt.UnixMilli())}},
		{"moves the time", model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &old}, fmt.Sprintf(`{"id":"a1","autoArchiveAt":%d}`, newAt.UnixMilli()), []string{fmt.Sprintf("a1 %d", newAt.UnixMilli())}},
		{"reactivates", model.Announcement{ID: "a1", IsActive: false, AutoArchiveAt: &newAt}, `{"id":"a1","isActive":true}`, []string{fmt.Sprintf("a1 %d", newAt.UnixMilli())}},
		{"same time", model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &newAt}, fmt.Sprintf(`{"id":"a1","autoArchiveAt":%d}`, newAt.UnixMilli()), nil},
		{"other field", model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &newAt}, `{"id":"a1","title":"new"}`, nil},
		{"clears", model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &old}, `{"id":"a1","autoArchiveAt":null}`, nil},
		{"deactivates", model.Announcement{ID: "a1", IsActive: true, AutoArchiveAt: &old}, `{"id":"a1","isActive":false}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			a := tc.before
			repo.Items["a1"] = &a
			sched := &fakeArchiveScheduler{}
			h.SetArchiveScheduler(sched)
			rec := doPost(h.AdminUpdate, tc.body, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want, sched.got)
		})
	}
}
