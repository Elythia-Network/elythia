package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// stubAuditRepo records what the handler wrote and hands back canned rows.
type stubAuditRepo struct {
	written  []*model.IPLookupLog
	writeErr error

	gotLimit  int
	gotOffset int
	rows      []*model.IPLookupLog
	listErr   error
}

func (s *stubAuditRepo) Create(l *model.IPLookupLog) error {
	s.written = append(s.written, l)
	return s.writeErr
}

func (s *stubAuditRepo) List(limit, offset int) ([]*model.IPLookupLog, error) {
	s.gotLimit, s.gotOffset = limit, offset
	return s.rows, s.listErr
}

func (s *stubAuditRepo) DeleteOlderThan(time.Time) (int64, error) { return 0, nil }

type auditEntryJSON struct {
	ID   string `json:"id"`
	User *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	UserID     string `json:"userId"`
	Kind       string `json:"kind"`
	IP         string `json:"ip"`
	TargetUser *struct {
		ID string `json:"id"`
	} `json:"targetUser"`
	TargetUserID string `json:"targetUserId"`
	SinceDays    int    `json:"sinceDays"`
	ResultCount  int    `json:"resultCount"`
	CreatedAt    string `json:"createdAt"`
}

type auditJSON struct {
	RetentionDays int              `json:"retentionDays"`
	Limit         int              `json:"limit"`
	Offset        int              `json:"offset"`
	HasMore       bool             `json:"hasMore"`
	Entries       []auditEntryJSON `json:"entries"`
}

func decodeAudit(t *testing.T, body []byte) auditJSON {
	t.Helper()
	var out auditJSON
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func newAuditHandler(t *testing.T, repo *stubAuditRepo) (*apiadmin.Handler, *testutil.MockUserRepository) {
	t.Helper()
	h, users, _, _ := newTestHandler(t)
	if repo != nil {
		h.SetIPLookupLogRepo(repo)
	}
	return h, users
}

func logRow(rowID, userID, kind, ip, target string, at time.Time) *model.IPLookupLog {
	return &model.IPLookupLog{
		ID: rowID, UserID: userID, Kind: kind, IP: ip, TargetUserID: target,
		SinceDays: 90, ResultCount: 2, CreatedAt: at,
	}
}

// --- 監査の記録 (#3106) ---

func auditedHandler(t *testing.T, stub *stubIPSearch) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository, *stubAuditRepo) {
	t.Helper()
	h, users, meta := newIPSearchHandler(t, stub)
	audit := &stubAuditRepo{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIPLookupAudit(iplookuplog.NewService(audit, gen))
	return h, users, meta, audit
}

// **IP 起点の照会が監査に残る。** 残すのは「誰が・いつ・何を・どの期間で引いて・
// 何件返したか」だけで、結果そのものは残さない。
func TestIPAccounts_RecordsAudit(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		{UserID: "u1", FirstSeen: now, LastSeen: now, ObservationCount: 1},
	}}
	h, users, _, audit := auditedHandler(t, stub)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPAccounts, `{"ip":"::ffff:192.0.2.1","sinceDays":7}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, audit.written, 1, "照会が監査に残っていない")
	got := audit.written[0]
	assert.Equal(t, adminUser.ID, got.UserID)
	assert.Equal(t, model.IPLookupKindIP, got.Kind)
	// **正規形で残す。** 入力のエコーだと、同じアドレスの照会が別物として並ぶ。
	assert.Equal(t, "192.0.2.1", got.IP)
	assert.Equal(t, "", got.TargetUserID)
	assert.Equal(t, 7, got.SinceDays)
	assert.Equal(t, 1, got.ResultCount)
	// 結果そのものは残さない。
	assert.NotContains(t, got.IP+got.TargetUserID, "alice")
}

// **`resultCount` は「返した件数」で、「引いた行数」ではない** (#3106)。
//
// 利用者の行を解決できなかった観測は候補にならないので、返していないものを
// 数えると監査が実際の開示より多く見える。**落ちが 0 件の fixture では
// どちらでも通る**ので、落ちのある形で固定する (敵対的レビュー 1 周目で、
// `len(rows)` への変異が緑のまま通ることを実測された)。
func TestIPAccounts_AuditCountsWhatWasReturned(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		{UserID: "u1", FirstSeen: now, LastSeen: now, ObservationCount: 1},
		// 利用者の行を引けない観測 (完全削除済みのアカウント)。
		{UserID: "gone", FirstSeen: now, LastSeen: now, ObservationCount: 1},
	}}
	h, users, _, audit := auditedHandler(t, stub)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, audit.written, 1)
	assert.Equal(t, 1, audit.written[0].ResultCount,
		"引いた 2 行ではなく、実際に返した 1 件を残すこと")
}

// **upstream の口 (`admin/get-user-ips`) も残る** (#3106)。
//
// ここを記録しないと、**監査を迂回して IP を引ける経路**が 1 本残る — 返すのは
// mk-go 独自の口と同じ「利用者 ↔ IP の対応」で、管理者なら誰でも叩ける。
// 窓を取らない口なので `sinceDays` は 0。
func TestGetUserIPs_RecordsAudit(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetUserIPRepo(&stubIPRepo{})
	audit := &stubAuditRepo{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIPLookupAudit(iplookuplog.NewService(audit, gen))

	rec := doPost(h.GetUserIPs, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	// 共有キャッシュに残さないのも同じ扱い。
	assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")

	require.Len(t, audit.written, 1, "upstream の照会が監査に残っていない")
	got := audit.written[0]
	assert.Equal(t, adminUser.ID, got.UserID)
	assert.Equal(t, model.IPLookupKindUserIPs, got.Kind)
	assert.Equal(t, "u1", got.TargetUserID)
	assert.Equal(t, "", got.IP, "利用者起点なので IP は空")
	assert.Equal(t, 0, got.SinceDays, "窓を取らない口なので 0")
}

// 引けなかった照会は監査に残さない。**`resultCount: 0` を書くと、本当に 0 件
// だった照会と見分けが付かなくなる** (この口は DB 障害でも 200 + 空を返す)。
func TestGetUserIPs_FailedLookupIsNotRecorded(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetUserIPRepo(&failingIPRepo{})
	audit := &stubAuditRepo{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIPLookupAudit(iplookuplog.NewService(audit, gen))

	rec := doPost(h.GetUserIPs, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, audit.written, "引けていないのに監査に残している")
	assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
}

// **NUL 入りの id は引く前に落とす** (#3025)。そのまま渡すと PostgreSQL が
// クエリごと落とし、しかも握り潰しで 200 になる一方、**監査の INSERT だけが
// 22021 で失敗する**。
func TestGetUserIPs_NulIDIsRejectedBeforeQuery(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := &failingIPRepo{}
	h.SetUserIPRepo(repo)
	audit := &stubAuditRepo{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIPLookupAudit(iplookuplog.NewService(audit, gen))

	rec := doPost(h.GetUserIPs, `{"userId":"u\u00001"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, repo.called, "NUL を含む id で DB を引いている")
	assert.Empty(t, audit.written)
}

// **弾かれた照会は残さない。** 引いていない以上、開示は起きていない。
func TestGetUserIPs_RejectedLookupIsNotRecorded(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetUserIPRepo(&stubIPRepo{})
	audit := &stubAuditRepo{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIPLookupAudit(iplookuplog.NewService(audit, gen))

	require.Equal(t, http.StatusBadRequest, doPost(h.GetUserIPs, `{}`, adminUser).Code)
	assert.Empty(t, audit.written)
}

// 利用者起点の照会も残る。対象は userId で、IP は空。
func TestIPRelated_RecordsAudit(t *testing.T) {
	now := time.Now()
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
		sharedRows:   []repository.UserIPSharedRow{sharedRow("u1", "192.0.2.1", now)},
	}
	h, users, _ := newRelatedHandler(t, stub)
	audit := &stubAuditRepo{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIPLookupAudit(iplookuplog.NewService(audit, gen))
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target","sinceDays":30}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, audit.written, 1, "照会が監査に残っていない")
	got := audit.written[0]
	assert.Equal(t, model.IPLookupKindRelated, got.Kind)
	assert.Equal(t, "", got.IP)
	assert.Equal(t, "target", got.TargetUserID)
	assert.Equal(t, 30, got.SinceDays)
	assert.Equal(t, 1, got.ResultCount)
}

// **弾いた照会は記録しない。** 引いていないので監査に残す事実が無く、
// 残すと「照会した」件数が実態と合わなくなる。
func TestIPAccounts_DoesNotRecordRejectedLookups(t *testing.T) {
	h, _, _, audit := auditedHandler(t, &stubIPSearch{hasAny: true})

	rec := doPost(h.IPAccounts, `{"ip":"not-an-ip"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, audit.written)
}

// **監査に書けなくても照会は成立する。** 監査の障害が調査そのものを止めない。
func TestIPAccounts_AuditFailureDoesNotBreakLookup(t *testing.T) {
	h, _, _, audit := auditedHandler(t, &stubIPSearch{hasAny: true})
	audit.writeErr = assertError{}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code, "監査の失敗で照会まで落ちている")
}

// **結果を共有キャッシュに残さない** (#3066 §8)。
func TestIPLookups_AreNotCacheable(t *testing.T) {
	now := time.Now()
	t.Run("admin/ip/accounts", func(t *testing.T) {
		h, _, _, _ := auditedHandler(t, &stubIPSearch{hasAny: true})
		rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	})
	t.Run("admin/ip/related-accounts", func(t *testing.T) {
		h, _, _ := newRelatedHandler(t, &stubIPRelated{
			stubIPSearch: stubIPSearch{hasAny: true},
			ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
		})
		rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	})
	t.Run("admin/ip/lookup-log", func(t *testing.T) {
		h, _ := newAuditHandler(t, &stubAuditRepo{})
		rec := doPost(h.IPLookupLog, `{}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	})
}

// --- 監査の読み出し ---

func TestIPLookupLog_ReturnsEntries(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	at := time.Date(2026, 9, 19, 21, 0, 0, 0, jst)
	repo := &stubAuditRepo{rows: []*model.IPLookupLog{
		logRow("a1", "mod1", model.IPLookupKindIP, "192.0.2.1", "", at),
		logRow("a2", "mod1", model.IPLookupKindRelated, "", "target1", at.Add(-time.Hour)),
	}}
	h, users := newAuditHandler(t, repo)
	users.Users["mod1"] = &model.User{ID: "mod1", Username: "mod"}
	users.Users["target1"] = &model.User{ID: "target1", Username: "suspect"}

	rec := doPost(h.IPLookupLog, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeAudit(t, rec.Body.Bytes())
	assert.Equal(t, int(iplookuplog.Retention/(24*time.Hour)), got.RetentionDays)
	require.Len(t, got.Entries, 2)
	assert.Equal(t, "a1", got.Entries[0].ID)
	require.NotNil(t, got.Entries[0].User)
	assert.Equal(t, "mod", got.Entries[0].User.Username)
	assert.Equal(t, "192.0.2.1", got.Entries[0].IP)
	assert.Equal(t, "2026-09-19T12:00:00.000Z", got.Entries[0].CreatedAt)
	require.NotNil(t, got.Entries[1].TargetUser)
	assert.Equal(t, "target1", got.Entries[1].TargetUser.ID)
}

// **引けない利用者は null で返し、行そのものは残す。** `ip_lookup_log.userId` に
// FK は無く、退会しても記録は残る (監査の目的からしてそれが正しい)。
func TestIPLookupLog_KeepsEntriesWithUnresolvableUsers(t *testing.T) {
	repo := &stubAuditRepo{rows: []*model.IPLookupLog{
		logRow("a1", "gone", model.IPLookupKindRelated, "", "alsoGone", time.Now()),
	}}
	h, _ := newAuditHandler(t, repo)

	rec := doPost(h.IPLookupLog, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeAudit(t, rec.Body.Bytes())
	require.Len(t, got.Entries, 1, "引けない利用者の記録が落ちている")
	assert.Nil(t, got.Entries[0].User)
	assert.Equal(t, "gone", got.Entries[0].UserID, "誰かを追う手掛かりまで消えている")
	assert.Nil(t, got.Entries[0].TargetUser)
	assert.Equal(t, "alsoGone", got.Entries[0].TargetUserID)
}

// +1 件引いて hasMore を判断する。+1 件目は返さない。
func TestIPLookupLog_Paging(t *testing.T) {
	now := time.Now()
	rows := make([]*model.IPLookupLog, 0, 3)
	for i := 0; i < 3; i++ {
		rows = append(rows, logRow("a"+strconv.Itoa(i), "mod1", model.IPLookupKindIP, "192.0.2.1", "", now))
	}
	repo := &stubAuditRepo{rows: rows}
	h, users := newAuditHandler(t, repo)
	users.Users["mod1"] = &model.User{ID: "mod1", Username: "mod"}

	rec := doPost(h.IPLookupLog, `{"limit":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeAudit(t, rec.Body.Bytes())
	assert.Equal(t, 3, repo.gotLimit, "hasMore 判定のための +1 件が渡っていない")
	assert.Equal(t, 2, got.Limit)
	assert.True(t, got.HasMore)
	assert.Len(t, got.Entries, 2)
	assert.NotNil(t, got.Entries)
}

func TestIPLookupLog_RejectsBadParams(t *testing.T) {
	for _, body := range []string{
		`{"limit":0}`, `{"limit":101}`, `{"offset":-1}`, `{"offset":10001}`,
		`{"limit":"2"}`, `not json`,
	} {
		t.Run(body, func(t *testing.T) {
			repo := &stubAuditRepo{}
			h, _ := newAuditHandler(t, repo)
			rec := doPost(h.IPLookupLog, body, adminUser)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Zero(t, repo.gotLimit, "不正なパラメータのまま DB を引いている")
		})
	}
}

// 障害は 500。**空の一覧にしない** — 「誰も照会していない」という誤った事実になる。
func TestIPLookupLog_FailuresAreInternal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) *apiadmin.Handler
		logWant string
	}{
		{"未配線", func(t *testing.T) *apiadmin.Handler {
			h, _ := newAuditHandler(t, nil)
			return h
		}, "dependencies are not wired"},
		{"一覧が失敗", func(t *testing.T) *apiadmin.Handler {
			h, _ := newAuditHandler(t, &stubAuditRepo{listErr: assertError{}})
			return h
		}, "list failed"},
		{"利用者の解決が失敗", func(t *testing.T) *apiadmin.Handler {
			h, users := newAuditHandler(t, &stubAuditRepo{rows: []*model.IPLookupLog{
				logRow("a1", "mod1", model.IPLookupKindIP, "192.0.2.1", "", time.Now()),
			}})
			users.FindManyByIDsErr = assertError{}
			return h
		}, "user lookup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureSlog(t)
			h := tc.prepare(t)

			rec := doPost(h.IPLookupLog, `{}`, adminUser)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.NotContains(t, rec.Body.String(), `"entries"`)
			assert.Contains(t, logged.String(), "admin/ip/lookup-log")
			assert.Contains(t, logged.String(), tc.logWant)
		})
	}
}

// failingIPRepo は ListByUser が必ず失敗する UserIPRepository。
type failingIPRepo struct{ called bool }

func (r *failingIPRepo) Observe(_, _ string, _ time.Time) error { return nil }
func (r *failingIPRepo) ListByUser(_ string, _ int) ([]*model.UserIP, error) {
	r.called = true
	return nil, errors.New("boom")
}
func (r *failingIPRepo) DeleteLastSeenBefore(_ time.Time) (int64, error) { return 0, nil }
