package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// stubReviewer records calls and returns canned results.
type stubReviewer struct {
	rows        []*model.SignupApplication
	count       int
	listErr     error
	countErr    error
	expireErr   error
	actErr      error
	expireCalls int
	lastFilter  string
	lastLimit   int
	lastOffset  int
	lastID      string
	lastActor   string
}

func (s *stubReviewer) List(filter string, limit, offset int) ([]*model.SignupApplication, error) {
	s.lastFilter, s.lastLimit, s.lastOffset = filter, limit, offset
	return s.rows, s.listErr
}

func (s *stubReviewer) Count(string) (int, error) { return s.count, s.countErr }

func (s *stubReviewer) Approve(id, moderatorID string) error {
	s.lastID, s.lastActor = id, moderatorID
	return s.actErr
}

func (s *stubReviewer) Reject(id, moderatorID string) error {
	s.lastID, s.lastActor = id, moderatorID
	return s.actErr
}

func (s *stubReviewer) ExpireStale() (int, error) {
	s.expireCalls++
	return 0, s.expireErr
}

func newReviewerHandler(t *testing.T, rev apiadmin.SignupApplicationReviewer) *apiadmin.Handler {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	if rev != nil {
		h.SetSignupApplicationReviewer(rev)
	}
	return h
}

var testModerator = &model.User{ID: "mod-1", Username: "mod"}

func sampleApplication() *model.SignupApplication {
	now := time.Now().UTC()
	return &model.SignupApplication{
		ID:            "app-1",
		ClaimCodeHash: "hash-1",
		Status:        model.SignupApplicationPending,
		Answers:       datatypes.JSON(`[{"label":"参加の動機","value":"よろしくお願いします"}]`),
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(7 * 24 * time.Hour),
	}
}

func TestSignupApplicationList(t *testing.T) {
	rev := &stubReviewer{rows: []*model.SignupApplication{sampleApplication()}, count: 1}
	h := newReviewerHandler(t, rev)

	rec := doPost(h.SignupApplicationList, `{"filter":"pending","limit":10,"offset":5}`, testModerator)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Applications []map[string]any `json:"applications"`
		Count        int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Applications, 1)
	assert.Equal(t, 1, body.Count)

	app := body.Applications[0]
	assert.Equal(t, "app-1", app["id"])
	assert.Equal(t, "pending", app["status"])
	// 回答はラベル付きで審査画面へ渡る (#2570)。
	answers, err := json.Marshal(app["answers"])
	require.NoError(t, err)
	assert.Contains(t, string(answers), "参加の動機")
	assert.Contains(t, string(answers), "よろしくお願いします")
	// **クレームコードは出さない。** hash しか持っていないうえ、出せてしまうと
	// 管理者が申請者になりすまして登録できる。
	assert.NotContains(t, app, "claimCode")
	assert.NotContains(t, app, "claimCodeHash")

	assert.Equal(t, "pending", rev.lastFilter)
	assert.Equal(t, 10, rev.lastLimit)
	assert.Equal(t, 5, rev.lastOffset)
}

// **一覧の前に掃除する。** 期限切れの反映は申請者の参照時に行われるので、誰も
// 見に来ていない申請は pending のまま残る。掃除せずに出すと、管理者に
// 「審査待ちに見えるが承認できない」行が並ぶ。
func TestSignupApplicationList_SweepsExpiredFirst(t *testing.T) {
	rev := &stubReviewer{}
	h := newReviewerHandler(t, rev)

	rec := doPost(h.SignupApplicationList, `{}`, testModerator)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, rev.expireCalls)
}

// 掃除に失敗しても一覧は出す (表示が実態より古くなるだけ)。
func TestSignupApplicationList_SweepFailureStillLists(t *testing.T) {
	rev := &stubReviewer{expireErr: errors.New("boom"), rows: []*model.SignupApplication{sampleApplication()}, count: 1}
	h := newReviewerHandler(t, rev)

	rec := doPost(h.SignupApplicationList, `{}`, testModerator)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestSignupApplicationList_Defaults(t *testing.T) {
	rev := &stubReviewer{}
	h := newReviewerHandler(t, rev)

	// body 無しでも既定値で応答する (管理画面が引数なしで叩く)。
	rec := doPost(h.SignupApplicationList, ``, testModerator)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "all", rev.lastFilter)
	assert.Equal(t, 30, rev.lastLimit)
	assert.Equal(t, 0, rev.lastOffset)
}

func TestSignupApplicationList_ClampsLimitAndOffset(t *testing.T) {
	rev := &stubReviewer{}
	h := newReviewerHandler(t, rev)

	rec := doPost(h.SignupApplicationList, `{"limit":9999,"offset":-5}`, testModerator)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 100, rev.lastLimit)
	assert.Equal(t, 0, rev.lastOffset)
}

func TestSignupApplicationList_Errors(t *testing.T) {
	t.Run("list failure", func(t *testing.T) {
		h := newReviewerHandler(t, &stubReviewer{listErr: errors.New("boom")})
		rec := doPost(h.SignupApplicationList, `{}`, testModerator)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("count failure", func(t *testing.T) {
		h := newReviewerHandler(t, &stubReviewer{countErr: errors.New("boom")})
		rec := doPost(h.SignupApplicationList, `{}`, testModerator)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// 未配線の構成では 503。承認制を有効にしていないインスタンスで管理画面を
// 開いても、500 ではなく「その機能は無い」と分かる形にする。
func TestSignupApplication_NotWired(t *testing.T) {
	h := newReviewerHandler(t, nil)

	rec := doPost(h.SignupApplicationList, `{}`, testModerator)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	rec = doPost(h.SignupApplicationApprove, `{"applicationId":"app-1"}`, testModerator)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	rec = doPost(h.SignupApplicationReject, `{"applicationId":"app-1"}`, testModerator)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestSignupApplicationApproveReject(t *testing.T) {
	t.Run("approve records the moderator", func(t *testing.T) {
		rev := &stubReviewer{}
		h := newReviewerHandler(t, rev)
		rec := doPost(h.SignupApplicationApprove, `{"applicationId":"app-1"}`, testModerator)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "app-1", rev.lastID)
		assert.Equal(t, "mod-1", rev.lastActor)
	})

	t.Run("reject records the moderator", func(t *testing.T) {
		rev := &stubReviewer{}
		h := newReviewerHandler(t, rev)
		rec := doPost(h.SignupApplicationReject, `{"applicationId":"app-1"}`, testModerator)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "app-1", rev.lastID)
		assert.Equal(t, "mod-1", rev.lastActor)
	})
}

func TestSignupApplicationApprove_MissingParam(t *testing.T) {
	h := newReviewerHandler(t, &stubReviewer{})
	rec := doPost(h.SignupApplicationApprove, `{}`, testModerator)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSignupApplicationApprove_MissingActor(t *testing.T) {
	h := newReviewerHandler(t, &stubReviewer{})
	// RequireModerator が保証するので、ここに来るなら配線ミス。
	rec := doPost(h.SignupApplicationApprove, `{"applicationId":"app-1"}`, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// 期限切れは「審査待ちではない」と一緒にしない。行は pending のまま残りうる
// ので、まとめると管理者に伝わらない。
func TestSignupApplicationApprove_ErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{name: "not found", err: signupapplication.ErrNotFound, wantCode: http.StatusNotFound, wantBody: "NO_SUCH_APPLICATION"},
		{name: "expired", err: signupapplication.ErrExpired, wantCode: http.StatusBadRequest, wantBody: "APPLICATION_EXPIRED"},
		{name: "not pending", err: signupapplication.ErrNotPending, wantCode: http.StatusBadRequest, wantBody: "APPLICATION_NOT_PENDING"},
		{name: "unexpected", err: errors.New("boom"), wantCode: http.StatusInternalServerError, wantBody: "INTERNAL_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newReviewerHandler(t, &stubReviewer{actErr: tt.err})
			rec := doPost(h.SignupApplicationApprove, `{"applicationId":"app-1"}`, testModerator)
			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.wantBody)
		})
	}
}
