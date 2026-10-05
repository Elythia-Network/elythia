package emojiapplications_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/api/emojiapplications"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

type stubApps struct {
	// #2960 の関連履歴。
	related       []repository.RelatedApplication
	relatedCounts repository.RelatedCounts
	relatedErr    error
	rows          []model.EmojiApplication
	byID          map[string]*model.EmojiApplication
	err           error
	createErr     error
	created       *model.EmojiApplication
	// lastUser records who the list was scoped to.
	lastUser string
	// quotaErr is returned from CreateWithQuota (#2958).
	quotaErr error
}

func (s *stubApps) Create(a *model.EmojiApplication) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.created = a
	return nil
}
func (s *stubApps) FindRelated(*model.EmojiApplication, int, string) ([]repository.RelatedApplication, error) {
	return s.related, s.relatedErr
}
func (s *stubApps) CountRelated(*model.EmojiApplication) (repository.RelatedCounts, error) {
	return s.relatedCounts, s.relatedErr
}

func (s *stubApps) ListByUserFiltered(string, string, string, int, string) ([]model.EmojiApplication, error) {
	return nil, nil
}
func (s *stubApps) CountByUserStatus(string) (repository.StatusCounts, error) {
	return repository.StatusCounts{}, nil
}
func (s *stubApps) QuotaUsage(string, repository.QuotaLimits, time.Time) (repository.QuotaUsage, error) {
	return repository.QuotaUsage{}, nil
}

func (s *stubApps) CreateWithQuota(a *model.EmojiApplication, _ repository.QuotaLimits) error {
	if s.quotaErr != nil {
		return s.quotaErr
	}
	return s.Create(a)
}
func (s *stubApps) FindByID(id string) (*model.EmojiApplication, error) {
	if a, ok := s.byID[id]; ok {
		// **コピーを返す。** 同じポインタだと service の書き換えが保存済みの
		// 行にも反映され、条件付き UPDATE の検証が成立しない。
		cp := *a
		return &cp, nil
	}
	return nil, gorm.ErrRecordNotFound
}
func (s *stubApps) List(string, int, string) ([]model.EmojiApplication, error) { return nil, nil }
func (s *stubApps) ListByUser(userID string, _ int, _ string) ([]model.EmojiApplication, error) {
	s.lastUser = userID
	return s.rows, s.err
}
func (s *stubApps) CountPending() (int64, error) { return 0, nil }
func (s *stubApps) UpdateIfPending(a *model.EmojiApplication) (bool, error) {
	cur, ok := s.byID[a.ID]
	if !ok || cur.Status != model.EmojiApplicationPending {
		return false, nil
	}
	s.byID[a.ID] = a
	return true, nil
}

func doPost(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

var alice = &model.User{ID: "u1"}

// **一覧は必ず自分の分に絞る。** userId をリクエストから取る形にすると、
// 他人の申請 (却下理由を含む) が読めてしまう。
func TestListMineScopesToCaller(t *testing.T) {
	apps := &stubApps{rows: []model.EmojiApplication{{ID: "a1", Name: "sushi", Status: "pending"}}}
	h := emojiapplications.NewHandler(nil, apps, nil)

	rec := doPost(h.ListMine, `{"userId":"someone-else"}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "u1", apps.lastUser, "呼び出し元以外の申請を引いている")

	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1)
	require.Equal(t, "sushi", body[0]["name"])
}

func TestListMineRequiresAuth(t *testing.T) {
	h := emojiapplications.NewHandler(nil, &stubApps{}, nil)
	rec := doPost(h.ListMine, `{}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// **審査したモデレーターは出さない。** 誰が押したかは監査用で、申請者に
// 見せると個人への抗議に繋がりやすい。
func TestPackOmitsModerator(t *testing.T) {
	mod := "mod1"
	reason := "潰れて読めません"
	apps := &stubApps{rows: []model.EmojiApplication{{
		ID: "a1", Name: "kusa", Status: "rejected",
		ProcessedByID: &mod, RejectReason: &reason,
	}}}
	h := emojiapplications.NewHandler(nil, apps, nil)

	rec := doPost(h.ListMine, `{}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "mod1", "審査したモデレーターが漏れている")
	// 却下理由は出す。直して出し直すための唯一の手がかり。
	require.Contains(t, rec.Body.String(), reason)
}

func TestListMineSurfacesFailure(t *testing.T) {
	apps := &stubApps{err: gorm.ErrInvalidDB}
	h := emojiapplications.NewHandler(nil, apps, nil)
	rec := doPost(h.ListMine, `{}`, alice)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// wiredHandler builds a handler with a real service so the auth and validation
// branches are actually reachable.
//
// **svc を nil にしたまま「認証を見た」と書かない。** handler は svc == nil を
// 先に見るので、nil のままでは認証も検証も一度も通らない — 名前だけ合っている
// 空のテストになる。
func wiredHandler(apps *stubApps) *emojiapplications.Handler {
	svc := emojiapplication.NewService(apps, &stubEmojiLookup{}, ownedFile(), &stubIDGen{}, nil, nil)
	return emojiapplications.NewHandler(svc, apps, nil)
}

type stubEmojiLookup struct{}

func (stubEmojiLookup) FindByNameAndHost(string, *string) (*model.Emoji, error) {
	return nil, gorm.ErrRecordNotFound
}

// ownedFile resolves a drive file owned by alice.
func ownedFile() *stubFiles {
	owner := alice.ID
	return &stubFiles{file: &model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png"}}
}

type stubIDGen struct{}

func (stubIDGen) Generate(time.Time) string { return "new1" }

func TestCreateRequiresAuth(t *testing.T) {
	rec := doPost(wiredHandler(&stubApps{}).Create, `{"name":"sushi","license":"自作","fileId":"f1"}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestCreateValidatesName(t *testing.T) {
	rec := doPost(wiredHandler(&stubApps{}).Create, `{"name":"su-shi","license":"自作","fileId":"f1"}`, alice)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "INVALID_EMOJI_NAME")
}

func TestCreateRequiresLicense(t *testing.T) {
	rec := doPost(wiredHandler(&stubApps{}).Create, `{"name":"sushi","fileId":"f1"}`, alice)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "LICENSE_REQUIRED")
}

// **申請者は呼び出し元で決まる (レビュー H7)。** ボディの userId を見る形に
// 変えると、他人名義の申請を作られて被害者の一覧に出る。stubApps が Create の
// 引数を捨てていたので、以前はこの変異が全テスト緑で通っていた。
func TestCreateRecordsCallerAsApplicant(t *testing.T) {
	apps := &stubApps{}
	rec := doPost(wiredHandler(apps).Create,
		`{"name":"sushi","license":"自作","fileId":"f1","userId":"victim"}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, apps.created, "申請が保存されていない")
	require.Equal(t, alice.ID, apps.created.UserID,
		"ボディの userId が使われている。他人名義の申請を作られる")
}

func TestCreateSucceeds(t *testing.T) {
	rec := doPost(wiredHandler(&stubApps{}).Create,
		`{"name":"sushi","license":"自作","fileId":"f1","category":"たべもの"}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "sushi", body["name"])
	require.Equal(t, "pending", body["status"])
}

func TestCancelRequiresAuth(t *testing.T) {
	rec := doPost(wiredHandler(&stubApps{}).Cancel, `{"applicationId":"a1"}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestCancelRequiresID(t *testing.T) {
	rec := doPost(wiredHandler(&stubApps{}).Cancel, `{}`, alice)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// **他人の申請は「無い」と答える。** 403 を返すと、存在する申請 ID を
// 総当たりで数えられる。
func TestCancelHidesOthersApplications(t *testing.T) {
	apps := &stubApps{byID: map[string]*model.EmojiApplication{
		"a1": {ID: "a1", UserID: "someone-else", Status: model.EmojiApplicationPending},
	}}
	rec := doPost(wiredHandler(apps).Cancel, `{"applicationId":"a1"}`, alice)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "NO_SUCH_APPLICATION")
}

// stubFiles resolves drive files for the preview URL.
type stubFiles struct {
	file *model.DriveFile
	err  error
}

func (s *stubFiles) FindByID(string) (*model.DriveFile, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.file == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return s.file, nil
}

// **残りの error を種別ごとに落とす。** すべて 400 INVALID_PARAM に丸めると、
// 利用者には「何か間違っている」としか伝わらない。
func TestCreateErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*stubApps, *stubEmojiLookupWithHit)
		body   string
		code   int
		expect string
	}{
		{"同名の絵文字がある", func(_ *stubApps, e *stubEmojiLookupWithHit) { e.hit = true },
			`{"name":"sushi","license":"自作","fileId":"f1"}`, http.StatusBadRequest, "DUPLICATE_NAME"},
		{"審査中の重複", func(a *stubApps, _ *stubEmojiLookupWithHit) {
			a.createErr = repository.ErrEmojiApplicationDuplicatePending
		},
			`{"name":"sushi","license":"自作","fileId":"f1"}`, http.StatusBadRequest, "ALREADY_REQUESTED"},
		{"画像が無い", func(*stubApps, *stubEmojiLookupWithHit) {},
			`{"name":"sushi","license":"自作"}`, http.StatusBadRequest, "NO_SUCH_FILE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apps := &stubApps{}
			lookup := &stubEmojiLookupWithHit{}
			tc.setup(apps, lookup)
			svc := emojiapplication.NewService(apps, lookup, ownedFile(), &stubIDGen{}, nil, nil)
			h := emojiapplications.NewHandler(svc, apps, nil)

			rec := doPost(h.Create, tc.body, alice)
			require.Equal(t, tc.code, rec.Code)
			require.Contains(t, rec.Body.String(), tc.expect)
		})
	}
}

type stubEmojiLookupWithHit struct{ hit bool }

func (s *stubEmojiLookupWithHit) FindByNameAndHost(string, *string) (*model.Emoji, error) {
	if s.hit {
		return &model.Emoji{ID: "e1"}, nil
	}
	return nil, gorm.ErrRecordNotFound
}

// DB 障害は 500 のまま残す (#2792)。not-found に丸めない。
func TestCreateSurfacesInternalError(t *testing.T) {
	apps := &stubApps{createErr: gorm.ErrInvalidDB}
	svc := emojiapplication.NewService(apps, &stubEmojiLookup{}, ownedFile(), &stubIDGen{}, nil, nil)
	rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create,
		`{"name":"sushi","license":"自作","fileId":"f1"}`, alice)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCancelSucceeds(t *testing.T) {
	apps := &stubApps{byID: map[string]*model.EmojiApplication{
		"a1": {ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending},
	}}
	rec := doPost(wiredHandler(apps).Cancel, `{"applicationId":"a1"}`, alice)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestCancelAlreadyProcessed(t *testing.T) {
	apps := &stubApps{byID: map[string]*model.EmojiApplication{
		"a1": {ID: "a1", UserID: "u1", Status: model.EmojiApplicationApproved},
	}}
	rec := doPost(wiredHandler(apps).Cancel, `{"applicationId":"a1"}`, alice)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "ALREADY_PROCESSED")
}

// **画像が消えていても一覧は出す。** url が空になるだけで、却下理由などの
// 経緯は読めるべき。
func TestPreviewURLTolerable(t *testing.T) {
	fileID := "f1"
	apps := &stubApps{rows: []model.EmojiApplication{{ID: "a1", Name: "sushi", FileID: &fileID}}}

	t.Run("drive にある", func(t *testing.T) {
		h := emojiapplications.NewHandler(nil, apps, &stubFiles{file: &model.DriveFile{URL: "https://x/f1.png"}})
		rec := doPost(h.ListMine, `{}`, alice)
		require.Contains(t, rec.Body.String(), "https://x/f1.png")
	})

	t.Run("drive から消えている", func(t *testing.T) {
		h := emojiapplications.NewHandler(nil, apps, &stubFiles{})
		rec := doPost(h.ListMine, `{}`, alice)
		require.Equal(t, http.StatusOK, rec.Code, "画像が無いだけで一覧が落ちている")
		var body []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		// #2989: 空文字ではなく理由を返す。「削除された」と「確認できなかった」を
		// クライアントが区別できるようにするため。
		preview := body[0]["preview"].(map[string]any)
		require.Equal(t, "", preview["url"])
		require.Equal(t, "sourceGone", preview["state"])
	})
}

// **新しく到達可能になった error も種別ごとに落とす。** H2 / M8 で
// create から TOO_LONG / UNSUPPORTED_FILE_TYPE / NO_SUCH_FILE が返るように
// なったので、500 に潰していないことを固定する。
func TestCreateErrorMappingForFileAndLength(t *testing.T) {
	owner := alice.ID
	cases := []struct {
		name   string
		file   *model.DriveFile
		body   string
		expect string
	}{
		{"長すぎるライセンス",
			&model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png"},
			`{"name":"sushi","fileId":"f1","license":"` + strings.Repeat("x", 1100) + `"}`,
			"TOO_LONG"},
		// NUL も同じ TOO_LONG に落ちる (#3022)。長さと NUL を 1 つの述語で
		// 見ているので、message は両方を説明する形にしてある。
		{"ライセンスに NUL",
			&model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png"},
			`{"name":"sushi","fileId":"f1","license":"a\u0000b"}`,
			"TOO_LONG"},
		{"画像でない",
			&model.DriveFile{ID: "f1", UserID: &owner, Type: "video/mp4"},
			`{"name":"sushi","fileId":"f1","license":"自作"}`,
			"UNSUPPORTED_FILE_TYPE"},
		{"他人のファイル",
			&model.DriveFile{ID: "f1", UserID: func() *string { s := "someone"; return &s }(), Type: "image/png"},
			`{"name":"sushi","fileId":"f1","license":"自作"}`,
			"NO_SUCH_FILE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apps := &stubApps{}
			svc := emojiapplication.NewService(apps, &stubEmojiLookup{},
				&stubFiles{file: tc.file}, &stubIDGen{}, nil, nil)
			rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create, tc.body, alice)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), tc.expect)
			if tc.expect == "TOO_LONG" {
				// **message が長さだけを言わない。** NUL もここへ来るので、
				// 「短くすれば通る」と読める文面だと直しようが無くなる。
				require.Contains(t, rec.Body.String(), "invalid character")
			}
			require.Nil(t, apps.created, "検証を通さずに申請が保存されている")
		})
	}
}

// stubRemoteLookup answers host-qualified lookups so remote requests can pass.
type stubRemoteLookup struct{ known bool }

func (s *stubRemoteLookup) FindByNameAndHost(_ string, host *string) (*model.Emoji, error) {
	if host != nil {
		if s.known {
			return &model.Emoji{ID: "e-remote"}, nil
		}
		return nil, gorm.ErrRecordNotFound
	}
	return nil, gorm.ErrRecordNotFound
}

// **リモートのインポート申請も error を種別ごとに落とす (#2935)。**
func TestCreateRemoteErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		known  bool
		body   string
		expect string
	}{
		{"知らないリモート絵文字", false,
			`{"kind":"remote","name":"sushi","license":"x","remoteHost":"example.com","remoteName":"s"}`,
			"NO_SUCH_EMOJI"},
		{"host が無い", true,
			`{"kind":"remote","name":"sushi","license":"x","remoteName":"s"}`,
			"INVALID_PARAM"},
		{"未知の kind", true,
			`{"kind":"whatever","name":"sushi","license":"x"}`,
			"INVALID_PARAM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apps := &stubApps{}
			svc := emojiapplication.NewService(apps, &stubRemoteLookup{known: tc.known},
				ownedFile(), &stubIDGen{}, nil, nil)
			rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create, tc.body, alice)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), tc.expect)
			require.Nil(t, apps.created, "検証を通さずに申請が保存されている")
		})
	}
}

// リモートの申請が成功すると、取り込み元が応答に出ること。
func TestCreateRemoteSucceeds(t *testing.T) {
	apps := &stubApps{}
	svc := emojiapplication.NewService(apps, &stubRemoteLookup{known: true},
		ownedFile(), &stubIDGen{}, nil, nil)
	rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create,
		`{"kind":"remote","name":"sushi","license":"x","remoteHost":"example.com","remoteName":"sushi_remote"}`, alice)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "remote", body["kind"])
	require.Equal(t, "example.com", body["remoteHost"])
	require.Equal(t, "sushi_remote", body["remoteName"])
}

// **上限超過は 429 で返し、いつ空くかも返す (#2958)。** 400 に倒すと利用者は
// 入力を直そうとして無駄に試行する。
func TestCreateQuotaExceededReturns429(t *testing.T) {
	// **now からの差と、ミリ秒未満の端数を、どちらも決定的にする。**
	// `Truncate(time.Millisecond)` だけだと RFC3339Nano との差が出るのが
	// 実測 10% しかなく書式の検査が空振りし、秒の端数を落とすと今度は
	// `Ceil` と `Floor` が区別できない秒に落ちる。
	// - 端数 456789ns → `ISOMillis` は切り捨てて `.xxx`、RFC3339Nano とは必ず違う
	// - now との差 5400.5 秒 → `Ceil` は 5401、`Floor` は 5400 で必ず割れる
	retryAt := time.Now().Add(90*time.Minute + 500*time.Millisecond).UTC().
		Truncate(time.Millisecond).Add(456789 * time.Nanosecond)
	apps := &stubApps{quotaErr: &repository.QuotaExceededError{
		Window:  repository.QuotaWindow{Name: "day", Duration: 24 * time.Hour, Max: 3},
		Used:    4,
		RetryAt: retryAt,
	}}
	// **policy provider は配線しない。** stub が windows を無視するので、
	// 配線しても何も検証していない。ここで見るのは HTTP の形だけ。
	svc := emojiapplication.NewService(apps, &stubEmojiLookup{}, ownedFile(), &stubIDGen{}, nil, nil)
	rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create,
		`{"name":"sushi","license":"自作","fileId":"f1"}`, alice)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
			Info struct {
				Period  string `json:"period"`
				Used    int    `json:"used"`
				Limit   int    `json:"limit"`
				RetryAt string `json:"retryAt"`
			} `json:"info"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "EMOJI_APPLICATION_QUOTA_EXCEEDED", body.Error.Code)
	require.NotEmpty(t, body.Error.ID)
	require.Equal(t, "day", body.Error.Info.Period)
	require.Equal(t, 4, body.Error.Info.Used)
	require.Equal(t, 3, body.Error.Info.Limit)
	// ミリ秒で丸めた形 (`.123Z`) であること。RFC3339Nano だと `.123456789Z`。
	require.Equal(t, retryAt.Format("2006-01-02T15:04:05.000Z"), body.Error.Info.RetryAt)
	require.NotEqual(t, retryAt.Format(time.RFC3339Nano), body.Error.Info.RetryAt,
		"ナノ秒まで出ている。misskey-js の他の時刻はミリ秒")

	// **Retry-After も付ける。** 機械的に再試行するクライアントが 429 を
	// 無視して叩き続けるのを防ぐ唯一の手がかり。
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	require.NoError(t, err)
	// 5400.5 秒先なので切り上げれば 5401。**切り捨てると 5400** になり、
	// その秒に叩いてもまだ窓の中にいる。
	require.Equal(t, 5401, secs)
	// 申請自体は作られていないこと。
	require.Nil(t, apps.created)
}

// 既に窓を出ている (RetryAt が過去) ときに負の Retry-After を返さない。
// 負値はクライアントによっては解釈に失敗する。
func TestCreateQuotaExceededClampsRetryAfter(t *testing.T) {
	apps := &stubApps{quotaErr: &repository.QuotaExceededError{
		Window:  repository.QuotaWindow{Name: "month", Duration: 30 * 24 * time.Hour, Max: 1},
		Used:    1,
		RetryAt: time.Now().Add(-time.Hour),
	}}
	svc := emojiapplication.NewService(apps, &stubEmojiLookup{}, ownedFile(), &stubIDGen{}, nil, nil)
	rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create,
		`{"name":"sushi","license":"自作","fileId":"f1"}`, alice)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "0", rec.Header().Get("Retry-After"))
	// どの期間で弾かれたかは窓から取ること (決め打ちにしない)。
	require.Contains(t, rec.Body.String(), `"period":"month"`)
}

// **審査待ちの上限は 429 にしない (#2977)。** いつ空くかを返せないので
// `Retry-After` を付けられず、レート制限と同じ形にすると「待てば通る」と
// 誤解させる。実際に空くのはモデレーターが処理したときか、取り下げたとき。
func TestCreatePendingLimitExceededReturns400(t *testing.T) {
	apps := &stubApps{quotaErr: &repository.PendingLimitExceededError{Used: 5, Limit: 3}}
	svc := emojiapplication.NewService(apps, &stubEmojiLookup{}, ownedFile(), &stubIDGen{}, nil, nil)
	rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create,
		`{"name":"sushi","license":"自作","fileId":"f1"}`, alice)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"), "待てば通ると誤解させる")

	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
			Info struct {
				Used    int    `json:"used"`
				Limit   int    `json:"limit"`
				RetryAt string `json:"retryAt"`
			} `json:"info"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "EMOJI_APPLICATION_PENDING_LIMIT_EXCEEDED", body.Error.Code)
	require.NotEmpty(t, body.Error.ID)
	require.Equal(t, 5, body.Error.Info.Used)
	require.Equal(t, 3, body.Error.Info.Limit)
	require.Empty(t, body.Error.Info.RetryAt, "予告できない時刻を出している")
	// 期間上限とコードが衝突しないこと (どちらで弾かれたかを区別できる)。
	require.NotContains(t, rec.Body.String(), "EMOJI_APPLICATION_QUOTA_EXCEEDED")
	require.Nil(t, apps.created)
}

// **時刻が無ければ `retryAt` も `Retry-After` も出さないこと (#2977)。**
// 審査待ちの上限も同時に満杯だと、期間が空いてもまだ通らない。予告できない
// 時刻を広告すると、利用者はその時刻に叩いて別のエラーを受け取る。
func TestCreateQuotaExceededWithoutRetryAtOmitsTime(t *testing.T) {
	apps := &stubApps{quotaErr: &repository.QuotaExceededError{
		Window: repository.QuotaWindow{Name: "day", Duration: 24 * time.Hour, Max: 3},
		Used:   4,
		// RetryAt はゼロ値 (審査待ちも満杯)。
	}}
	svc := emojiapplication.NewService(apps, &stubEmojiLookup{}, ownedFile(), &stubIDGen{}, nil, nil)
	rec := doPost(emojiapplications.NewHandler(svc, apps, nil).Create,
		`{"name":"sushi","license":"自作","fileId":"f1"}`, alice)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"), "予告できない時刻を出している")

	var body struct {
		Error struct {
			Code string         `json:"code"`
			Info map[string]any `json:"info"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "EMOJI_APPLICATION_QUOTA_EXCEEDED", body.Error.Code)
	require.NotContains(t, body.Error.Info, "retryAt")
	// 期間と件数は出す (どこで弾かれたかは伝わる)。
	require.Equal(t, "day", body.Error.Info["period"])
	require.EqualValues(t, 4, body.Error.Info["used"])
	require.EqualValues(t, 3, body.Error.Info["limit"])
}

// **列に入らないカーソルは 400 (#3025)。** `untilId` はそのまま `"id" < ?` の
// bind parameter に載るので、NUL を含むとクエリごと落ちて 500 になる。ここは
// `list-mine` = **一般の認証ユーザーが叩ける**経路。
func TestListMineRejectsUnstorableCursor(t *testing.T) {
	h := emojiapplications.NewHandler(nil, &stubApps{}, nil)
	rec := doPost(h.ListMine, `{"untilId":"a\u0000b"}`, alice)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"列に入らないカーソルを repository へ渡している (SELECT がそこで落ちる)")
	require.Contains(t, rec.Body.String(), "INVALID_PARAM")
}
