package admin_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/iprelation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

const relDay = 24 * time.Hour

// stubIPRelated serves both halves of the related-accounts lookup.
type stubIPRelated struct {
	stubIPSearch

	gotIPsUser  string
	gotIPsSince time.Time
	gotIPsLimit int
	ipRows      []repository.UserIPWindowRow
	ipErr       error

	gotSharedIPs   []string
	gotSharedPerIP int
	sharedCalls    int
	sharedRows     []repository.UserIPSharedRow
	sharedErr      error
}

func (s *stubIPRelated) ListIPsByUser(userID string, since time.Time, limit int) ([]repository.UserIPWindowRow, error) {
	s.gotIPsUser, s.gotIPsSince, s.gotIPsLimit = userID, since, limit
	return s.ipRows, s.ipErr
}

func (s *stubIPRelated) ListSharedIPAccounts(ips []string, since time.Time, perIP int) ([]repository.UserIPSharedRow, error) {
	s.sharedCalls++
	s.gotSharedIPs, s.gotSharedPerIP = ips, perIP
	return s.sharedRows, s.sharedErr
}

type relatedSharedJSON struct {
	IP                         string  `json:"ip"`
	TargetLastSeenAt           string  `json:"targetLastSeenAt"`
	CandidateLastSeenAt        string  `json:"candidateLastSeenAt"`
	IPAccountCount             int     `json:"ipAccountCount"`
	IPAccountCountIsLowerBound bool    `json:"ipAccountCountIsLowerBound"`
	ElapsedDays                float64 `json:"elapsedDays"`
}

type relatedCandidateJSON struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	IsSuspended    bool                `json:"isSuspended"`
	IsDeleted      bool                `json:"isDeleted"`
	LastActiveDate *string             `json:"lastActiveDate"`
	SharedIPCount  int                 `json:"sharedIpCount"`
	Score          float64             `json:"score"`
	SharedIPs      []relatedSharedJSON `json:"sharedIps"`
}

type relatedJSON struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	LoggingEnabled      bool                   `json:"loggingEnabled"`
	HasAnyHistory       bool                   `json:"hasAnyHistory"`
	SinceDays           int                    `json:"sinceDays"`
	RetentionDays       int                    `json:"retentionDays"`
	HalfLifeDays        int                    `json:"halfLifeDays"`
	TargetIPCount       int                    `json:"targetIpCount"`
	Truncated           bool                   `json:"truncated"`
	TargetIPsTruncated  bool                   `json:"targetIpsTruncated"`
	CandidatesTruncated bool                   `json:"candidatesTruncated"`
	Limit               int                    `json:"limit"`
	Offset              int                    `json:"offset"`
	HasMore             bool                   `json:"hasMore"`
	DroppedCount        int                    `json:"droppedCount"`
	TotalCount          int                    `json:"totalCount"`
	Candidates          []relatedCandidateJSON `json:"candidates"`
}

func decodeRelated(t *testing.T, body []byte) relatedJSON {
	t.Helper()
	var out relatedJSON
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func newRelatedHandler(t *testing.T, stub *stubIPRelated) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository) {
	t.Helper()
	h, users, meta, _ := newTestHandler(t)
	if stub != nil {
		h.SetIPSearchRepo(stub)
	}
	users.Users["target"] = &model.User{ID: "target", Username: "target"}
	return h, users, meta
}

func winRow(ip string, last time.Time) repository.UserIPWindowRow {
	return repository.UserIPWindowRow{IP: ip, LastSeen: last}
}

func sharedRow(userID, ip string, last time.Time) repository.UserIPSharedRow {
	return repository.UserIPSharedRow{UserID: userID, IP: ip, LastSeen: last}
}

func TestIPRelated_ReturnsCandidatesWithEvidence(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	targetLast := time.Date(2026, 9, 18, 21, 0, 0, 0, jst)
	candLast := time.Date(2026, 9, 17, 12, 0, 0, 0, jst)
	active := time.Date(2026, 9, 19, 9, 30, 0, 0, jst)
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", targetLast)},
		// その IP を使ったのは 3 アカウント (対象 + u1 + u2) なので候補は 2 件。
		// 行数と `ipAccountCount` が食い違うと打ち切り扱いになる。
		// **対象本人の行も返る。** 「その IP を使ったアカウント数」は本人を含む数
		// なので SQL では落とさない。候補から外すのは handler。
		sharedRows: []repository.UserIPSharedRow{
			sharedRow("target", "192.0.2.1", targetLast),
			sharedRow("u1", "192.0.2.1", candLast),
			sharedRow("u2", "192.0.2.1", candLast.Add(-relDay)),
		},
	}
	h, users, meta := newRelatedHandler(t, stub)
	meta.Meta.EnableIPLogging = true
	users.Users["u1"] = &model.User{
		ID: "u1", Username: "alice", IsSuspended: true, LastActiveDate: &active,
	}
	users.Users["u2"] = &model.User{ID: "u2", Username: "bob"}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeRelated(t, rec.Body.Bytes())
	assert.Equal(t, "target", got.User.ID)
	assert.True(t, got.LoggingEnabled)
	assert.True(t, got.HasAnyHistory)
	assert.Equal(t, 1, got.TargetIPCount)
	assert.False(t, got.Truncated)
	// **半減期を画面へ渡す。** 順位の根拠なので、画面で決め打ちするとサーバーが
	// 変えたときに黙って嘘になる。
	assert.Equal(t, int(iprelation.DefaultHalfLife/relDay), got.HalfLifeDays)

	require.Len(t, got.Candidates, 2)
	cand := got.Candidates[0]
	assert.Equal(t, "u1", cand.User.ID)
	assert.Equal(t, "alice", cand.User.Username)
	assert.True(t, cand.IsSuspended)
	assert.False(t, cand.IsDeleted)
	require.NotNil(t, cand.LastActiveDate)
	assert.Equal(t, "2026-09-19T00:30:00.000Z", *cand.LastActiveDate)
	assert.Equal(t, 1, cand.SharedIPCount)

	require.Len(t, cand.SharedIPs, 1)
	p := cand.SharedIPs[0]
	assert.Equal(t, "192.0.2.1", p.IP)
	// **双方の最終観測をそれぞれ出す。** 片方だけだと「同時に使っていた」と
	// 読めてしまう (#3105)。
	assert.Equal(t, "2026-09-18T12:00:00.000Z", p.TargetLastSeenAt)
	assert.Equal(t, "2026-09-17T03:00:00.000Z", p.CandidateLastSeenAt)
	// **共有回線かを判断する材料。** 見えた行数 (対象本人を含む) から数える。
	assert.Equal(t, 3, p.IPAccountCount)
	assert.False(t, p.IPAccountCountIsLowerBound)
	// 重みではなく経過日数を返す ([0,1] の値はパーセントと見分けが付かない)。
	assert.GreaterOrEqual(t, p.ElapsedDays, 0.0)
}

// 対象ユーザーの IP を起点に、本人を除いて引く。
func TestIPRelated_UsesTargetIPsAndExcludesSelf(t *testing.T) {
	now := time.Now()
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows: []repository.UserIPWindowRow{
			winRow("192.0.2.1", now), winRow("192.0.2.2", now.Add(-relDay)),
		},
	}
	h, _, _ := newRelatedHandler(t, stub)

	before := time.Now()
	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, "target", stub.gotIPsUser)
	assert.WithinDuration(t, before.AddDate(0, 0, -90), stub.gotIPsSince, time.Minute)
	assert.Equal(t, []string{"192.0.2.1", "192.0.2.2"}, stub.gotSharedIPs)
	// **+1 件引く。** 上限ちょうどで返ったときに「まだ他にも居る」と判断できない。
	assert.Equal(t, 201, stub.gotSharedPerIP, "1 IP あたりの上限 + 1 が渡っていない")
	// 起点も +1 件引く。上限ちょうどだと「切った」と判断できない。
	assert.Equal(t, 51, stub.gotIPsLimit, "起点の上限 + 1 が渡っていない")
}

// **起点の IP が無ければ引かない。** 空の集合で引いても候補は出ないので、
// 無駄な往復を増やさない。
func TestIPRelated_SkipsSharedLookupWithoutIPs(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, _, _ := newRelatedHandler(t, stub)

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, stub.sharedCalls)
	got := decodeRelated(t, rec.Body.Bytes())
	assert.Empty(t, got.Candidates)
	assert.NotNil(t, got.Candidates, "候補が無くても null ではなく空配列")
	assert.Equal(t, 0, got.TargetIPCount)
}

// 複数 IP で一致した候補は 1 アカウントに統合され、スコアの高い順に並ぶ。
func TestIPRelated_MergesAndRanks(t *testing.T) {
	// 実時刻から離した固定の時刻にする。handler が固定した時刻を使わずに実時刻を
	// 取ると、減衰でスコアが変わって落ちる (近い時刻だと気付けない)。
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows: []repository.UserIPWindowRow{
			winRow("192.0.2.1", now), winRow("192.0.2.2", now),
		},
		sharedRows: []repository.UserIPSharedRow{
			sharedRow("u_one", "192.0.2.1", now.Add(-60*relDay)),
			sharedRow("u_two", "192.0.2.1", now),
			sharedRow("u_two", "192.0.2.2", now),
		},
	}
	h, users, _ := newRelatedHandler(t, stub)
	// スコアは経過時間で減衰するので、handler の時刻を行の時刻に固定する (固定
	// しないと、-race の遅い CI で数ミリ秒の減衰が許容誤差を超える)。
	h.SetClockForTest(func() time.Time { return now })
	for _, u := range []string{"u_one", "u_two"} {
		users.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	require.Len(t, got.Candidates, 2)
	assert.Equal(t, "u_two", got.Candidates[0].User.ID)
	assert.Equal(t, 2, got.Candidates[0].SharedIPCount)
	assert.InDelta(t, 2.0, got.Candidates[0].Score, 1e-9)
	assert.Equal(t, "u_one", got.Candidates[1].User.ID)
	assert.InDelta(t, 0.25, got.Candidates[1].Score, 1e-9)
}

// **スコアは確率ではない。** 上限が 1 ではないので、パーセントとして読めない。
func TestIPRelated_ScoreIsNotAProbability(t *testing.T) {
	// 実時刻から離した固定の時刻にする。handler が固定した時刻を使わずに実時刻を
	// 取ると、減衰でスコアが変わって落ちる (近い時刻だと気付けない)。
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows: []repository.UserIPWindowRow{
			winRow("192.0.2.1", now), winRow("192.0.2.2", now), winRow("192.0.2.3", now),
		},
		sharedRows: []repository.UserIPSharedRow{
			sharedRow("u1", "192.0.2.1", now),
			sharedRow("u1", "192.0.2.2", now),
			sharedRow("u1", "192.0.2.3", now),
		},
	}
	h, users, _ := newRelatedHandler(t, stub)
	// スコアは経過時間で減衰するので、handler の時刻を行の時刻に固定する (固定
	// しないと、-race の遅い CI で数ミリ秒の減衰が許容誤差を超える)。
	h.SetClockForTest(func() time.Time { return now })
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	require.Len(t, got.Candidates, 1)
	assert.InDelta(t, 3.0, got.Candidates[0].Score, 1e-9,
		"スコアが [0,1] に正規化されている。パーセントとして読めてしまう")
}

// 打ち切りは伝える。黙って切ると「これで全部」と読まれる。
func TestIPRelated_ReportsTruncation(t *testing.T) {
	now := time.Now()
	t.Run("起点の IP が多すぎる", func(t *testing.T) {
		rows := make([]repository.UserIPWindowRow, 0, 200)
		for i := 0; i < 200; i++ {
			rows = append(rows, winRow("192.0.2."+string(rune('a'+i%26)), now))
		}
		stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}, ipRows: rows}
		h, _, _ := newRelatedHandler(t, stub)

		rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		got := decodeRelated(t, rec.Body.Bytes())
		assert.True(t, got.Truncated, "起点の打ち切りが伝わっていない")
		// **原因を分ける。** 起点を切ったのか候補を切ったのかで運用者が取れる手が違う。
		assert.True(t, got.TargetIPsTruncated, "起点を切ったことが候補側と区別できない")
		assert.False(t, got.CandidatesTruncated)
		assert.Less(t, got.TargetIPCount, 200, "上限で切れていない")
	})

	t.Run("1 IP のアカウントが多すぎる", func(t *testing.T) {
		shared := make([]repository.UserIPSharedRow, 0, 400)
		for i := 0; i < 400; i++ {
			// 上限 (200) + 1 件を超える行が 1 つの IP から返る = まだ他にも居る。
			shared = append(shared, sharedRow("u"+string(rune('a'+i%26))+string(rune('a'+i/26)), "192.0.2.1", now))
		}
		stub := &stubIPRelated{
			stubIPSearch: stubIPSearch{hasAny: true},
			ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
			sharedRows:   shared,
		}
		h, _, _ := newRelatedHandler(t, stub)

		rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		got := decodeRelated(t, rec.Body.Bytes())
		assert.True(t, got.Truncated, "1 IP あたりの打ち切りが伝わっていない")
		assert.False(t, got.TargetIPsTruncated, "候補側の打ち切りを起点の打ち切りとして伝えている")
		assert.True(t, got.CandidatesTruncated)
	})

	// **対象本人の行も候補から外す。** SQL では落とさない (落とすと数えられない)。
	t.Run("対象本人は候補にしない", func(t *testing.T) {
		stub := &stubIPRelated{
			stubIPSearch: stubIPSearch{hasAny: true},
			ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
			sharedRows: []repository.UserIPSharedRow{
				sharedRow("target", "192.0.2.1", now),
				sharedRow("u1", "192.0.2.1", now),
			},
		}
		h, users, _ := newRelatedHandler(t, stub)
		users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

		rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		got := decodeRelated(t, rec.Body.Bytes())
		require.Len(t, got.Candidates, 1, "対象本人が候補に混ざっている")
		assert.Equal(t, "u1", got.Candidates[0].User.ID)
		assert.Zero(t, got.DroppedCount, "本人を落としたぶんを「削除済み」として数えている")
		// 本人も 1 アカウントとして数える。
		assert.Equal(t, 2, got.Candidates[0].SharedIPs[0].IPAccountCount)
		assert.False(t, got.Truncated)
	})

	// 全部返ってきていれば立てない (対象本人のぶん 1 件を差し引く)。
	t.Run("上限に達していなければ立てない", func(t *testing.T) {
		stub := &stubIPRelated{
			stubIPSearch: stubIPSearch{hasAny: true},
			ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
			sharedRows:   []repository.UserIPSharedRow{sharedRow("u1", "192.0.2.1", now)},
		}
		h, users, _ := newRelatedHandler(t, stub)
		users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

		rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		got := decodeRelated(t, rec.Body.Bytes())
		assert.False(t, got.Truncated)
		assert.False(t, got.TargetIPsTruncated)
	})
}

// ページングは順位を保ったまま切る。
func TestIPRelated_Paging(t *testing.T) {
	now := time.Now()
	ipRows := make([]repository.UserIPWindowRow, 0, 4)
	shared := make([]repository.UserIPSharedRow, 0, 4)
	names := []string{"u1", "u2", "u3", "u4"}
	for i, n := range names {
		ip := "192.0.2." + string(rune('1'+i))
		ipRows = append(ipRows, winRow(ip, now))
		// 後ろほど古い = スコアが低い。
		shared = append(shared, sharedRow(n, ip, now.Add(-time.Duration(i)*30*relDay)))
	}
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}, ipRows: ipRows, sharedRows: shared}
	h, users, _ := newRelatedHandler(t, stub)
	for _, n := range names {
		users.Users[n] = &model.User{ID: n, Username: n}
	}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target","limit":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	page1 := decodeRelated(t, rec.Body.Bytes())
	assert.True(t, page1.HasMore)
	require.Len(t, page1.Candidates, 2)
	assert.Equal(t, []string{"u1", "u2"},
		[]string{page1.Candidates[0].User.ID, page1.Candidates[1].User.ID})

	rec = doPost(h.IPRelatedAccounts, `{"userId":"target","limit":2,"offset":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	page2 := decodeRelated(t, rec.Body.Bytes())
	assert.False(t, page2.HasMore)
	require.Len(t, page2.Candidates, 2)
	assert.Equal(t, []string{"u3", "u4"},
		[]string{page2.Candidates[0].User.ID, page2.Candidates[1].User.ID})

	// 端を越えたら空。panic させない。
	rec = doPost(h.IPRelatedAccounts, `{"userId":"target","limit":2,"offset":100}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	beyond := decodeRelated(t, rec.Body.Bytes())
	assert.Empty(t, beyond.Candidates)
	assert.False(t, beyond.HasMore)
}

// **利用者の行が引けない候補は出さない。** 落とした件数は伝える。
func TestIPRelated_SkipsUnresolvableCandidates(t *testing.T) {
	now := time.Now()
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
		sharedRows: []repository.UserIPSharedRow{
			sharedRow("gone", "192.0.2.1", now),
			sharedRow("u1", "192.0.2.1", now.Add(-relDay)),
		},
	}
	h, users, _ := newRelatedHandler(t, stub)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	require.Len(t, got.Candidates, 1)
	assert.Equal(t, "u1", got.Candidates[0].User.ID)
	assert.Equal(t, 1, got.DroppedCount)
	assert.NotContains(t, rec.Body.String(), `"gone"`)
}

// 記録が無効でも、残っている記録からは引ける (#3066 §7)。
func TestIPRelated_ReportsLoggingAndHistorySeparately(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name             string
		logging, hasAny  bool
		ipRows           []repository.UserIPWindowRow
		wantLog, wantHas bool
	}{
		{"記録は無効だが過去の記録はある", false, true,
			[]repository.UserIPWindowRow{winRow("192.0.2.1", now)}, false, true},
		{"記録は有効だがまだ 1 件も無い", true, false, nil, true, false},
		{"記録は無効で記録も無い", false, false, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: tc.hasAny}, ipRows: tc.ipRows}
			h, _, meta := newRelatedHandler(t, stub)
			meta.Meta.EnableIPLogging = tc.logging

			rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			got := decodeRelated(t, rec.Body.Bytes())
			assert.Equal(t, tc.wantLog, got.LoggingEnabled)
			assert.Equal(t, tc.wantHas, got.HasAnyHistory)
		})
	}
}

// 存在しない利用者は NO_SUCH_USER。
func TestIPRelated_UnknownUser(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, _, _ := newRelatedHandler(t, stub)

	rec := doPost(h.IPRelatedAccounts, `{"userId":"nope"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Zero(t, stub.sharedCalls)
}

// **リモート利用者は引く前に断る。** `user_ip` は自インスタンスで認証を通した
// 利用者しか書かないので、必ず「候補なし」になる。それを「関連が無い」と
// 読ませない。
func TestIPRelated_RejectsRemoteUser(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, users, _ := newRelatedHandler(t, stub)
	host := "remote.example"
	users.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"remote"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, stub.sharedCalls)
}

// パラメータ検証。丸めて 200 にしない (#3025 のカーソルと同じ理由)。
func TestIPRelated_RejectsBadParams(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"userId":""}`,
		`{"userId":"target","sinceDays":0}`,
		`{"userId":"target","sinceDays":3651}`,
		`{"userId":"target","limit":0}`,
		`{"userId":"target","limit":101}`,
		`{"userId":"target","offset":-1}`,
		`{"userId":"target","offset":10001}`,
		`{"userId":"target","offset":"5"}`,
		`{"userId":"target","limit":true}`,
		`{"userId":123}`,
		`not json`,
	} {
		t.Run(body, func(t *testing.T) {
			stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
			h, _, _ := newRelatedHandler(t, stub)

			rec := doPost(h.IPRelatedAccounts, body, adminUser)
			assert.GreaterOrEqual(t, rec.Code, 400)
			assert.Less(t, rec.Code, 500)
			assert.Zero(t, stub.sharedCalls, "不正なパラメータのまま DB を引いている")
		})
	}
}

// sinceDays が窓に効く。効かないと画面の指定が無視される。
func TestIPRelated_SinceDaysNarrowsWindow(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, _, _ := newRelatedHandler(t, stub)

	before := time.Now()
	rec := doPost(h.IPRelatedAccounts, `{"userId":"target","sinceDays":7}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.WithinDuration(t, before.AddDate(0, 0, -7), stub.gotIPsSince, time.Minute)
	assert.Equal(t, 7, decodeRelated(t, rec.Body.Bytes()).SinceDays)
}

// 障害はすべて 500。**空の候補にしない** — 「関連する候補は無い」という誤った
// 事実になり、調査の結論を反転させる (#2792)。
func TestIPRelated_FailuresAreInternal(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) *apiadmin.Handler
		logWant string
	}{
		{"未配線", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newRelatedHandler(t, nil)
			return h
		}, "dependencies are not wired"},
		{"対象の解決が失敗", func(t *testing.T) *apiadmin.Handler {
			h, users, _ := newRelatedHandler(t, &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}})
			users.FindErr = assertError{}
			return h
		}, "target lookup failed"},
		{"meta を読めない", func(t *testing.T) *apiadmin.Handler {
			h, _, meta := newRelatedHandler(t, &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}})
			meta.Meta = nil
			return h
		}, "meta fetch failed"},
		{"起点の IP を引けない", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newRelatedHandler(t, &stubIPRelated{
				stubIPSearch: stubIPSearch{hasAny: true}, ipErr: assertError{}})
			return h
		}, "target ips failed"},
		{"記録の有無を確かめられない", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newRelatedHandler(t, &stubIPRelated{
				stubIPSearch: stubIPSearch{hasAnyErr: assertError{}}})
			return h
		}, "history probe failed"},
		{"共有 IP を引けない", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newRelatedHandler(t, &stubIPRelated{
				stubIPSearch: stubIPSearch{hasAny: true},
				ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
				sharedErr:    assertError{}})
			return h
		}, "shared lookup failed"},
		{"候補の解決が失敗", func(t *testing.T) *apiadmin.Handler {
			h, users, _ := newRelatedHandler(t, &stubIPRelated{
				stubIPSearch: stubIPSearch{hasAny: true},
				ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
				sharedRows:   []repository.UserIPSharedRow{sharedRow("u1", "192.0.2.1", now)}})
			users.FindManyByIDsErr = assertError{}
			return h
		}, "candidate lookup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureSlog(t)
			h := tc.prepare(t)

			rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.JSONEq(t, adminInternalErrorJSON, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), `"candidates"`,
				"障害なのに結果の形を返している")
			assert.Contains(t, logged.String(), "admin/ip/related-accounts",
				"応答は汎用 500 なので、ログが唯一の手掛かり")
			assert.Contains(t, logged.String(), tc.logWant)
		})
	}
}

// 行が引けていれば記録はあるので、probe を投げない。
func TestIPRelated_SkipsHistoryProbeWhenIPsExist(t *testing.T) {
	now := time.Now()
	stub := &stubIPRelated{
		// probe が走れば 500 になる。走らなければ 200 のまま。
		stubIPSearch: stubIPSearch{hasAnyErr: assertError{}},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
	}
	h, _, _ := newRelatedHandler(t, stub)

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, "起点の IP が引けているのに probe を投げている")
	assert.True(t, decodeRelated(t, rec.Body.Bytes()).HasAnyHistory)
}

// **1 つの IP から取る候補には上限がある。** 無いと CGNAT の IP 1 本で
// 数千件をランク付けすることになり、走査を有界にした意味が消える。
func TestIPRelated_CapsCandidatesPerIP(t *testing.T) {
	now := time.Now()
	shared := make([]repository.UserIPSharedRow, 0, 250)
	users := make([]string, 0, 250)
	for i := 0; i < 250; i++ {
		id := "u" + strconv.Itoa(i)
		users = append(users, id)
		shared = append(shared, sharedRow(id, "192.0.2.1", now))
	}
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
		sharedRows:   shared,
	}
	h, repoUsers, _ := newRelatedHandler(t, stub)
	for _, u := range users {
		repoUsers.Users[u] = &model.User{ID: u, Username: u}
	}

	// limit を上限より大きく取り、切れているかを 1 ページで見る。
	rec := doPost(h.IPRelatedAccounts, `{"userId":"target","limit":100,"offset":100}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	// 200 件で切れていれば offset 100 の次ページは 100 件ちょうどで終わる。
	assert.Len(t, got.Candidates, 100)
	assert.False(t, got.HasMore, "1 IP あたりの候補が上限で切れていない")
}

// **上限まで見えたアカウント数は「これ以上」でしかない。** 正確に数えるには
// その IP の全行を読む必要があり、それは走査が有界でなくなる。
func TestIPRelated_ReportsAccountCountAsLowerBound(t *testing.T) {
	now := time.Now()
	shared := make([]repository.UserIPSharedRow, 0, 201)
	users := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		id := "u" + strconv.Itoa(i)
		users = append(users, id)
		shared = append(shared, sharedRow(id, "192.0.2.1", now))
	}
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
		sharedRows:   shared,
	}
	h, repoUsers, _ := newRelatedHandler(t, stub)
	for _, u := range users {
		repoUsers.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	require.NotEmpty(t, got.Candidates)
	p := got.Candidates[0].SharedIPs[0]
	assert.Equal(t, 201, p.IPAccountCount)
	assert.True(t, p.IPAccountCountIsLowerBound,
		"上限まで見えた数を確定値として出している")
}

// **起点に無い IP の行は捨てる。** 混ざると対象側の最終観測がゼロ値になり、
// `0001-01-01` を「観測日時」として画面に出すことになる。
func TestIPRelated_DropsRowsForUnknownIPs(t *testing.T) {
	now := time.Now()
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true},
		ipRows:       []repository.UserIPWindowRow{winRow("192.0.2.1", now)},
		sharedRows: []repository.UserIPSharedRow{
			sharedRow("u1", "192.0.2.1", now),
			// 起点に無い IP。現在の SQL では返らないが、返っても描かない。
			sharedRow("u2", "198.51.100.9", now),
		},
	}
	h, users, _ := newRelatedHandler(t, stub)
	for _, u := range []string{"u1", "u2"} {
		users.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	require.Len(t, got.Candidates, 1, "起点に無い IP の行が候補になっている")
	assert.Equal(t, "u1", got.Candidates[0].User.ID)
	assert.NotContains(t, rec.Body.String(), "0001-01-01")
}

// **両方の打ち切りを同時に伝える。** 片方に潰すと、そのとき残りの説明が画面から
// 丸ごと落ちる (IPv6 の起点 50 本超と CGNAT の IP は同時に起きやすい)。
func TestIPRelated_ReportsBothTruncationCauses(t *testing.T) {
	now := time.Now()
	ipRows := make([]repository.UserIPWindowRow, 0, 60)
	for i := 0; i < 60; i++ {
		ipRows = append(ipRows, winRow("192.0.2."+strconv.Itoa(i), now))
	}
	shared := make([]repository.UserIPSharedRow, 0, 201)
	users := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		id := "u" + strconv.Itoa(i)
		users = append(users, id)
		shared = append(shared, sharedRow(id, "192.0.2.0", now))
	}
	stub := &stubIPRelated{
		stubIPSearch: stubIPSearch{hasAny: true}, ipRows: ipRows, sharedRows: shared,
	}
	h, repoUsers, _ := newRelatedHandler(t, stub)
	for _, u := range users {
		repoUsers.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeRelated(t, rec.Body.Bytes())
	assert.True(t, got.Truncated)
	assert.True(t, got.TargetIPsTruncated, "起点の打ち切りが落ちている")
	assert.True(t, got.CandidatesTruncated, "候補側の打ち切りが落ちている")
}

// **対象が root / 管理者なら断ること。**
//
// `admin/show-user` は同じ相手に `ACCESS_DENIED` を返すのに、こちらは候補一覧と
// 共有 IP (生のアドレス) を返していた。候補ゼロでも `targetIpCount` /
// `hasAnyHistory` から「その管理者の IP 記録が何本あるか」が分かる。
func TestIPRelated_RefusesPrivilegedTarget(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, users, meta := newRelatedHandler(t, stub)

	// meta.rootUserId が対象を指す = root。
	rootID := "target"
	meta.Meta.RootUserID = &rootID
	users.Users["target"] = &model.User{ID: "target", Username: "target"}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"root を対象にした関連アカウント検索は断ること")
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
}

// 普通の利用者は従来どおり引けること (通る集合を狭めていない)。
func TestIPRelated_AllowsOrdinaryTarget(t *testing.T) {
	stub := &stubIPRelated{stubIPSearch: stubIPSearch{hasAny: true}}
	h, users, _ := newRelatedHandler(t, stub)
	users.Users["target"] = &model.User{ID: "target", Username: "target"}

	rec := doPost(h.IPRelatedAccounts, `{"userId":"target"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
}
