package hashtags_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/hashtags"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var testDB *gorm.DB

func init() {
	testDB = testutil.MustOpenTestDB()
	testutil.ApplyMigrations(testDB)
}

func newHandler() *hashtags.Handler {
	h := hashtags.NewHandler(testDB)
	// #2106 L25: 設定済 ID generator を共有する経路を test でも通す。
	gen, _ := id.NewGenerator("aidx")
	h.SetIDGen(gen)
	return h
}

func brokenHandler() *hashtags.Handler {
	db := testutil.MustOpenTestDB()
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
	return hashtags.NewHandler(db)
}

func doPost(h func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h(c)
	return rec
}

// doPostAs is doPost with an authenticated viewer set in the context (#1957-a).
func doPostAs(h func(echo.Context) error, body, viewerID string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{ID: viewerID})
	_ = h(c)
	return rec
}

func cleanup() {
	testDB.Exec(`DELETE FROM "hashtag"`)
}

// --- List ---

func TestList_Success(t *testing.T) {
	cleanup()
	assert.Equal(t, http.StatusOK, doPost(newHandler().List, `{"sort":"+mentionedUsers"}`).Code)
}

func TestList_WithData(t *testing.T) {
	cleanup()
	testDB.Create(&model.Hashtag{ID: "ht1", Name: "golang", MentionedUsersCount: 5})
	defer cleanup()
	rec := doPost(newHandler().List, `{"limit":10,"sort":"+mentionedUsers"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "golang")
}

func TestList_WithOffset(t *testing.T) {
	assert.Equal(t, http.StatusOK, doPost(newHandler().List, `{"limit":5,"offset":1,"sort":"+mentionedUsers"}`).Code)
}

func TestList_LimitCap(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().List, `{"limit":999,"sort":"+mentionedUsers"}`).Code)
}

func TestList_InvalidJSON(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().List, `invalid`).Code)
}

// TestList_SortRequired: upstream Misskey TS は paramDef で sort を required
// にしている。mk-go も同 shape に揃え、sort 抜きは 400 で弾く (#925)。
func TestList_SortRequired(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().List, `{}`).Code)
}

// TestList_SortEnum: upstream paramDef は sort を 12 値の enum 限定にしている。
// mk-go も同 enum に絞り、未定義 sort 値で 400 を返す (#925 review nit)。
func TestList_SortEnum(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().List, `{"sort":"bogus"}`).Code)
}

func TestList_DBError(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError, doPost(brokenHandler().List, `{"sort":"+mentionedUsers"}`).Code)
}

// decodeTagNames は list/search レスポンスを順序保持で tag 名配列に変換する。
func decodeListTagNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["tag"].(string))
	}
	return out
}

// TestList_SortHonored: sort 値に応じて order by が切り替わること (#1544)。
// 旧実装は常に mentionedUsersCount DESC 固定だった。
func TestList_SortHonored(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_s1", Name: "alpha", MentionedUsersCount: 1, AttachedUsersCount: 9})
	testDB.Create(&model.Hashtag{ID: "ht_s2", Name: "bravo", MentionedUsersCount: 9, AttachedUsersCount: 1})

	// +mentionedUsers DESC → bravo(9) が先頭。
	got := decodeListTagNames(t, doPost(newHandler().List, `{"sort":"+mentionedUsers"}`))
	require.Len(t, got, 2)
	assert.Equal(t, "bravo", got[0])

	// -mentionedUsers ASC → alpha(1) が先頭。
	got = decodeListTagNames(t, doPost(newHandler().List, `{"sort":"-mentionedUsers"}`))
	require.Len(t, got, 2)
	assert.Equal(t, "alpha", got[0])

	// +attachedUsers DESC → alpha(9) が先頭 (列が切り替わる)。
	got = decodeListTagNames(t, doPost(newHandler().List, `{"sort":"+attachedUsers"}`))
	require.Len(t, got, 2)
	assert.Equal(t, "alpha", got[0])
}

// TestList_AttachedToFilters: attachedTo* で count!=0 の tag のみ返す (#1544)。
func TestList_AttachedToFilters(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_a1", Name: "withlocal", AttachedUsersCount: 3, AttachedLocalUsersCount: 3, AttachedRemoteUsersCount: 0})
	testDB.Create(&model.Hashtag{ID: "ht_a2", Name: "withremote", AttachedUsersCount: 2, AttachedLocalUsersCount: 0, AttachedRemoteUsersCount: 2})
	testDB.Create(&model.Hashtag{ID: "ht_a3", Name: "noattach", AttachedUsersCount: 0})

	got := decodeListTagNames(t, doPost(newHandler().List, `{"sort":"+mentionedUsers","attachedToUserOnly":true}`))
	assert.ElementsMatch(t, []string{"withlocal", "withremote"}, got)

	got = decodeListTagNames(t, doPost(newHandler().List, `{"sort":"+mentionedUsers","attachedToLocalUserOnly":true}`))
	assert.ElementsMatch(t, []string{"withlocal"}, got)

	got = decodeListTagNames(t, doPost(newHandler().List, `{"sort":"+mentionedUsers","attachedToRemoteUserOnly":true}`))
	assert.ElementsMatch(t, []string{"withremote"}, got)
}

// --- Search ---

func TestSearch_Success(t *testing.T) {
	cleanup()
	testDB.Create(&model.Hashtag{ID: "ht2", Name: "gotest"})
	defer cleanup()
	rec := doPost(newHandler().Search, `{"query":"gotest"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "gotest")
}

func TestSearch_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Search, `{}`).Code)
}

func TestSearch_DBError(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError, doPost(brokenHandler().Search, `{"query":"x"}`).Code)
}

// decodeSearchNames は search レスポンス (string 配列) を順序保持で返す。
func decodeSearchNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var names []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &names))
	return names
}

// TestSearch_PrefixMatch: upstream は前方一致 (name LIKE query+'%')。部分一致では
// ない。query="go" は "golang" に一致するが "mygolang" には一致しない (#1544)。
func TestSearch_PrefixMatch(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_p1", Name: "golang"})
	testDB.Create(&model.Hashtag{ID: "ht_p2", Name: "mygolang"})

	names := decodeSearchNames(t, doPost(newHandler().Search, `{"query":"go"}`))
	assert.Contains(t, names, "golang")
	assert.NotContains(t, names, "mygolang", "部分一致ではなく前方一致")
}

// TestSearch_CaseInsensitivePrefix: query は lowercase 化されて前方一致する。
func TestSearch_CaseInsensitivePrefix(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_ci", Name: "misskey"})
	names := decodeSearchNames(t, doPost(newHandler().Search, `{"query":"Miss"}`))
	assert.Contains(t, names, "misskey")
}

// TestSearch_OrderAndOffset: mentionedLocalUsersCount DESC で並び offset を適用する。
func TestSearch_OrderAndOffset(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_o1", Name: "golow", MentionedLocalUsersCount: 1})
	testDB.Create(&model.Hashtag{ID: "ht_o2", Name: "gohigh", MentionedLocalUsersCount: 9})

	names := decodeSearchNames(t, doPost(newHandler().Search, `{"query":"go"}`))
	require.Len(t, names, 2)
	assert.Equal(t, "gohigh", names[0], "mentionedLocalUsersCount DESC")

	// offset=1 で先頭 (gohigh) を飛ばす。
	names = decodeSearchNames(t, doPost(newHandler().Search, `{"query":"go","offset":1}`))
	require.Len(t, names, 1)
	assert.Equal(t, "golow", names[0])
}

// TestSearch_EscapesWildcards: query 内の LIKE メタ文字 (% _) はリテラル扱い。
func TestSearch_EscapesWildcards(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_w1", Name: "abc"})
	// "%" を前方一致リテラルとして扱うため、"%" は "abc" に一致しない。
	names := decodeSearchNames(t, doPost(newHandler().Search, `{"query":"%"}`))
	assert.NotContains(t, names, "abc")
}

// --- Show ---

func TestShow_Found(t *testing.T) {
	cleanup()
	testDB.Create(&model.Hashtag{ID: "ht3", Name: "rustlang"})
	defer cleanup()
	rec := doPost(newHandler().Show, `{"tag":"rustlang"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "rustlang")
	// L3 (#1270): hashtags/show の実レスポンスを golden Hashtag に突合する。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "Hashtag", resp)
}

// TestShow_BadRequestForUnknownTag: upstream Misskey TS は ApiError 既定動作
// で 400 + NO_SUCH_HASHTAG body を返す (#925)。404 ではなく 400 に揃える。
// HTTP semantics 的には 404 が自然だが drop-in 互換を優先する。
func TestShow_BadRequestForUnknownTag(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Show, `{"tag":"ghost"}`).Code)
}

func TestShow_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Show, `{}`).Code)
}

// TestShow_NormalizesTag: upstream は normalizeForSearch (NFKC+lowercase) で
// 引くため、大文字や全角入力でも格納名 (lowercase) に一致する (#1544)。
// 旧実装は exact match で 'Misskey' が 'misskey' にヒットせず NO_SUCH_HASHTAG。
func TestShow_NormalizesTag(t *testing.T) {
	cleanup()
	defer cleanup()
	testDB.Create(&model.Hashtag{ID: "ht_n1", Name: "misskey"})

	rec := doPost(newHandler().Show, `{"tag":"Misskey"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "misskey")

	// 全角 (NFKC で半角化) でも一致する。
	rec = doPost(newHandler().Show, `{"tag":"ＭＩＳＳＫＥＹ"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Trend ---

// cleanupNotes は Trend 関連 test の前後で note と user (FK) を掃除する。
func cleanupNotes() {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user" WHERE "id" LIKE 'tu_%'`)
}

// ensureUser は note の userId FK を満たすための最小限の user row を
// 用意する。同 id が既存の場合は no-op。
func ensureUser(t *testing.T, userID string) {
	t.Helper()
	testDB.Exec(`
		INSERT INTO "user" ("id", "username", "usernameLower")
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, userID, userID)
}

// insertNote: Trend test 用の最小限 note row を作る。aidx ID を
// createdAt として埋め込み、note.tags に集計対象タグを入れる。
// model.Note 経由だと validation が重いので raw SQL で軽く済ます。
func insertNote(t *testing.T, gen id.Generator, userID string, tags []string, createdAt time.Time) {
	t.Helper()
	insertNoteWithVisibility(t, gen, userID, tags, createdAt, "public")
}

// insertNoteWithVisibility: visibility (public/home/followers/specified)
// を制御できる挿入ヘルパー。trend 集計の visibility フィルタ検証用。
func insertNoteWithVisibility(t *testing.T, gen id.Generator, userID string, tags []string, createdAt time.Time, visibility string) {
	t.Helper()
	ensureUser(t, userID)
	noteID := gen.Generate(createdAt)
	require.NoError(t, testDB.Exec(
		`INSERT INTO "note" ("id", "userId", "tags", "visibility", "text")
		 VALUES (?, ?, ?, ?::note_visibility_enum, '')`,
		noteID, userID, model.StringArray(tags), visibility,
	).Error)
}

func TestTrend_EmptyReturnsEmptyArray(t *testing.T) {
	cleanupNotes()
	rec := doPost(newHandler().Trend, `{}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "no notes → empty trend list")
}

func TestTrend_AggregatesRecentNotes(t *testing.T) {
	cleanupNotes()
	defer cleanupNotes()

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	now := time.Now()
	// alpha: 2 distinct users (tu_1@bucket0, tu_2@bucket1)
	insertNote(t, gen, "tu_1", []string{"alpha"}, now.Add(-5*time.Minute))
	insertNote(t, gen, "tu_2", []string{"alpha"}, now.Add(-15*time.Minute))
	// beta: 1 user (tu_1@bucket0)
	insertNote(t, gen, "tu_1", []string{"beta"}, now.Add(-5*time.Minute))
	// gamma: 1 note だが 200 分超 → bucket 範囲外、cutoff で除外
	insertNote(t, gen, "tu_3", []string{"gamma"}, now.Add(-300*time.Minute))

	rec := doPost(newHandler().Trend, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// alpha (total 2 users) → beta (total 1) の順にランクされる。gamma は除外。
	require.Len(t, resp, 2, "alpha + beta only")
	assert.Equal(t, "alpha", resp[0]["tag"])
	assert.Equal(t, "beta", resp[1]["tag"])

	// chart 配列は 20 buckets 固定
	chart := resp[0]["chart"].([]any)
	require.Len(t, chart, 20, "chart bucket count = 20")

	// alpha: bucket 0 (直近 10 分) と bucket 1 (10-20 分) に各 1 user
	assert.EqualValues(t, 1, chart[0], "alpha bucket 0 has tu_1")
	assert.EqualValues(t, 1, chart[1], "alpha bucket 1 has tu_2")
	for i := 2; i < 20; i++ {
		assert.EqualValues(t, 0, chart[i], "alpha bucket %d empty", i)
	}

	// usersCount は max(chart) で alpha は 1 (各 bucket の users が 1)
	assert.EqualValues(t, 1, resp[0]["usersCount"])
}

func TestTrend_DBError(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError, doPost(brokenHandler().Trend, `{}`).Code)
}

// TestTrend_VisibilityFilterExcludesPrivate guards #678 review:
// followers / specified の note は trend 集計対象外。
func TestTrend_VisibilityFilterExcludesPrivate(t *testing.T) {
	cleanupNotes()
	defer cleanupNotes()

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	now := time.Now()
	// public + home は集計対象、followers + specified は除外。
	insertNoteWithVisibility(t, gen, "tu_p", []string{"public_tag"}, now.Add(-5*time.Minute), "public")
	insertNoteWithVisibility(t, gen, "tu_h", []string{"home_tag"}, now.Add(-5*time.Minute), "home")
	insertNoteWithVisibility(t, gen, "tu_f", []string{"followers_tag"}, now.Add(-5*time.Minute), "followers")
	insertNoteWithVisibility(t, gen, "tu_s", []string{"specified_tag"}, now.Add(-5*time.Minute), "specified")

	rec := doPost(newHandler().Trend, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	// public_tag / home_tag は出る。
	assert.Contains(t, body, `"public_tag"`)
	assert.Contains(t, body, `"home_tag"`)
	// followers_tag / specified_tag は trend に出てはいけない (プライバシー保護)。
	assert.NotContains(t, body, `"followers_tag"`,
		"followers visibility は集計対象外であるべき")
	assert.NotContains(t, body, `"specified_tag"`,
		"specified visibility は集計対象外であるべき")
}

// --- Users ---

// seedTagUser inserts a local/remote user with the given normalized tags for
// the hashtags/users query tests. user.tags は正規化済み (lowercase) で渡す
// 前提 (= Part 2 の populate と同じ。query 側もここで正規化一致を検証する)。
func seedTagUser(t *testing.T, id, username string, tags []string, host *string, suspended bool, followers int, updatedAt time.Time) {
	t.Helper()
	u := &model.User{
		ID:                id,
		Username:          username,
		UsernameLower:     strings.ToLower(username),
		Tags:              model.StringArray(tags),
		Host:              host,
		IsSuspended:       suspended,
		FollowersCount:    followers,
		UpdatedAt:         &updatedAt,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(u).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
}

func decodeUserIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	ids := make([]string, 0, len(list))
	for _, u := range list {
		id, _ := u["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// tag containment は正規化一致で効く: "tokyo" を持つ user は "Tokyo" query で
// ヒットし、別タグの user はヒットしない。あわせて UserDetailed shape も確認。
func TestUsers_ContainmentAndShape(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_a", "htua", []string{"tokyo"}, nil, false, 1, now)
	seedTagUser(t, "u_htu_b", "htub", []string{"osaka"}, nil, false, 1, now)

	rec := doPost(newHandler().Users, `{"tag":"Tokyo","sort":"+follower"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "u_htu_a", list[0]["id"])
	// UserDetailed shape (UserLite + detailed fields) で返ること。
	assert.Equal(t, "htua", list[0]["username"])
	assert.Contains(t, list[0], "followersCount")
	assert.Contains(t, list[0], "notesCount")
}

// #1957-a: hashtags/users の embed user に viewer 視点の relation block が乗る。
// viewer が tag user を follow していれば isFollowing=true、匿名なら省略。
func TestUsers_EmbedsViewerRelation(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_rel", "hturel", []string{"reltag"}, nil, false, 1, now)
	// viewer 自身も user 行が要る (following の FK 制約)。tag 無しなので結果には出ない。
	seedTagUser(t, "viewer1", "htuviewer", nil, nil, false, 0, now)
	// viewer1 -> u_htu_rel の following 行を作る。
	require.NoError(t, repository.NewFollowingRepository(testDB).Create(&model.Following{
		ID: "f_htu_rel", FollowerID: "viewer1", FolloweeID: "u_htu_rel",
	}))
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "f_htu_rel") })

	h := hashtags.NewHandler(testDB)
	h.SetRelationRepos(userrelation.Repos{Following: repository.NewFollowingRepository(testDB)})

	// 認証 viewer: isFollowing=true。
	rec := doPostAs(h.Users, `{"tag":"reltag","sort":"+follower"}`, "viewer1")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, true, rows[0]["isFollowing"], "認証 viewer の embed user に isFollowing=true (#1957-a)")

	// 匿名: relation 省略。
	rec = doPost(h.Users, `{"tag":"reltag","sort":"+follower"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	require.Len(t, anon, 1)
	_, has := anon[0]["isFollowing"]
	assert.False(t, has, "匿名には relation を出さない (#1957-a)")
}

// suspended user は除外される (isSuspended = FALSE)。
func TestUsers_SuspendedExcluded(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_sus", "htususp", []string{"golangx"}, nil, true, 1, now)
	ids := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"golangx","sort":"+follower"}`))
	assert.NotContains(t, ids, "u_htu_sus")
}

// +follower DESC / -follower ASC の sort 順を確認。
func TestUsers_SortFollower(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_lo", "htulo", []string{"sortt"}, nil, false, 1, now)
	seedTagUser(t, "u_htu_hi", "htuhi", []string{"sortt"}, nil, false, 100, now)

	desc := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"sortt","sort":"+follower"}`))
	require.Equal(t, []string{"u_htu_hi", "u_htu_lo"}, desc)
	asc := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"sortt","sort":"-follower"}`))
	require.Equal(t, []string{"u_htu_lo", "u_htu_hi"}, asc)
}

// state=alive は updatedAt が 5日以内の user のみ返す。
func TestUsers_StateAlive(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	seedTagUser(t, "u_htu_new", "htunew", []string{"alivet"}, nil, false, 1, now)
	seedTagUser(t, "u_htu_old", "htuold", []string{"alivet"}, nil, false, 1, old)

	all := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"alivet","sort":"+follower"}`))
	assert.ElementsMatch(t, []string{"u_htu_new", "u_htu_old"}, all)
	alive := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"alivet","sort":"+follower","state":"alive"}`))
	assert.Equal(t, []string{"u_htu_new"}, alive)
}

// origin=local は host IS NULL のみ、remote は host IS NOT NULL のみ、
// combined は両方返す。
func TestUsers_Origin(t *testing.T) {
	now := time.Now()
	host := "remote.example"
	seedTagUser(t, "u_htu_loc", "htuloc", []string{"origint"}, nil, false, 1, now)
	seedTagUser(t, "u_htu_rem", "hturem", []string{"origint"}, &host, false, 1, now)

	local := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"origint","sort":"+follower","origin":"local"}`))
	assert.Equal(t, []string{"u_htu_loc"}, local)
	remote := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"origint","sort":"+follower","origin":"remote"}`))
	assert.Equal(t, []string{"u_htu_rem"}, remote)
	combined := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"origint","sort":"+follower","origin":"combined"}`))
	assert.ElementsMatch(t, []string{"u_htu_loc", "u_htu_rem"}, combined)
}

// limit + offset が効く (offset で 2 ページ目を取得)。
func TestUsers_LimitOffset(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_p1", "htup1", []string{"paget"}, nil, false, 3, now)
	seedTagUser(t, "u_htu_p2", "htup2", []string{"paget"}, nil, false, 2, now)
	seedTagUser(t, "u_htu_p3", "htup3", []string{"paget"}, nil, false, 1, now)

	page1 := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"paget","sort":"+follower","limit":2}`))
	require.Equal(t, []string{"u_htu_p1", "u_htu_p2"}, page1)
	page2 := decodeUserIDs(t, doPost(newHandler().Users, `{"tag":"paget","sort":"+follower","limit":2,"offset":2}`))
	require.Equal(t, []string{"u_htu_p3"}, page2)
}

func TestUsers_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Users, `{}`).Code)
}

// TestUsers_SortRequired: upstream Misskey TS は paramDef で sort も
// required にしている。tag だけだと 400 (#925)。
func TestUsers_SortRequired(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Users, `{"tag":"test"}`).Code)
}

// TestUsers_SortEnum: upstream paramDef は sort を 6 値の enum 限定。
// 未定義 sort で 400 を返すこと (#925 review nit)。
func TestUsers_SortEnum(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Users, `{"tag":"test","sort":"bogus"}`).Code)
}

// state / origin も upstream paramDef enum 限定。未定義値で 400。
func TestUsers_StateEnum(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Users, `{"tag":"test","sort":"+follower","state":"bogus"}`).Code)
}
func TestUsers_OriginEnum(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Users, `{"tag":"test","sort":"+follower","origin":"bogus"}`).Code)
}
func TestUsers_DBError(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError, doPost(brokenHandler().Users, `{"tag":"test","sort":"+follower"}`).Code)
}

// **列に入らない tag / query は 500 にしない (#3025)。** ここは実 DB を使うので、
// guard が無ければ SELECT が SQLSTATE 08P01 で落ち、handler が 500 (Search /
// Users) または「DB 障害を not-found に丸めた」400 (Show) を返す。
//
// **JSON では NUL をエスケープで送る。** 生バイトを body に載せると JSON として
// 不正になり、`Bind` の 400 で guard まで届かない (= 何も検証しないまま緑になる)。
//
// **`hashtags/show` はここに入れていない。** あちらは `err != nil` を丸ごと
// `NO_SUCH_HASHTAG` (400) に潰す既存実装なので、guard の有無で**外から見える
// 応答が変わらない** (実測で、guard を外しても同じ 400 が返る)。空虚なテストに
// なるので置かない。guard 自体は、確実に落ちるクエリを投げないため、および
// DB 障害が not-found に化ける経路 (#2792) にこの入力を流し込まないために残す。
func TestUnstorableTagIsNotAServerError(t *testing.T) {
	h := newHandler()

	t.Run("search", func(t *testing.T) {
		rec := doPost(h.Search, `{"query":"a\u0000b"}`)
		require.Equal(t, http.StatusOK, rec.Code, "検索語を LIKE に載せている: %s", rec.Body.String())
		assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
	})
	t.Run("users", func(t *testing.T) {
		rec := doPost(h.Users, `{"tag":"a\u0000b","sort":"+follower"}`)
		require.Equal(t, http.StatusOK, rec.Code, "tag を containment query に載せている: %s", rec.Body.String())
		assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
	})
}
