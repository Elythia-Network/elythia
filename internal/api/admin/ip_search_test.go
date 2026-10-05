package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/iplog"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// stubIPSearch records what the handler asked for and hands back canned rows.
type stubIPSearch struct {
	gotIP     string
	gotSince  time.Time
	gotLimit  int
	gotOffset int
	calls     int

	rows    []repository.UserIPAccountRow
	listErr error

	hasAny    bool
	hasAnyErr error
}

func (s *stubIPSearch) ListAccountsByIP(ip string, since time.Time, limit, offset int) ([]repository.UserIPAccountRow, error) {
	s.calls++
	s.gotIP, s.gotSince, s.gotLimit, s.gotOffset = ip, since, limit, offset
	return s.rows, s.listErr
}

func (s *stubIPSearch) HasAnyHistory() (bool, error) { return s.hasAny, s.hasAnyErr }

// #3105 の 2 本はここでは使わない。`stubIPRelated` が上書きする。
func (s *stubIPSearch) ListIPsByUser(string, time.Time, int) ([]repository.UserIPWindowRow, error) {
	return nil, nil
}

func (s *stubIPSearch) ListSharedIPAccounts([]string, time.Time, int) ([]repository.UserIPSharedRow, error) {
	return nil, nil
}

type ipAccountJSON struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	IsSuspended      bool    `json:"isSuspended"`
	IsDeleted        bool    `json:"isDeleted"`
	LastActiveDate   *string `json:"lastActiveDate"`
	FirstSeenAt      string  `json:"firstSeenAt"`
	LastSeenAt       string  `json:"lastSeenAt"`
	ObservationCount int     `json:"observationCount"`
}

type ipAccountsJSON struct {
	IP             string          `json:"ip"`
	LoggingEnabled bool            `json:"loggingEnabled"`
	HasAnyHistory  bool            `json:"hasAnyHistory"`
	SinceDays      int             `json:"sinceDays"`
	RetentionDays  int             `json:"retentionDays"`
	Limit          int             `json:"limit"`
	Offset         int             `json:"offset"`
	HasMore        bool            `json:"hasMore"`
	DroppedCount   int             `json:"droppedCount"`
	Accounts       []ipAccountJSON `json:"accounts"`
}

func decodeIPAccounts(t *testing.T, body []byte) ipAccountsJSON {
	t.Helper()
	var out ipAccountsJSON
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// newIPSearchHandler wires the stub and one known user.
func newIPSearchHandler(t *testing.T, stub *stubIPSearch) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository) {
	t.Helper()
	h, users, meta, _ := newTestHandler(t)
	if stub != nil {
		h.SetIPSearchRepo(stub)
	}
	return h, users, meta
}

func ipRow(userID string, first, last time.Time, count int) repository.UserIPAccountRow {
	return repository.UserIPAccountRow{
		UserID: userID, FirstSeen: first, LastSeen: last, ObservationCount: count,
	}
}

func TestIPAccounts_ReturnsCandidates(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	first := time.Date(2026, 9, 1, 12, 4, 5, 600*int(time.Millisecond), jst)
	last := time.Date(2026, 9, 18, 21, 0, 0, 0, jst)
	active := time.Date(2026, 9, 19, 9, 30, 0, 0, jst)
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		ipRow("u1", first, last, 12),
	}}
	h, users, meta := newIPSearchHandler(t, stub)
	meta.Meta.EnableIPLogging = true
	users.Users["u1"] = &model.User{
		ID: "u1", Username: "alice", IsSuspended: true, LastActiveDate: &active,
	}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.Equal(t, "192.0.2.1", got.IP)
	assert.True(t, got.LoggingEnabled)
	assert.True(t, got.HasAnyHistory)
	assert.False(t, got.HasMore)
	require.Len(t, got.Accounts, 1)
	assert.Zero(t, got.DroppedCount, "落としていないのに件数が立っている")
	a := got.Accounts[0]
	assert.Equal(t, "u1", a.User.ID)
	assert.Equal(t, "alice", a.User.Username)
	assert.True(t, a.IsSuspended)
	assert.False(t, a.IsDeleted)
	// **時刻は UTC へ寄せて返す。** `.UTC()` を落とすと、末尾 `Z` を付けたまま
	// ローカル時刻を出すので、観測の前後関係を 9 時間ずらして読ませる。
	assert.Equal(t, "2026-09-01T03:04:05.600Z", a.FirstSeenAt)
	assert.Equal(t, "2026-09-18T12:00:00.000Z", a.LastSeenAt)
	require.NotNil(t, a.LastActiveDate)
	assert.Equal(t, "2026-09-19T00:30:00.000Z", *a.LastActiveDate)
	assert.Equal(t, 12, a.ObservationCount)
}

// **正規化した形で引く。** 記録側は正規形しか書かないので、揺れたまま引くと
// 当たらない。応答の `ip` も検索に使った形で返す (入力のエコーにしない)。
func TestIPAccounts_NormalizesBeforeSearch(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"::ffff:192.0.2.1", "192.0.2.1"},
		{"2001:DB8::0001", "2001:db8::1"},
		{"  192.0.2.1  ", "192.0.2.1"},
		{"192.0.2.1:5678", "192.0.2.1"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			stub := &stubIPSearch{hasAny: true}
			h, _, _ := newIPSearchHandler(t, stub)

			rec := doPost(h.IPAccounts, `{"ip":`+quoteJSON(tc.in)+`}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.want, stub.gotIP, "repository へ渡る IP が正規形でない")
			assert.Equal(t, tc.want, decodeIPAccounts(t, rec.Body.Bytes()).IP)
		})
	}
}

// IP として読めない値は 400。列に入らない NUL もここで落ちる (#3025)。
func TestIPAccounts_RejectsBadIP(t *testing.T) {
	for _, in := range []string{
		"", "   ", "not-an-ip", "example.com", "192.0.2.0/24", "256.0.0.1",
		"192.0.2.1\x00", "\x00",
	} {
		t.Run(in, func(t *testing.T) {
			stub := &stubIPSearch{hasAny: true}
			h, _, _ := newIPSearchHandler(t, stub)

			rec := doPost(h.IPAccounts, `{"ip":`+quoteJSON(in)+`}`, adminUser)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Zero(t, stub.calls, "不正な IP で DB を引いている")
		})
	}
}

// body 無しでも 400 (IP は必須)。既定で全件を引かせない。
func TestIPAccounts_MissingBodyIsBadRequest(t *testing.T) {
	stub := &stubIPSearch{hasAny: true}
	h, _, _ := newIPSearchHandler(t, stub)

	rec := doPost(h.IPAccounts, ``, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, stub.calls)
}

// 既定値: 90 日・30 件・offset 0。**+1 件引く**ので limit は 31 で渡る。
func TestIPAccounts_Defaults(t *testing.T) {
	stub := &stubIPSearch{hasAny: true}
	h, _, _ := newIPSearchHandler(t, stub)

	before := time.Now()
	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, 31, stub.gotLimit, "hasMore 判定のための +1 件が渡っていない")
	assert.Equal(t, 0, stub.gotOffset)
	assert.WithinDuration(t, before.AddDate(0, 0, -90), stub.gotSince, time.Minute)

	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.Equal(t, 90, got.SinceDays)
	// **保持期間を画面へ渡す。** 「一致なし」を「もう消した」と説明できる材料で、
	// 掃除の実際の基準 (`iplog.Retention`) と同じ値でなければ画面が嘘をつく。
	assert.Equal(t, int(iplog.Retention/(24*time.Hour)), got.RetentionDays)
	// 応答の limit は利用者が受け取る件数。+1 を漏らさない。
	assert.Equal(t, 30, got.Limit)
	assert.Equal(t, 0, got.Offset)
	assert.NotNil(t, got.Accounts, "行が無くても null ではなく空配列")
}

// sinceDays が窓に効くこと。効かないと画面の指定が無視される。
func TestIPAccounts_SinceDaysNarrowsWindow(t *testing.T) {
	stub := &stubIPSearch{hasAny: true}
	h, _, _ := newIPSearchHandler(t, stub)

	before := time.Now()
	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1","sinceDays":7}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.WithinDuration(t, before.AddDate(0, 0, -7), stub.gotSince, time.Minute)
	assert.Equal(t, 7, decodeIPAccounts(t, rec.Body.Bytes()).SinceDays)
}

// 範囲外のパラメータは 400。**丸めて 200 にしない** — 利用者が指定した窓や
// ページと無関係な結果を「正しい応答」として返すことになる (#3025 のカーソルと同じ理由)。
func TestIPAccounts_RejectsOutOfRangeParams(t *testing.T) {
	for _, body := range []string{
		`{"ip":"192.0.2.1","sinceDays":0}`,
		`{"ip":"192.0.2.1","sinceDays":-1}`,
		`{"ip":"192.0.2.1","sinceDays":3651}`,
		`{"ip":"192.0.2.1","limit":0}`,
		`{"ip":"192.0.2.1","limit":101}`,
		`{"ip":"192.0.2.1","offset":-1}`,
		`{"ip":"192.0.2.1","offset":10001}`,
	} {
		t.Run(body, func(t *testing.T) {
			stub := &stubIPSearch{hasAny: true}
			h, _, _ := newIPSearchHandler(t, stub)

			rec := doPost(h.IPAccounts, body, adminUser)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Zero(t, stub.calls, "範囲外のまま DB を引いている")
		})
	}
}

// 境界は通る。片側だけ弾いていないことを見る。
func TestIPAccounts_AcceptsBoundaryParams(t *testing.T) {
	for _, tc := range []struct {
		body        string
		limit, off  int
		wantSinceDs int
	}{
		{`{"ip":"192.0.2.1","sinceDays":1}`, 31, 0, 1},
		{`{"ip":"192.0.2.1","sinceDays":3650}`, 31, 0, 3650},
		{`{"ip":"192.0.2.1","limit":1}`, 2, 0, 90},
		{`{"ip":"192.0.2.1","limit":100}`, 101, 0, 90},
		{`{"ip":"192.0.2.1","offset":0}`, 31, 0, 90},
		{`{"ip":"192.0.2.1","offset":10000}`, 31, 10000, 90},
	} {
		t.Run(tc.body, func(t *testing.T) {
			stub := &stubIPSearch{hasAny: true}
			h, _, _ := newIPSearchHandler(t, stub)

			rec := doPost(h.IPAccounts, tc.body, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.limit, stub.gotLimit)
			assert.Equal(t, tc.off, stub.gotOffset)
			assert.Equal(t, tc.wantSinceDs, decodeIPAccounts(t, rec.Body.Bytes()).SinceDays)
		})
	}
}

// **+1 件目は返さず hasMore で伝える。** そのまま返すと limit を超えた件数が出て、
// 次ページの先頭が重複する。
func TestIPAccounts_HasMoreTrimsExtraRow(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		ipRow("u1", now, now, 1), ipRow("u2", now, now, 1), ipRow("u3", now, now, 1),
	}}
	h, users, _ := newIPSearchHandler(t, stub)
	for _, u := range []string{"u1", "u2", "u3"} {
		users.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1","limit":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.True(t, got.HasMore)
	require.Len(t, got.Accounts, 2)
	assert.Equal(t, "u1", got.Accounts[0].User.ID)
	assert.Equal(t, "u2", got.Accounts[1].User.ID)
}

// ちょうど limit 件なら hasMore は false。
func TestIPAccounts_ExactPageHasNoMore(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		ipRow("u1", now, now, 1), ipRow("u2", now, now, 1),
	}}
	h, users, _ := newIPSearchHandler(t, stub)
	for _, u := range []string{"u1", "u2"} {
		users.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1","limit":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.False(t, got.HasMore)
	assert.Len(t, got.Accounts, 2)
}

// **repository の並びを保つ。** 最終観測の新しい順という根拠が、利用者の解決
// 順 (map) で入れ替わると、画面の「最近使った順」が嘘になる。
func TestIPAccounts_KeepsRepositoryOrder(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		ipRow("zzz", now, now, 1), ipRow("aaa", now, now, 1), ipRow("mmm", now, now, 1),
	}}
	h, users, _ := newIPSearchHandler(t, stub)
	for _, u := range []string{"zzz", "aaa", "mmm"} {
		users.Users[u] = &model.User{ID: u, Username: u}
	}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	require.Len(t, got.Accounts, 3)
	assert.Equal(t, []string{"zzz", "aaa", "mmm"},
		[]string{got.Accounts[0].User.ID, got.Accounts[1].User.ID, got.Accounts[2].User.ID})
}

// **利用者の行が引けない観測は候補にしない。** `user_ip.userId` には **FK が
// 無い**ので、アカウントを完全削除しても行が残る。名前もアバターも出せない候補は
// 調査の材料にならない。
func TestIPAccounts_SkipsUnresolvableUsers(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{
		ipRow("gone", now, now, 1), ipRow("u1", now, now, 1),
	}}
	h, users, _ := newIPSearchHandler(t, stub)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	require.Len(t, got.Accounts, 1)
	assert.Equal(t, "u1", got.Accounts[0].User.ID)
	assert.NotContains(t, rec.Body.String(), "gone")
	assert.Equal(t, 1, got.DroppedCount, "落とした件数が伝わっていない")
}

// **`loggingEnabled` と `hasAnyHistory` は別の問い。** 記録が今は無効でも過去の
// 記録は検索できる (#3066 §7)。片方に潰すと「記録が無い」と誤って断定させる。
func TestIPAccounts_ReportsLoggingAndHistorySeparately(t *testing.T) {
	for _, tc := range []struct {
		name             string
		logging, hasAny  bool
		wantLog, wantHas bool
	}{
		{"両方 true", true, true, true, true},
		{"記録は無効だが過去の記録はある", false, true, false, true},
		{"記録は有効だがまだ 1 件も無い", true, false, true, false},
		{"両方 false", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubIPSearch{hasAny: tc.hasAny}
			h, _, meta := newIPSearchHandler(t, stub)
			meta.Meta.EnableIPLogging = tc.logging

			rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			got := decodeIPAccounts(t, rec.Body.Bytes())
			assert.Equal(t, tc.wantLog, got.LoggingEnabled)
			assert.Equal(t, tc.wantHas, got.HasAnyHistory)
		})
	}
}

// **記録が無効でも検索は通る。** 残っている過去の記録を引けるのが #3066 §7 の要求。
func TestIPAccounts_SearchesWhileLoggingDisabled(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{ipRow("u1", now, now, 3)}}
	h, users, meta := newIPSearchHandler(t, stub)
	meta.Meta.EnableIPLogging = false
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.False(t, got.LoggingEnabled)
	assert.Len(t, got.Accounts, 1, "記録が無効だと過去の記録まで引けなくなっている")
}

// 障害はすべて 500。**空の結果にしない** — 「その IP を使ったアカウントは無い」
// という誤った事実になり、調査の結論を反転させる (#2792)。
func TestIPAccounts_FailuresAreInternal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) *apiadmin.Handler
		logWant string
	}{
		{"未配線", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newIPSearchHandler(t, nil)
			return h
		}, "dependencies are not wired"},
		{"検索が失敗", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newIPSearchHandler(t, &stubIPSearch{hasAny: true, listErr: assertError{}})
			return h
		}, "lookup failed"},
		{"記録の有無を確かめられない", func(t *testing.T) *apiadmin.Handler {
			h, _, _ := newIPSearchHandler(t, &stubIPSearch{hasAnyErr: assertError{}})
			return h
		}, "history probe failed"},
		{"meta を読めない", func(t *testing.T) *apiadmin.Handler {
			h, _, meta := newIPSearchHandler(t, &stubIPSearch{hasAny: true})
			meta.Meta = nil
			return h
		}, "meta fetch failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureSlog(t)
			h := tc.prepare(t)

			rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.JSONEq(t, adminInternalErrorJSON, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), `"accounts"`,
				"障害なのに結果の形を返している")
			assert.Contains(t, logged.String(), "admin/ip/accounts",
				"応答は汎用 500 なので、ログが唯一の手掛かり")
			assert.Contains(t, logged.String(), tc.logWant)
		})
	}
}

// 利用者の解決が失敗したときも 500。**空にして「候補なし」と言わない。**
func TestIPAccounts_UserLookupFailureIsInternal(t *testing.T) {
	now := time.Now()
	stub := &stubIPSearch{hasAny: true, rows: []repository.UserIPAccountRow{ipRow("u1", now, now, 1)}}
	h, users, _ := newIPSearchHandler(t, stub)
	users.FindManyByIDsErr = assertError{}
	logged := captureSlog(t)

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"accounts"`)
	assert.Contains(t, logged.String(), "user lookup failed")
}

// quoteJSON escapes a value for embedding in a JSON body (NUL を含む値も送る)。
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// **型が違うパラメータは 400。** `encoding/json` は `*int` のポインタを
// デコードする前に確保するので、`{"offset":"99"}` でも `req.Offset` は非 nil の
// `*0` になる。Bind のエラーを捨てると範囲検査を素通りして `offset = 0` が
// 採用され、**利用者の指定と無関係なページを 200 で返す**。
func TestIPAccounts_RejectsMalformedBody(t *testing.T) {
	for _, body := range []string{
		`{"ip":"192.0.2.1","offset":"99999"}`,
		`{"ip":"192.0.2.1","offset":true}`,
		`{"ip":"192.0.2.1","offset":{}}`,
		`{"ip":"192.0.2.1","offset":[]}`,
		`{"ip":"192.0.2.1","limit":"30"}`,
		`{"ip":"192.0.2.1","sinceDays":"7"}`,
		`{"ip":192}`,
		`{"ip":"192.0.2.1"`,
		`not json`,
	} {
		t.Run(body, func(t *testing.T) {
			stub := &stubIPSearch{hasAny: true}
			h, _, _ := newIPSearchHandler(t, stub)

			rec := doPost(h.IPAccounts, body, adminUser)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Zero(t, stub.calls, "壊れた body のまま DB を引いている")
		})
	}
}

// **1 ページが丸ごと「利用者を解決できない観測」でも `hasMore` は立てる。**
// `hasMore` は解決の前の行数で決まるので、そこで打ち切ると削除済みアカウントが
// 30 件並んだだけで調査が止まる。
func TestIPAccounts_EmptyPageStillReportsHasMore(t *testing.T) {
	now := time.Now()
	rows := make([]repository.UserIPAccountRow, 0, 3)
	for _, u := range []string{"gone1", "gone2", "gone3"} {
		rows = append(rows, ipRow(u, now, now, 1))
	}
	stub := &stubIPSearch{hasAny: true, rows: rows}
	h, _, _ := newIPSearchHandler(t, stub)
	// 利用者は 1 件も登録しない = すべて解決できない。

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1","limit":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.Empty(t, got.Accounts)
	assert.True(t, got.HasMore, "解決できない観測で hasMore が落ちている")
	// **落とした件数を伝える。** これが無いと画面は「0 行ヒット」と
	// 「N 行ヒットして全部落とした」を区別できず、記録が残っているのに
	// 「接続は記録されていません」と断定することになる。
	assert.Equal(t, 2, got.DroppedCount)
	assert.Equal(t, 2, got.Limit, "次の offset を作るための limit が返っていない")
	assert.Equal(t, 0, got.Offset)
}

// **最後のページで全件落ちても「一致なし」と同じ応答にしない。**
//
// `hasMore` は「引けた行が limit を超えたか」で決まるので、行数が limit 以下で
// 全件落ちると `accounts: []` + `hasMore: false` になり、**「一致する行が 1 件も
// 無い」ときとバイト単位で同じ応答**になる。`droppedCount` だけがそこを分ける。
// しかもこれは**荒らしの使い捨てアカウントを消した後にその IP を引く**という、
// この endpoint が要る場面そのもの (`user_ip` に FK が無いので行だけ残る)。
func TestIPAccounts_LastPageAllDroppedIsNotNoMatch(t *testing.T) {
	now := time.Now()
	rows := []repository.UserIPAccountRow{
		ipRow("gone1", now, now, 4), ipRow("gone2", now, now, 2),
	}
	stub := &stubIPSearch{hasAny: true, rows: rows}
	h, _, _ := newIPSearchHandler(t, stub)
	// 利用者は 1 件も登録しない = すべて解決できない。

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	assert.Empty(t, got.Accounts)
	assert.False(t, got.HasMore, "limit 以下なのに hasMore が立っている")
	assert.Equal(t, 2, got.DroppedCount,
		"最後のページで全件落ちたことが伝わらず、「一致なし」と同じ応答になっている")
	assert.True(t, got.HasAnyHistory)
}

// **行が引けていれば記録はある。** そのときは probe を投げない — `SELECT EXISTS`
// はテーブルの走査になり、90 日の掃除が大量に消した直後は dead tuple を全部読む。
func TestIPAccounts_SkipsHistoryProbeWhenRowsExist(t *testing.T) {
	now := time.Now()
	// probe が走れば 500 になる stub。走らなければ 200 のまま。
	stub := &stubIPSearch{
		hasAnyErr: assertError{},
		rows:      []repository.UserIPAccountRow{ipRow("u1", now, now, 1)},
	}
	h, users, _ := newIPSearchHandler(t, stub)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, "行が引けているのに probe を投げている")
	assert.True(t, decodeIPAccounts(t, rec.Body.Bytes()).HasAnyHistory)
}

// 保持期間は `core/iplog` の値そのもの。既定の窓もそこから導く。
func TestIPAccounts_RetentionComesFromIPLog(t *testing.T) {
	stub := &stubIPSearch{hasAny: true}
	h, _, _ := newIPSearchHandler(t, stub)

	rec := doPost(h.IPAccounts, `{"ip":"192.0.2.1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeIPAccounts(t, rec.Body.Bytes())
	want := int(iplog.Retention / (24 * time.Hour))
	assert.Equal(t, want, got.RetentionDays)
	assert.Equal(t, want, got.SinceDays, "既定の窓が保持期間から導かれていない")
}
