package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// stubEmojiReviewer records calls and returns canned results.
type stubEmojiReviewer struct {
	app     *model.EmojiApplication
	err     error
	lastID  string
	lastMod string
	lastWhy string

	// #2961 (ユーザーモデレーション画面の集計)。**err は review 側と分ける** —
	// 共有すると、片方の分岐を消してももう片方の err で同じ結果になる。
	summary       emojiapplication.UserSummary
	summaryErr    error
	lastSummaryID string

	// #2962
	resetBefore     emojiapplication.UserSummary
	resetRow        *model.EmojiApplicationQuotaReset
	resetErr        error
	resetCalls      int
	lastResetUserID string
	lastResetBy     string
	lastResetReason string
}

func (s *stubEmojiReviewer) UserSummary(userID string) (emojiapplication.UserSummary, error) {
	s.lastSummaryID = userID
	return s.summary, s.summaryErr
}

// #2962 (申請枠の手動リセット)。**err は他と分ける** — 共有すると、片方の
// 分岐を消してももう片方の err で同じ結果になる。
func (s *stubEmojiReviewer) ResetQuota(userID, moderatorID, reason string) (emojiapplication.UserSummary, *model.EmojiApplicationQuotaReset, error) {
	s.lastResetUserID, s.lastResetBy, s.lastResetReason = userID, moderatorID, reason
	s.resetCalls++
	if s.resetErr != nil {
		return emojiapplication.UserSummary{}, nil, s.resetErr
	}
	row := s.resetRow
	if row == nil {
		row = &model.EmojiApplicationQuotaReset{
			ID: "qr1", UserID: userID, ResetByID: moderatorID, Reason: reason,
			CreatedAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		}
	}
	return s.resetBefore, row, nil
}

func (s *stubEmojiReviewer) Approve(_ context.Context, id, moderatorID string) (*model.EmojiApplication, error) {
	s.lastID, s.lastMod = id, moderatorID
	return s.app, s.err
}

func (s *stubEmojiReviewer) Reject(_ context.Context, id, moderatorID, reason string) (*model.EmojiApplication, error) {
	s.lastID, s.lastMod, s.lastWhy = id, moderatorID, reason
	return s.app, s.err
}

func approvedApp() *model.EmojiApplication {
	emojiID := "e1"
	return &model.EmojiApplication{
		ID: "a1", UserID: "u1", Name: "sushi",
		Status: model.EmojiApplicationApproved, EmojiID: &emojiID,
	}
}

func newEmojiReviewerHandler(t *testing.T, r *stubEmojiReviewer) *apiadmin.Handler {
	t.Helper()
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationReviewer(r)
	return h
}

func TestEmojiApplicationApprove(t *testing.T) {
	rev := &stubEmojiReviewer{app: approvedApp()}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationApprove, `{"applicationId":"a1"}`, adminUser)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "a1", rev.lastID)
	// **押した人を記録する。** 監査で「誰が通したか」を辿れないと、
	// 権限の誤用に気付けない。
	require.Equal(t, "admin1", rev.lastMod)
}

func TestEmojiApplicationRejectPassesReason(t *testing.T) {
	rev := &stubEmojiReviewer{app: &model.EmojiApplication{ID: "a1", Status: model.EmojiApplicationRejected}}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationReject,
		`{"applicationId":"a1","reason":"潰れて読めません"}`, adminUser)

	require.Equal(t, http.StatusOK, rec.Code)
	// 理由は申請者にそのまま届く唯一の文面なので、落とさず渡す。
	require.Equal(t, "潰れて読めません", rev.lastWhy)
}

// **service のエラーを種別ごとに落とす。** すべて 500 にすると、
// モデレーターには「なぜか失敗した」としか見えない。
func TestEmojiApplicationReviewErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		body string
		// **id も見る。** 500 に落とすものが複数あるので、code だけだと
		// 取り違えても気付けない (id はエラーごとに一意)。
		id string
	}{
		{"存在しない", emojiapplication.ErrNotFound, http.StatusNotFound, "NO_SUCH_APPLICATION", "2b7e0c94-6d1a-4f3e-b5c8-0a9d3e7f1b44"},
		{"処理済み", emojiapplication.ErrNotPending, http.StatusBadRequest, "ALREADY_PROCESSED", "4e6f1a37-9c2b-4d80-a1f5-7b3c8e0d2a55"},
		{"同名あり", emojiapplication.ErrDuplicateName, http.StatusBadRequest, "DUPLICATE_NAME", "f7a3462c-4e6e-4069-8421-b9bd4f4c3975"},
		{"未対応の形式", emojiapplication.ErrUnsupportedFileType, http.StatusBadRequest, "UNSUPPORTED_FILE_TYPE", "f7599d96-8750-af68-1633-9575d625c1a7"},
		{"画像が消えた", emojiapplication.ErrFileGone, http.StatusBadRequest, "NO_SUCH_FILE", "fc46b5a4-6b92-4c33-ac66-b806659bb5cf"},
		// 以下 4 つは「承認だけが失敗する」経路。**却下すべき申請と、直せば
		// 通る申請を区別できる文面が要る**ので、種別ごとに分けている。
		{"大きすぎる", emojiapplication.ErrImageTooLarge, http.StatusBadRequest, "EMOJI_IMAGE_TOO_LARGE", "6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"},
		{"複製に失敗", emojiapplication.ErrImageCopyFailed, http.StatusInternalServerError, "INTERNAL_ERROR", "c2f7a3d1-58be-4e09-bb26-0d4a9e7f3c15"},
		{"リモート取得に失敗", emojiapplication.ErrRemoteFetchFailed, http.StatusInternalServerError, "INTERNAL_ERROR", "0a4e0b9e-2d7c-4d6f-8f6b-1f9c2e9b4d83"},
		{"リモート絵文字が消えた", emojiapplication.ErrNoSuchRemoteEmoji, http.StatusBadRequest, "NO_SUCH_EMOJI", "e2785b66-dca3-4087-9cac-b93c541cc425"},
		// 却下理由が列に入らない場合 (#3022)。**無いと 500 に落ちる**ので、
		// 申請側と同じ code / id で 400 を返す。
		{"列に入らない理由", emojiapplication.ErrTooLong, http.StatusBadRequest, "TOO_LONG", "8c5e2f60-1a4d-4b93-8e77-3d0f6a2b9c66"},
		// 保存先の URL が emoji の列に入らない (#3023)。**無いと 500 に落ちる。**
		{"列に入らない URL", emojiapplication.ErrImageURLTooLong, http.StatusBadRequest, "INVALID_PARAM", "3d81ceae-475f-4600-b2a8-2bc116157532"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev := &stubEmojiReviewer{err: tc.err}
			rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationApprove, `{"applicationId":"a1"}`, adminUser)
			require.Equal(t, tc.code, rec.Code)
			require.Contains(t, rec.Body.String(), tc.body)
			require.Contains(t, rec.Body.String(), tc.id)
		})
	}
}

func TestEmojiApplicationApproveRequiresID(t *testing.T) {
	rev := &stubEmojiReviewer{app: approvedApp()}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationApprove, `{}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rev.lastID, "id 無しで service が呼ばれた")
}

// **未配線なら 500。** reviewer が nil のまま 200 を返すと、押しても何も
// 起きないのに成功したように見える。
func TestEmojiApplicationApproveUnwired(t *testing.T) {
	rec := doPost((&apiadmin.Handler{}).EmojiApplicationApprove, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 監査に残す情報が出ていること。承認だけでなく却下も残す。
func TestEmojiApplicationResponseShape(t *testing.T) {
	rev := &stubEmojiReviewer{app: approvedApp()}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationApprove, `{"applicationId":"a1"}`, adminUser)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "a1", body["id"])
	require.Equal(t, "approved", body["status"])
	require.Equal(t, "e1", body["emojiId"])
}

// ---- CreateFromApplication (承認時の emoji 生成) ----
//
// **承認が検証を迂回する経路になっていないことを固定する。** admin/emoji/add は
// MIME allowlist / webpublic variant の優先 / 重複チェックを持つ。承認だけが
// それらを漏らすと、承認が「検証を迂回して絵文字を登録する方法」になる。
// レビューで、この関数のカバレッジが 0% で 4 通りの迂回が全部素通りすると
// 実測された。

func ownApplication() *model.EmojiApplication {
	fileID := "f1"
	cat := "たべもの"
	return &model.EmojiApplication{
		ID: "a1", UserID: "u1", Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: "sushi",
		Category: &cat, License: "自作", FileID: &fileID,
	}
}

// newEmojiRepoWith returns a mock emoji repository, optionally pre-seeded with
// a conflicting local emoji.
func newEmojiRepoWith(existingName string) *testutil.MockEmojiRepository {
	r := testutil.NewMockEmojiRepository()
	if existingName != "" {
		r.Emojis[existingName+"@"] = &model.Emoji{ID: "e-existing", Name: existingName}
	}
	return r
}

// newDriveRepoWith returns a mock drive repository holding the given file.
func newDriveRepoWith(f *model.DriveFile) *testutil.MockDriveFileRepository {
	r := testutil.NewMockDriveFileRepository()
	if f != nil {
		r.Files[f.ID] = f
	}
	return r
}

// pngFile is a drive file the emoji pipeline accepts, with a webpublic variant
// so the "生の URL を使っていないか" assertion has something to catch.
func pngFile() *model.DriveFile {
	webURL := "https://x/webpublic.webp"
	webType := "image/webp"
	owner := "u1"
	return &model.DriveFile{
		ID: "f1", URL: "https://x/original.png", Type: "image/png", UserID: &owner,
		WebpublicURL: &webURL, WebpublicType: &webType,
	}
}

func newCreatorHandler(t *testing.T, emojis *testutil.MockEmojiRepository, files *testutil.MockDriveFileRepository) *apiadmin.Handler {
	t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h := apiadmin.NewHandler(nil, nil, nil, nil, gen)
	h.SetEmojiRepo(emojis)
	h.SetDriveFileRepo(files)
	return h
}

// **MIME の allowlist を通ること。** 抜けると、承認が SVG 等を登録する経路になる。
func TestCreateFromApplicationChecksMIME(t *testing.T) {
	emojis := newEmojiRepoWith("")
	files := newDriveRepoWith(&model.DriveFile{
		ID: "f1", URL: "https://x/a.svg", Type: "image/svg+xml",
		UserID: func() *string { u := "u1"; return &u }(),
	})

	_, err := newCreatorHandler(t, emojis, files).CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrUnsupportedFileType)
	require.Empty(t, emojis.Emojis, "検証を通さずに emoji が作られている")
}

// **承認の直前にも重複を見ること。** 申請の受付時にも見ているが、審査を待つ間に
// 同じ名前が登録されうる。
func TestCreateFromApplicationRechecksDuplicate(t *testing.T) {
	emojis := newEmojiRepoWith("sushi")
	_, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrDuplicateName)
	require.Len(t, emojis.Emojis, 1, "重複を弾いたのに emoji が増えている")
}

// **webpublic variant を優先すること。** 生の URL / type を使うと、
// admin/emoji/add で登録した絵文字と配信物が食い違う。
func TestCreateFromApplicationPrefersWebpublic(t *testing.T) {
	emojis := newEmojiRepoWith("")
	emojiID, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), ownApplication())
	require.NoError(t, err)
	require.NotEmpty(t, emojiID)
	created := emojis.Emojis["sushi@"]
	require.NotNil(t, created, "emoji が作られていない")

	require.Equal(t, "https://x/original.png", created.OriginalURL)
	require.Equal(t, "https://x/webpublic.webp", created.PublicURL, "webpublic を優先していない")
	require.NotNil(t, created.Type)
	require.Equal(t, "image/webp", *created.Type)

	// 申請の内容がそのまま emoji へ写ること。
	require.Equal(t, "sushi", created.Name)
	require.Equal(t, "自作", *created.License)
	require.Equal(t, "たべもの", *created.Category)
}

// **申請から承認までの間にファイルが消えることがある。** 利用者が drive から
// 消せるので、500 ではなく「もう無い」と伝える。
func TestCreateFromApplicationHandlesMissingFile(t *testing.T) {
	_, err := newCreatorHandler(t, newEmojiRepoWith(""), newDriveRepoWith(nil)).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrFileGone)
}

func TestCreateFromApplicationRequiresFile(t *testing.T) {
	app := ownApplication()
	app.FileID = nil
	_, err := newCreatorHandler(t, newEmojiRepoWith(""), newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), app)
	require.ErrorIs(t, err, emojiapplication.ErrFileGone)
}

// **承認側でも所有者を見る (レビュー Low 3)。** 申請時の検証が入る前に作られた
// 行が残っている可能性があり、多層にしておけば「申請時の検証を落としたら
// 承認が素通りする」形にもならない。
func TestCreateFromApplicationChecksOwner(t *testing.T) {
	other := "someone-else"
	f := pngFile()
	f.UserID = &other
	emojis := newEmojiRepoWith("")

	_, err := newCreatorHandler(t, emojis, newDriveRepoWith(f)).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrFileGone)
	require.Empty(t, emojis.Emojis, "他人のファイルで emoji が作られている")
}

// **modlog に `info.emoji` が入ること (レビュー H6 / M2)。**
//
// upstream の `modlog.ModLog.vue` は `log.info.emoji.name` を無条件に読む。
// 欠けると Vue が render 例外を握って Comment ノードを返すので、**その 1 件が
// 監査ログから静かに消える**。コメントだけでは守れないのでテストで固定する。
func TestEmojiApplicationLogsEmojiForModLogUI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   func(*apiadmin.Handler) func(echo.Context) error
		app    *model.EmojiApplication
		decide string
	}{
		{"承認", func(h *apiadmin.Handler) func(echo.Context) error { return h.EmojiApplicationApprove },
			approvedApp(), "approved"},
		{"却下", func(h *apiadmin.Handler) func(echo.Context) error { return h.EmojiApplicationReject },
			&model.EmojiApplication{ID: "a1", UserID: "u1", Name: "kusa", Status: model.EmojiApplicationRejected},
			"rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEmojiReviewerHandler(t, &stubEmojiReviewer{app: tc.app})
			logs := attachModLog(t, h)

			rec := doPost(tc.call(h), `{"applicationId":"a1","reason":"x"}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)

			// **Log は fire-and-forget (goroutine)。** 同期で読むと空のことがある。
			require.Eventually(t, func() bool { return len(logs.Snapshot()) == 1 },
				time.Second, 5*time.Millisecond, "モデレーションログが残っていない")
			entries := logs.Snapshot()

			// Info は jsonb なので展開して読む。
			var info map[string]any
			require.NoError(t, json.Unmarshal(entries[0].Info, &info))

			emoji, ok := info["emoji"].(map[string]any)
			require.True(t, ok, "info.emoji が無い。管理画面の modlog がこの行を描けず静かに消える")
			require.Equal(t, tc.app.Name, emoji["name"], "info.emoji.name が申請の名前と違う")
			require.Equal(t, tc.decide, info["emojiApplicationDecided"])
			require.Equal(t, "a1", info["emojiApplicationId"])
		})
	}
}

// **負けた承認の emoji を実際に消すこと (レビュー M3)。**
//
// service 側は「負けたら呼ぶ」ことを固定しているが、呼ばれた先が no-op でも
// 気付かない。R5 が防ぎたかった「却下済みなのに絵文字が使える」が復活する。
func TestDeleteCreatedEmoji(t *testing.T) {
	emojis := newEmojiRepoWith("sushi")
	h := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile()))

	require.NoError(t, h.DeleteCreatedEmoji(context.Background(), emojiapplication.CreatedEmoji{EmojiID: "e-existing"}))
	require.Empty(t, emojis.Emojis, "emoji が消えていない")
}

// 既に無い emoji を指定しても失敗しない (二重に走っても壊れない)。
func TestDeleteCreatedEmojiIgnoresMissing(t *testing.T) {
	h := newCreatorHandler(t, newEmojiRepoWith(""), newDriveRepoWith(pngFile()))
	require.NoError(t, h.DeleteCreatedEmoji(context.Background(), emojiapplication.CreatedEmoji{EmojiID: "gone"}))
	require.NoError(t, h.DeleteCreatedEmoji(context.Background(), emojiapplication.CreatedEmoji{}))
}

// **重複の確認に失敗したことを隠さない (レビュー Low 1)。**
//
// DB 障害を「重複なし」(false) に潰すと、確認できていないのにモデレーターが
// 承認を押し、DUPLICATE_NAME で落ちる。null で「分からない」を出す。
func TestEmojiApplicationListMarksUnknownConflict(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(&stubAppsRepo{rows: []model.EmojiApplication{
		{ID: "a1", UserID: "u1", Name: "sushi", Status: model.EmojiApplicationPending},
	}})
	h.SetEmojiRepo(&failingEmojiRepo{})

	rec := doPost(h.EmojiApplicationList, `{"filter":"pending"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1)
	require.Contains(t, body[0], "nameConflict")
	require.Nil(t, body[0]["nameConflict"], "DB 障害が「重複なし」に潰されている")
}

// **既定の filter は審査待ち (レビュー Low 2)。** all にすると、審査タブを
// 開いたときに処理済みが混ざって審査待ちが埋もれる。
func TestEmojiApplicationListDefaultsToPending(t *testing.T) {
	repo := &stubAppsRepo{}
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(repo)

	doPost(h.EmojiApplicationList, `{}`, adminUser)
	require.Equal(t, "pending", repo.lastFilter)

	// limit のクランプ。際限なく受けると 1 リクエストで drive を大量に引く。
	doPost(h.EmojiApplicationList, `{"limit":500}`, adminUser)
	require.Equal(t, 30, repo.lastLimit, "limit がクランプされていない")
}

type stubAppsRepo struct {
	// #2960 の関連履歴。
	related       []repository.RelatedApplication
	relatedCounts repository.RelatedCounts
	// **count と list で別々に持つ。** 1 つの err を両方から返すと、
	// どちらの分岐を消しても「もう片方の err」で 500 になり、
	// 2 つのテストが揃って空虚になる (レビュー R2-H1 で実測)。
	countErr   error
	listErr    error
	findErr    error
	lastLimit2 int
	lastUntil  string
	rows       []model.EmojiApplication
	lastFilter string
	lastLimit  int

	// #2961 (ユーザーモデレーション画面の申請履歴)
	byUser          []model.EmojiApplication
	byUserErr       error
	userCounts      repository.StatusCounts
	userCountsErr   error
	usage           repository.QuotaUsage
	usageErr        error
	lastUserID      string
	lastStatus      string
	lastQuery       string
	lastUserLimit   int
	lastUserUntil   string
	lastCountUserID string
	lastUsageUserID string
	lastWindows     []repository.QuotaWindow
}

func (s *stubAppsRepo) Create(*model.EmojiApplication) error { return nil }
func (s *stubAppsRepo) CreateWithQuota(*model.EmojiApplication, repository.QuotaLimits) error {
	return nil
}
func (s *stubAppsRepo) FindRelated(_ *model.EmojiApplication, limit int, untilID string) ([]repository.RelatedApplication, error) {
	s.lastLimit2, s.lastUntil = limit, untilID
	return s.related, s.listErr
}
func (s *stubAppsRepo) CountRelated(*model.EmojiApplication) (repository.RelatedCounts, error) {
	return s.relatedCounts, s.countErr
}

// #2961 のユーザー単位の読み取り。**err はメソッドごとに分ける** — 1 つを
// 共有すると、片方の分岐を消しても「もう片方の err」で同じ結果になり、
// テストが揃って空虚になる (#2960 の R2-H1 で実測した形)。
func (s *stubAppsRepo) ListByUserFiltered(userID, status, query string, limit int, untilID string) ([]model.EmojiApplication, error) {
	s.lastUserID, s.lastStatus, s.lastQuery = userID, status, query
	s.lastUserLimit, s.lastUserUntil = limit, untilID
	return s.byUser, s.byUserErr
}

func (s *stubAppsRepo) CountByUserStatus(userID string) (repository.StatusCounts, error) {
	s.lastCountUserID = userID
	return s.userCounts, s.userCountsErr
}

func (s *stubAppsRepo) QuotaUsage(userID string, limits repository.QuotaLimits, _ time.Time) (repository.QuotaUsage, error) {
	s.lastUsageUserID, s.lastWindows = userID, limits.Windows
	return s.usage, s.usageErr
}

func (s *stubAppsRepo) FindByID(id string) (*model.EmojiApplication, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	for i := range s.rows {
		if s.rows[i].ID == id {
			return &s.rows[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (s *stubAppsRepo) List(filter string, limit int, _ string) ([]model.EmojiApplication, error) {
	s.lastFilter, s.lastLimit = filter, limit
	return s.rows, nil
}
func (s *stubAppsRepo) ListByUser(string, int, string) ([]model.EmojiApplication, error) {
	return nil, nil
}
func (s *stubAppsRepo) CountPending() (int64, error)                          { return 0, nil }
func (s *stubAppsRepo) UpdateIfPending(*model.EmojiApplication) (bool, error) { return true, nil }

// failingEmojiRepo fails every lookup so the "確認できなかった" branch runs.
type failingEmojiRepo struct{ testutil.MockEmojiRepository }

func (failingEmojiRepo) FindByNameAndHost(string, *string) (*model.Emoji, error) {
	return nil, gorm.ErrInvalidDB
}

// ---- createFromRemoteApplication (#2935) ----

func remoteApplication() *model.EmojiApplication {
	host, name := "example.com", "sushi_remote"
	cat := "たべもの"
	return &model.EmojiApplication{
		ID: "a2", UserID: "u1", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "sushi",
		Category: &cat, License: "リモートから取り込み",
		RemoteHost: &host, RemoteName: &name,
	}
}

// newRemoteEmojiRepo seeds a remote emoji so the import has a source.
func newRemoteEmojiRepo(t *testing.T) *testutil.MockEmojiRepository {
	t.Helper()
	r := testutil.NewMockEmojiRepository()
	host := "example.com"
	r.Emojis["sushi_remote@example.com"] = &model.Emoji{
		ID: "e-remote", Name: "sushi_remote", Host: &host,
		OriginalURL: "https://example.com/e.png", PublicURL: "https://example.com/e.png",
	}
	return r
}

// **`admin/emoji/copy` と同じ規則で列に収めること (#2726)。** 出どころは相手
// サーバーなので、そのまま入れると SQLSTATE 22001 でインポートが 500 になる。
func TestCreateFromRemoteApplicationFitsColumns(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	app := remoteApplication()
	long := strings.Repeat("あ", 200)
	app.Category = &long
	app.License = strings.Repeat("い", 2000)
	app.Aliases = model.StringArray{"ok", strings.Repeat("う", 200)}

	id, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), app)
	require.NoError(t, err)
	require.NotEmpty(t, id)

	created := emojis.Emojis["sushi@"]
	require.NotNil(t, created, "インポートされていない")
	// 本文は切る。
	require.LessOrEqual(t, len([]rune(*created.Category)), 128)
	require.LessOrEqual(t, len([]rune(*created.License)), 1024)
	// alias は長すぎる要素だけ落とす (切ると別の名前になる)。
	require.Equal(t, []string{"ok"}, []string(created.Aliases))
	// ローカルとして作られること。
	require.Nil(t, created.Host)
	require.Equal(t, "sushi", created.Name)
}

// **審査を待つ間に消えていたら「もう無い」と伝える。** リモート絵文字の行は
// キャッシュに近いので普通に起きる。500 にしない。
func TestCreateFromRemoteApplicationHandlesVanishedSource(t *testing.T) {
	_, err := newCreatorHandler(t, newEmojiRepoWith(""), newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), remoteApplication())
	require.ErrorIs(t, err, emojiapplication.ErrNoSuchRemoteEmoji)
}

// 承認の直前にも重複を見る (own と同じ)。
func TestCreateFromRemoteApplicationRechecksDuplicate(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	emojis.Emojis["sushi@"] = &model.Emoji{ID: "e-local", Name: "sushi"}

	_, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), remoteApplication())
	require.ErrorIs(t, err, emojiapplication.ErrDuplicateName)
}

// host / name を持たない remote 申請は取り込めない。
func TestCreateFromRemoteApplicationRequiresReference(t *testing.T) {
	app := remoteApplication()
	app.RemoteHost = nil

	_, err := newCreatorHandler(t, newRemoteEmojiRepo(t), newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), app)
	require.ErrorIs(t, err, emojiapplication.ErrNoSuchRemoteEmoji)
}

// **drive へ取り込むこと (レビュー M5-1)。** URL を引き継ぐだけだと、相手が
// 画像を消した瞬間に表示が壊れる。`copied.OriginalURL == df.URL` は
// DeleteOrphans の guard が依存する不変条件 (#670 / #722)。
func TestCreateFromRemoteApplicationStoresInDrive(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	h := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile()))
	h.SetEmojiImageFetcher(&stubFetcher{file: &model.DriveFile{
		ID: "df1", URL: "https://local/drive/df1.png", Type: "image/png",
	}})

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.NoError(t, err)

	created := emojis.Emojis["sushi@"]
	require.NotNil(t, created)
	require.Equal(t, "https://local/drive/df1.png", created.OriginalURL,
		"drive へ取り込んでいない。相手が画像を消すと表示が壊れる")
	require.NotEqual(t, "https://example.com/e.png", created.OriginalURL,
		"リモートの URL をそのまま引き継いでいる")
}

// **申請者が書いたときだけ license を上書きする (レビュー M1)。**
// リモート絵文字の 43% は AP 由来の本物の license を持っている。
func TestCreateFromRemoteApplicationKeepsSourceLicense(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	srcLicense := "CC BY 4.0 (remote author)"
	emojis.Emojis["sushi_remote@example.com"].License = &srcLicense

	app := remoteApplication()
	app.License = "" // 申請者は書かなかった

	_, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), app)
	require.NoError(t, err)

	created := emojis.Emojis["sushi@"]
	require.NotNil(t, created.License)
	require.Equal(t, srcLicense, *created.License, "相手の license が潰されている")
}

func TestCreateFromRemoteApplicationUsesApplicantLicense(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	srcLicense := "unknown"
	emojis.Emojis["sushi_remote@example.com"].License = &srcLicense

	_, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), remoteApplication())
	require.NoError(t, err)
	require.Equal(t, "リモートから取り込み", *emojis.Emojis["sushi@"].License)
}

// **審査画面にリモートの画像が出ること (レビュー H1)。** 出ないと
// `:name:@host` の文字列だけで承認を押すことになる。
func TestEmojiApplicationListShowsRemotePreview(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	h := &apiadmin.Handler{}
	h.SetEmojiRepo(emojis)
	h.SetEmojiApplicationRepo(&stubAppsRepo{rows: []model.EmojiApplication{*remoteApplication()}})

	rec := doPost(h.EmojiApplicationList, `{"filter":"pending"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1)
	require.Equal(t, "https://example.com/e.png", body[0]["url"], "リモートの画像が出ていない")
	require.Equal(t, "example.com", body[0]["remoteHost"])
	require.Equal(t, "sushi_remote", body[0]["remoteName"])
	require.Equal(t, false, body[0]["remoteGone"])
}

// 消えていたら承認前に分かること。
func TestEmojiApplicationListMarksRemoteGone(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiRepo(newEmojiRepoWith(""))
	h.SetEmojiApplicationRepo(&stubAppsRepo{rows: []model.EmojiApplication{*remoteApplication()}})

	rec := doPost(h.EmojiApplicationList, `{"filter":"pending"}`, adminUser)
	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, true, body[0]["remoteGone"], "消えていることが分からない")
}

// **NO_SUCH_EMOJI を落とさない (レビュー M3)。** この設計がいちばん想定している失敗。
func TestEmojiApplicationApproveMapsRemoteGone(t *testing.T) {
	rev := &stubEmojiReviewer{err: emojiapplication.ErrNoSuchRemoteEmoji}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationApprove, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "NO_SUCH_EMOJI")
}

type stubFetcher struct {
	file *model.DriveFile
	err  error

	// #2966 (承認時に system 所有へ複製する経路)
	copySrc       []*model.DriveFile
	copyNames     []string
	copySensitive []bool
	copyFile      *model.DriveFile
	copyErr       error
	deletedIDs    []string
	deleteErr     error
}

func (s *stubFetcher) FetchAndStore(_ context.Context, _ string, _ *model.User, _ string) (*model.DriveFile, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.file, nil
}

func (s *stubFetcher) CopyToSystemFile(_ context.Context, src *model.DriveFile, name string, sensitive bool) (*model.DriveFile, error) {
	s.copySrc = append(s.copySrc, src)
	s.copyNames = append(s.copyNames, name)
	s.copySensitive = append(s.copySensitive, sensitive)
	if s.copyErr != nil {
		return nil, s.copyErr
	}
	if s.copyFile != nil {
		return s.copyFile, nil
	}
	// 既定は「system 所有の別ファイルができた」形を返す。
	copied := *src
	copied.ID = src.ID + "-sys"
	copied.UserID = nil
	copied.UserHost = nil
	copied.URL = src.URL + "?sys"
	return &copied, nil
}

func (s *stubFetcher) DeleteSystemFile(_ context.Context, fileID string) error {
	s.deletedIDs = append(s.deletedIDs, fileID)
	return s.deleteErr
}

// **取り込んだ drive file の MIME を見ること (レビュー M7 / R2-M5)。**
// 相手が icon.url に非画像を置くと、承認でそれが絵文字として登録される。
func TestCreateFromRemoteApplicationChecksStoredMIME(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	h := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile()))
	h.SetEmojiImageFetcher(&stubFetcher{file: &model.DriveFile{
		ID: "df1", URL: "https://local/drive/df1.bin", Type: "application/octet-stream",
	}})

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.ErrorIs(t, err, emojiapplication.ErrUnsupportedFileType)
	require.Empty(t, emojis.Emojis["sushi@"], "非画像が絵文字として登録されている")
}

// **drive への取り込みが失敗したら種別のある error を返すこと (レビュー Low 2)。**
// 生の err だと汎用 500 になり、審査画面では「何か問題が」としか出ない。
func TestCreateFromRemoteApplicationMapsFetchFailure(t *testing.T) {
	h := newCreatorHandler(t, newRemoteEmojiRepo(t), newDriveRepoWith(pngFile()))
	h.SetEmojiImageFetcher(&stubFetcher{err: errors.New("timeout")})

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.ErrorIs(t, err, emojiapplication.ErrRemoteFetchFailed)
}

func TestEmojiApplicationApproveMapsFetchFailure(t *testing.T) {
	rev := &stubEmojiReviewer{err: emojiapplication.ErrRemoteFetchFailed}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationApprove, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "Failed to fetch emoji image")
}

// **取り込んだ drive file の type を採ること。**
//
// リモートの `src.Type` ではなく、drive へ保存した後の type が入る。
// `preferWebpublicType` は webpublic があればそちらを優先する。
//
// **fixture を非対称にする (レビュー M1)。** src と drive が同じ値だと、
// 「src を残した」と「drive を採った」が区別できず、代入を丸ごと消しても
// 通ってしまう (実測で素通りした)。
func TestCreateFromRemoteApplicationUsesStoredType(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	srcType := "image/gif"
	emojis.Emojis["sushi_remote@example.com"].Type = &srcType

	h := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile()))
	h.SetEmojiImageFetcher(&stubFetcher{file: &model.DriveFile{
		ID: "df1", URL: "https://local/drive/df1.png", Type: "image/png",
	}})

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.NoError(t, err)

	created := emojis.Emojis["sushi@"]
	require.NotNil(t, created.Type, "type が入っていない")
	require.Equal(t, "image/png", *created.Type,
		"drive へ保存した type ではなく src の値が使われている")
}

// webpublic があればそちらを優先すること (`EmojiCopy` と同じ)。
func TestCreateFromRemoteApplicationPrefersWebpublicType(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	webType := "image/webp"
	h := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile()))
	h.SetEmojiImageFetcher(&stubFetcher{file: &model.DriveFile{
		ID: "df1", URL: "https://local/drive/df1.png", Type: "image/png",
		WebpublicURL:  func() *string { u := "https://local/drive/df1.webp"; return &u }(),
		WebpublicType: &webType,
	}})

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.NoError(t, err)

	created := emojis.Emojis["sushi@"]
	require.Equal(t, "image/webp", *created.Type, "webpublic を優先していない")
	require.Equal(t, "https://local/drive/df1.webp", created.PublicURL)
}

// **isSensitive は指定があったときだけ上書きすること (レビュー Low 7)。**
func TestCreateFromRemoteApplicationKeepsSourceSensitive(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	emojis.Emojis["sushi_remote@example.com"].IsSensitive = true

	app := remoteApplication()
	app.IsSensitive = false // 申請者は指定しなかった

	_, err := newCreatorHandler(t, emojis, newDriveRepoWith(pngFile())).
		CreateFromApplication(context.Background(), app)
	require.NoError(t, err)
	require.True(t, emojis.Emojis["sushi@"].IsSensitive, "相手の sensitive が落ちている")
}

// **publicUrl が空なら originalUrl に落とすこと (レビュー R2-M5)。**
// AP 経由のリモート絵文字は publicUrl を持たないことがある。
func TestEmojiApplicationListFallsBackToOriginalURL(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	emojis.Emojis["sushi_remote@example.com"].PublicURL = ""

	h := &apiadmin.Handler{}
	h.SetEmojiRepo(emojis)
	h.SetEmojiApplicationRepo(&stubAppsRepo{rows: []model.EmojiApplication{*remoteApplication()}})

	rec := doPost(h.EmojiApplicationList, `{"filter":"pending"}`, adminUser)
	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "https://example.com/e.png", body[0]["url"], "originalUrl へ落ちていない")
}

// **リモートでも重複の警告が出ること (レビュー R2-H1)。**
// 早期 return を入れると nameConflict に到達しなくなり、承認を押してから
// DUPLICATE_NAME で落ちる (#2934 が塞いだ状態に戻る)。
func TestEmojiApplicationListReportsConflictForRemote(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	emojis.Emojis["sushi@"] = &model.Emoji{ID: "e-local", Name: "sushi"}

	h := &apiadmin.Handler{}
	h.SetEmojiRepo(emojis)
	h.SetEmojiApplicationRepo(&stubAppsRepo{rows: []model.EmojiApplication{*remoteApplication()}})

	rec := doPost(h.EmojiApplicationList, `{"filter":"pending"}`, adminUser)
	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body[0]["nameConflict"], "リモートで重複の警告が出ない")
	require.NotEqual(t, false, body[0]["nameConflict"])
}

// **DB 障害を「消えた」に丸めないこと (レビュー R2-H2)。**
// 丸めると障害中に承認が操作ごとブロックされる。
func TestEmojiApplicationListDistinguishesLookupFailure(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiRepo(&failingEmojiRepo{})
	h.SetEmojiApplicationRepo(&stubAppsRepo{rows: []model.EmojiApplication{*remoteApplication()}})

	rec := doPost(h.EmojiApplicationList, `{"filter":"pending"}`, adminUser)
	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Nil(t, body[0]["remoteGone"], "DB 障害が「消えた」に丸められている")
	require.Nil(t, body[0]["url"])
}

// **関連履歴は審査側の pack で返す (#2960)。** 申請者向けの pack はモデレーター
// と却下理由を落とすので、履歴として見る意味が無くなる。
func TestEmojiApplicationRelatedReturnsCountsAndItems(t *testing.T) {
	reason := "権利関係が不明"
	mod := "mod1"
	repo := &stubAppsRepo{
		rows: []model.EmojiApplication{
			{ID: "a1", UserID: "u1", Name: "sushi", Status: model.EmojiApplicationPending},
		},
		relatedCounts: repository.RelatedCounts{Total: 3, Approved: 1, Rejected: 2},
		related: []repository.RelatedApplication{{
			EmojiApplication: model.EmojiApplication{
				ID: "old1", UserID: "u2", Name: "sushi", Status: model.EmojiApplicationRejected,
				RejectReason: &reason, ProcessedByID: &mod,
			},
			MatchedName: true, MatchedHash: true,
		}},
	}
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(repo)

	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Counts repository.RelatedCounts `json:"counts"`
		Items  []map[string]any         `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, 3, body.Counts.Total)
	require.Equal(t, 2, body.Counts.Rejected)
	require.Len(t, body.Items, 1)
	require.Equal(t, "old1", body.Items[0]["id"])
	require.Equal(t, []any{"name", "fileHash"}, body.Items[0]["matchedBy"])
	// **却下理由を出す。** これが無いと「過去に却下された」しか分からず、
	// 同じ理由で再び却下すべきかを判断できない。
	require.Equal(t, reason, body.Items[0]["rejectReason"])
}

// **モデレーターは出さない。** 申請者向けの pack と同じく、誰が審査したかは
// 履歴にも載せない (#2934 で決めた扱いをここでも崩さない)。
func TestEmojiApplicationRelatedOmitsModerator(t *testing.T) {
	mod := "mod1"
	repo := &stubAppsRepo{
		rows: []model.EmojiApplication{{ID: "a1", Name: "sushi", Status: model.EmojiApplicationPending}},
		related: []repository.RelatedApplication{{
			EmojiApplication: model.EmojiApplication{
				ID: "old1", Name: "sushi", Status: model.EmojiApplicationRejected, ProcessedByID: &mod,
			},
			MatchedName: true,
		}},
	}
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(repo)

	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "mod1")
}

// applicationId は必須。無いと「全申請の履歴」を引くことになる。
func TestEmojiApplicationRelatedRequiresID(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(&stubAppsRepo{})
	rec := doPost(h.EmojiApplicationRelated, `{}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// 存在しない申請は 404。**DB 障害は 500 のまま残す (#2792)。**
func TestEmojiApplicationRelatedNotFoundVsFailure(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(&stubAppsRepo{})
	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"nope"}`, adminUser)
	require.Equal(t, http.StatusNotFound, rec.Code)

	h2 := &apiadmin.Handler{}
	h2.SetEmojiApplicationRepo(&stubAppsRepo{findErr: gorm.ErrInvalidDB})
	rec2 := doPost(h2.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec2.Code)
}

// limit の既定と上限。**上限を外すと 1 回で全履歴を引ける。**
func TestEmojiApplicationRelatedClampsLimit(t *testing.T) {
	repo := &stubAppsRepo{rows: []model.EmojiApplication{{ID: "a1", Name: "s", Status: model.EmojiApplicationPending}}}
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(repo)

	doPost(h.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, 10, repo.lastLimit2, "既定が 10 でない")

	doPost(h.EmojiApplicationRelated, `{"applicationId":"a1","limit":9999}`, adminUser)
	require.Equal(t, 10, repo.lastLimit2, "上限を超える limit がそのまま渡っている")

	doPost(h.EmojiApplicationRelated, `{"applicationId":"a1","limit":5,"untilId":"x"}`, adminUser)
	require.Equal(t, 5, repo.lastLimit2)
	require.Equal(t, "x", repo.lastUntil, "untilId が渡っていない")
}

// **件数の取得に失敗したら 500 にする (#2960)。** 握り潰して `total: 0` を
// 返すと、frontend は「関連する過去の申請は無い」と描画する — DB 障害を
// 「履歴なし」に化けさせると、実際には却下歴のある申請を承認してしまう。
func TestEmojiApplicationRelatedSurfacesCountFailure(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(&stubAppsRepo{
		rows:     []model.EmojiApplication{{ID: "a1", Name: "s", Status: model.EmojiApplicationPending}},
		countErr: gorm.ErrInvalidDB,
	})
	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), `"total":0`, "障害を「履歴なし」に化けさせている")
}

// 一覧の取得に失敗したときも同じ。件数だけ返して中身が空だと、
// 「3 件あるはずなのに何も出ない」という読めない状態になる。
func TestEmojiApplicationRelatedSurfacesListFailure(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(&stubAppsRepo{
		rows:          []model.EmojiApplication{{ID: "a1", Name: "s", Status: model.EmojiApplicationPending}},
		listErr:       gorm.ErrInvalidDB,
		relatedCounts: repository.RelatedCounts{Total: 3},
	})
	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **0 件でも `items` は配列で返す。** `null` を返すと frontend の
// `[...items, ...res.items]` が TypeError になる。
func TestEmojiApplicationRelatedReturnsEmptyArray(t *testing.T) {
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(&stubAppsRepo{
		rows: []model.EmojiApplication{{ID: "a1", Name: "s", Status: model.EmojiApplicationPending}},
	})
	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"a1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"items":[]`)
	require.NotContains(t, rec.Body.String(), `"items":null`)
}

// --- #2961 ユーザーモデレーション画面の申請履歴 ---

func userAppsHandler(rows []model.EmojiApplication) (*apiadmin.Handler, *stubAppsRepo) {
	repo := &stubAppsRepo{byUser: rows}
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(repo)
	return h, repo
}

// **userId が要る。** 省略を「全員ぶん」に倒すと、1 リクエストで全利用者の
// 却下理由が出る。
func TestEmojiApplicationListByUserRequiresUserID(t *testing.T) {
	h, _ := userAppsHandler(nil)
	rec := doPost(h.EmojiApplicationListByUser, `{}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

// **未知の status を全件に倒さない。** 絞ったつもりで全部出ると、
// モデレーターは「却下されたものだけ」を見ているつもりで判断する。
func TestEmojiApplicationListByUserRejectsUnknownStatus(t *testing.T) {
	h, repo := userAppsHandler(nil)
	rec := doPost(h.EmojiApplicationListByUser, `{"userId":"u1","status":"escalated"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, repo.lastUserID, "弾く前に repository を呼んでいる")
}

// 絞り込みと検索とページングがそのまま渡ること。
func TestEmojiApplicationListByUserPassesFilters(t *testing.T) {
	h, repo := userAppsHandler(nil)
	doPost(h.EmojiApplicationListByUser,
		`{"userId":"u1","status":"rejected","query":"sushi","limit":5,"untilId":"x"}`, adminUser)
	require.Equal(t, "u1", repo.lastUserID)
	require.Equal(t, "rejected", repo.lastStatus)
	require.Equal(t, "sushi", repo.lastQuery)
	require.Equal(t, 5, repo.lastUserLimit)
	require.Equal(t, "x", repo.lastUserUntil, "untilId が渡っていない")

	// 既定と、際限のない limit のクランプ。packer が 1 行あたり drive を引く。
	doPost(h.EmojiApplicationListByUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, 30, repo.lastUserLimit)
	doPost(h.EmojiApplicationListByUser, `{"userId":"u1","limit":500}`, adminUser)
	require.Equal(t, 30, repo.lastUserLimit, "limit がクランプされていない")
	// status 未指定は全件 (repository 側が "" を全件として扱う)。
	require.Equal(t, "", repo.lastStatus)

	// **"all" も通ること (レビュー M2)。** 画面の既定値がこれなので、
	// allowlist から落ちるとタブを開いた瞬間に 400 になり、履歴が丸ごと
	// 出なくなる。未指定 ("") しか検査していないと気付けない。
	rec := doPost(h.EmojiApplicationListByUser, `{"userId":"u1","status":"all"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, "画面の既定値 (all) が弾かれている")
	require.Equal(t, "all", repo.lastStatus)
}

// 未配線なら 500 (空の一覧を返して「申請なし」と描かない)。
func TestEmojiApplicationListByUserWithoutRepoIs500(t *testing.T) {
	h := &apiadmin.Handler{}
	rec := doPost(h.EmojiApplicationListByUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **障害を「履歴なし」に化けさせない。** 0 件として描くと、実際には申請が
// あるユーザーを何も無いものとして扱う。
func TestEmojiApplicationListByUserSurfacesFailure(t *testing.T) {
	h, repo := userAppsHandler(nil)
	repo.byUserErr = gorm.ErrInvalidDB
	rec := doPost(h.EmojiApplicationListByUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 0 件でも配列で返す (null だと frontend の追い読みが TypeError になる)。
func TestEmojiApplicationListByUserReturnsEmptyArray(t *testing.T) {
	h, _ := userAppsHandler(nil)
	rec := doPost(h.EmojiApplicationListByUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"items":[]`)
	require.NotContains(t, rec.Body.String(), `"items":null`)
}

// 審査画面と同じ pack を使う (却下理由が載る)。
func TestEmojiApplicationListByUserPacksForModerator(t *testing.T) {
	reason := "潰れて読めません"
	h, _ := userAppsHandler([]model.EmojiApplication{{
		ID: "a1", UserID: "u1", Name: "sushi",
		Status: model.EmojiApplicationRejected, RejectReason: &reason,
	}})
	rec := doPost(h.EmojiApplicationListByUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), reason, "却下理由が出ていない")
}

func TestEmojiApplicationUserSummaryRequiresUserID(t *testing.T) {
	rev := &stubEmojiReviewer{}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationUserSummary, `{}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rev.lastSummaryID, "弾く前に service を呼んでいる")
}

// **無制限を 0 で表さない。** 0 を「上限 0 件 = 出せない」と読める形にすると、
// 画面が「0 / 0」を出して枠が尽きているように見える。
func TestEmojiApplicationUserSummaryMarksUnlimited(t *testing.T) {
	at := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	rev := &stubEmojiReviewer{summary: emojiapplication.UserSummary{
		Counts:     repository.StatusCounts{Total: 3, Rejected: 2, Pending: 1},
		Pending:    2,
		MaxPending: 3,
		Windows: []emojiapplication.QuotaWindowUsage{
			{Period: "day", Used: 5, Limit: 5, RetryAt: at},
			{Period: "week", Used: 8, Limit: 20},
			{Period: "month", Used: 24, Limit: 0},
		},
	}}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationUserSummary, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "u1", rev.lastSummaryID)

	var body struct {
		Counts  repository.StatusCounts `json:"counts"`
		Pending struct {
			Used      int  `json:"used"`
			Limit     int  `json:"limit"`
			Unlimited bool `json:"unlimited"`
		} `json:"pending"`
		Windows []struct {
			Period    string `json:"period"`
			Used      int    `json:"used"`
			Limit     int    `json:"limit"`
			Unlimited bool   `json:"unlimited"`
			RetryAt   string `json:"retryAt"`
		} `json:"windows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, 3, body.Counts.Total)
	require.Len(t, body.Windows, 3)
	require.False(t, body.Windows[0].Unlimited)
	require.False(t, body.Windows[1].Unlimited)
	require.True(t, body.Windows[2].Unlimited, "上限 0 が無制限として出ていない")
	// **満杯の窓にだけ時刻。** 空きのある窓に出ると「今は出せない」と読める。
	require.NotEmpty(t, body.Windows[0].RetryAt, "満杯の窓に次回可能時刻が無い")
	// **審査待ちの上限も出ること (レビュー H1)。** 窓に空きがあってもこれが
	// 満杯なら申請は 400 で弾かれる。
	require.Equal(t, 2, body.Pending.Used, "審査待ちの件数が出ていない")
	require.Equal(t, 3, body.Pending.Limit, "審査待ちの上限が出ていない")
	require.False(t, body.Pending.Unlimited)
	require.Empty(t, body.Windows[1].RetryAt, "空きのある窓に次回可能時刻が出ている")
	require.Empty(t, body.Windows[2].RetryAt)
}

func TestEmojiApplicationUserSummarySurfacesFailure(t *testing.T) {
	rev := &stubEmojiReviewer{summaryErr: gorm.ErrInvalidDB}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationUserSummary, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), `"total":0`, "障害を「申請なし」に化けさせている")
}

// 未配線なら 500 (空の集計を返して「申請なし」と描かない)。
func TestEmojiApplicationUserSummaryWithoutServiceIs500(t *testing.T) {
	h := &apiadmin.Handler{}
	rec := doPost(h.EmojiApplicationUserSummary, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- #2962 申請枠の手動リセット ---

func resetQuotaHandler(t *testing.T) (*apiadmin.Handler, *stubEmojiReviewer, *testutil.MockUserRepository) {
	t.Helper()
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	rev := &stubEmojiReviewer{}
	h.SetEmojiApplicationReviewer(rev)
	return h, rev, userRepo
}

// **理由は必須。** 監査ログに残る唯一の文脈なので、空を許すと「誰かが戻した」
// 以上のことが後から分からなくなる。空白だけも弾く。
func TestEmojiApplicationResetQuotaRequiresReason(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	for _, body := range []string{
		`{"userId":"u1"}`,
		`{"userId":"u1","reason":""}`,
		`{"userId":"u1","reason":"   "}`,
	} {
		rec := doPost(h.EmojiApplicationResetUserQuota, body, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", body)
	}
	require.Zero(t, rev.resetCalls, "弾く前にリセットを実行している")
}

func TestEmojiApplicationResetQuotaRequiresUserID(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"reason":"理由"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, rev.resetCalls)
}

// **実在しない利用者に対して行を作らない。** 打ち間違いで残った行は
// 誰のものでもない監査記録になる。
func TestEmojiApplicationResetQuotaRejectsUnknownUser(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"nope","reason":"理由"}`, adminUser)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	require.Zero(t, rev.resetCalls, "存在しない利用者に対してリセットを実行している")
}

// **リセット前の使用数を監査ログに残す。** 後から採ると必ず 0 になり、
// 「何件使っていた人を戻したか」という記録として意味が無くなる。
func TestEmojiApplicationResetQuotaWritesModerationLog(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rev.resetBefore = emojiapplication.UserSummary{
		Windows: []emojiapplication.QuotaWindowUsage{
			{Period: "day", Used: 5, Limit: 5},
			{Period: "week", Used: 8, Limit: 20},
			{Period: "month", Used: 24, Limit: 50},
		},
	}
	repo := attachModLog(t, h)

	rec := doPost(h.EmojiApplicationResetUserQuota,
		`{"userId":"u1","reason":"修正後の画像で再申請してもらうため"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "u1", rev.lastResetUserID)
	require.Equal(t, adminUser.ID, rev.lastResetBy, "操作者が渡っていない")
	require.Equal(t, "修正後の画像で再申請してもらうため", rev.lastResetReason)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 },
		500*time.Millisecond, 5*time.Millisecond)
	entry := repo.Snapshot()[0]
	require.Equal(t, "resetEmojiApplicationQuota", entry.Type)
	require.Equal(t, adminUser.ID, entry.UserID, "操作者が記録されていない")

	var info map[string]any
	require.NoError(t, json.Unmarshal(entry.Info, &info))
	require.Equal(t, "u1", info["userId"], "対象者が記録されていない")
	require.Equal(t, "alice", info["userUsername"])
	require.Equal(t, "修正後の画像で再申請してもらうため", info["reason"], "理由が記録されていない")
	// **リセット前の使用数。** 0 になっていたら、書いてから採っている。
	require.EqualValues(t, 5, info["usedDay"], "リセット前の日次の使用数が残っていない")
	require.EqualValues(t, 8, info["usedWeek"])
	require.EqualValues(t, 24, info["usedMonth"])
}

// 実行後は最後のリセットを返す。**画面はこれを使わず集計を取り直す** (件数まで
// 更新しないと「5 / 5」のままに見える) が、レスポンスとしては操作の結果を返す。
func TestEmojiApplicationResetQuotaReturnsLastReset(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rev.resetRow = &model.EmojiApplicationQuotaReset{
		ID: "qr9", UserID: "u1", ResetByID: "mod1", Reason: "理由",
		CreatedAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"u1","reason":"理由"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		LastReset struct {
			At     string `json:"at"`
			ByID   string `json:"byId"`
			Reason string `json:"reason"`
		} `json:"lastReset"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.LastReset.At, "リセット時刻が返っていない")
	require.Equal(t, "mod1", body.LastReset.ByID, "実行者が返っていない")
	require.Equal(t, "理由", body.LastReset.Reason)
}

// 失敗を握り潰さない (成功したように見せない)。
func TestEmojiApplicationResetQuotaSurfacesFailure(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rev.resetErr = gorm.ErrInvalidDB
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"u1","reason":"理由"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 未配線なら 500 (黙って何もせず 200 を返さない)。
func TestEmojiApplicationResetQuotaWithoutServiceIs500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"u1","reason":"理由"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// user-summary が最後のリセットを返すこと (未実施なら null)。
func TestEmojiApplicationUserSummaryReportsLastReset(t *testing.T) {
	rev := &stubEmojiReviewer{summary: emojiapplication.UserSummary{
		LastReset: &model.EmojiApplicationQuotaReset{
			ID: "qr1", UserID: "u1", ResetByID: "mod1", Reason: "理由",
			CreatedAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		},
	}}
	rec := doPost(newEmojiReviewerHandler(t, rev).EmojiApplicationUserSummary, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"reason":"理由"`, "最後のリセットが返っていない")
	require.Contains(t, rec.Body.String(), `"byId":"mod1"`)

	none := &stubEmojiReviewer{}
	rec = doPost(newEmojiReviewerHandler(t, none).EmojiApplicationUserSummary, `{"userId":"u1"}`, adminUser)
	require.Contains(t, rec.Body.String(), `"lastReset":null`, "未実施が null になっていない")
}

// 列に入らない理由は 400 (500 にして「もう一度お試しください」と案内しない)。
func TestEmojiApplicationResetQuotaRejectsTooLongReason(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rev.resetErr = emojiapplication.ErrTooLong
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"u1","reason":"長い"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"列に入らない理由が 500 になっている (画面は再試行を促すが何度やっても通らない)")
	require.Contains(t, rec.Body.String(), "INVALID_PARAM")
	// **message が長さだけを言わない (#3022)。** NUL もここへ来るので、
	// 「短くすれば通る」と読める文面だと直しようが無くなる。
	require.Contains(t, rec.Body.String(), "invalid character")
}

// **窓の名前が空でも panic しない。** `Period` は素の string なので、
// バイトで切ると空のときに落ちる。
func TestEmojiApplicationResetQuotaToleratesEmptyPeriod(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rev.resetBefore = emojiapplication.UserSummary{
		Windows: []emojiapplication.QuotaWindowUsage{{Period: "", Used: 3}, {Period: "day", Used: 1}},
	}
	repo := attachModLog(t, h)
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"u1","reason":"理由"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 },
		500*time.Millisecond, 5*time.Millisecond)
	var info map[string]any
	require.NoError(t, json.Unmarshal(repo.Snapshot()[0].Info, &info))
	require.EqualValues(t, 1, info["usedDay"])
	// 空の期間名はキーを作らない ("used" に潰れて他の窓を上書きするため)。
	require.NotContains(t, info, "used")
}

// **操作者が取れないまま進めない (レビュー L3)。** 空のまま続けると
// `resetById = ”` の行が残り、しかも `logModeration` は actor nil で黙って
// return するので**監査ログが 1 件も残らない**。approve / reject と同じ扱い。
func TestEmojiApplicationResetQuotaWithoutActorIs500(t *testing.T) {
	h, rev, _ := resetQuotaHandler(t)
	rec := doPost(h.EmojiApplicationResetUserQuota, `{"userId":"u1","reason":"理由"}`, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Zero(t, rev.resetCalls, "操作者が取れないのにリセットを実行している")
}

// --- #2966 承認時に system 所有へ複製する ---

func newCreatorHandlerWithFetcher(t *testing.T, emojis *testutil.MockEmojiRepository,
	files *testutil.MockDriveFileRepository, fetcher *stubFetcher,
) *apiadmin.Handler {
	t.Helper()
	h := newCreatorHandler(t, emojis, files)
	h.SetEmojiImageFetcher(fetcher)
	return h
}

// **承認した絵文字が申請者のファイルを参照し続けないこと (#2966)。**
// これがバグの本体 — 申請者が drive から元のファイルを消すと、または
// アカウントを消すと、承認済みの絵文字が表示できなくなる。
func TestCreateFromApplicationCopiesToSystemFile(t *testing.T) {
	emojis := newEmojiRepoWith("")
	src := pngFile()
	files := newDriveRepoWith(src)
	sysWeb := "https://x/sys-webpublic.webp"
	sysWebType := "image/webp"
	fetcher := &stubFetcher{copyFile: &model.DriveFile{
		ID: "sys1", URL: "https://x/sys-original.png", Type: "image/png",
		WebpublicURL: &sysWeb, WebpublicType: &sysWebType,
	}}

	created, err := newCreatorHandlerWithFetcher(t, emojis, files, fetcher).
		CreateFromApplication(context.Background(), ownApplication())
	require.NoError(t, err)
	require.NotEmpty(t, created.EmojiID)
	require.Equal(t, "sys1", created.DriveFileID, "複製した drive ファイルを返していない")

	// **申請者のファイルから読んで複製すること。**
	require.Len(t, fetcher.copySrc, 1, "複製していない")
	require.Equal(t, "f1", fetcher.copySrc[0].ID, "元のファイルから読んでいない")
	require.Equal(t, "sushi", fetcher.copyNames[0], "絵文字の名前でファイルを作っていない")

	e := emojis.Emojis["sushi@"]
	require.NotNil(t, e)
	// **`originalUrl` は複製した実体の `url` と一致させる。** drive の孤児
	// cleanup がこの一致を参照保護の条件にしているので、ずれると保護が外れる。
	require.Equal(t, "https://x/sys-original.png", e.OriginalURL,
		"申請者のファイルを参照し続けている (申請者が消すと絵文字が壊れる)")
	require.Equal(t, "https://x/sys-webpublic.webp", e.PublicURL, "複製の webpublic を使っていない")
	require.NotNil(t, e.Type)
	require.Equal(t, "image/webp", *e.Type)

	// **元のファイルは触らない。** 申請者はノートの添付やプロフィールで
	// 使っている可能性がある。
	require.Equal(t, "https://x/original.png", src.URL, "元のファイルの URL が変わっている")
	require.NotNil(t, src.UserID, "元のファイルの所有者を奪っている")
	require.Equal(t, "u1", *src.UserID)
	require.NotNil(t, files.Files["f1"], "元のファイルが消えている")
}

// **複製に失敗したら絵文字を作らない (#2966)。** 元のファイルを参照して作ると、
// 直そうとしているバグをそのまま残すことになる。申請は pending のまま。
//
// **失敗の種類も潰さない (2 周目レビュー M1)。** 全部同じにすると、却下が
// 正しい申請 (画像の実体がもう無い) と、こちらの障害 (ストレージが落ちている) を
// モデレーターが区別できない。しかも後者を 4xx にすると監視で 5xx が立たない
// (#2792)。逆に前者を 5xx にすると運用側に直しようがない。
func TestCreateFromApplicationCopyErrorKinds(t *testing.T) {
	cases := []struct {
		name    string
		copyErr error
		want    error
	}{
		{"上限を超えた", safehttp.ErrResponseTooLarge, emojiapplication.ErrImageTooLarge},
		{"実体がもう無い", drive.ErrObjectNotFound, emojiapplication.ErrFileGone},
		{"ストレージ障害", errors.New("storage down"), emojiapplication.ErrImageCopyFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emojis := newEmojiRepoWith("")
			fetcher := &stubFetcher{copyErr: tc.copyErr}

			_, err := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher).
				CreateFromApplication(context.Background(), ownApplication())
			require.ErrorIs(t, err, tc.want, "複製の失敗が別の種類に化けている")
			require.Empty(t, emojis.Emojis, "複製に失敗したのに絵文字が作られている")
		})
	}
}

// **絵文字の作成に失敗したら複製したファイルを片付ける (#2966)。**
// 残すと誰からも参照されない孤児になる。
func TestCreateFromApplicationCleansUpSystemFileOnEmojiFailure(t *testing.T) {
	emojis := newEmojiRepoWith("")
	emojis.CreateErr = errors.New("db down")
	fetcher := &stubFetcher{}

	_, err := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher).
		CreateFromApplication(context.Background(), ownApplication())
	require.Error(t, err)
	require.Equal(t, []string{"f1-sys"}, fetcher.deletedIDs,
		"絵文字の作成に失敗したのに複製したファイルが残っている")
}

// **競合に負けたら絵文字と複製したファイルの両方を片付ける (#2966)。**
// 絵文字だけ消すと、複製が誰からも参照されないまま残る。
func TestDeleteCreatedEmojiRemovesSystemFile(t *testing.T) {
	emojis := newEmojiRepoWith("existing")
	fetcher := &stubFetcher{}
	h := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher)

	require.NoError(t, h.DeleteCreatedEmoji(context.Background(),
		emojiapplication.CreatedEmoji{EmojiID: "e-existing", DriveFileID: "sys9"}))
	require.Equal(t, []string{"sys9"}, fetcher.deletedIDs, "複製したファイルが残っている")
}

// **絵文字の削除に失敗しても複製したファイルは片付ける。** 別々の問題なので、
// 片方の失敗でもう片方を諦めない。
func TestDeleteCreatedEmojiKeepsFileWhenEmojiDeleteFails(t *testing.T) {
	emojis := newEmojiRepoWith("existing")
	emojis.DeleteErr = errors.New("db down")
	fetcher := &stubFetcher{}
	h := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher)

	err := h.DeleteCreatedEmoji(context.Background(),
		emojiapplication.CreatedEmoji{EmojiID: "e-existing", DriveFileID: "sys9"})
	require.Error(t, err, "絵文字の削除の失敗を握り潰している")
	// **「絵文字はあるのに画像が 404」を恒久化しない。** 消せなかった絵文字は
	// ピッカーに残り、名前が使用中なので再承認も `DUPLICATE_NAME` で通らない。
	// 複製を残しておけば、絵文字を消せたときに孤児 cleanup が回収する。
	require.Empty(t, fetcher.deletedIDs,
		"絵文字を消せていないのに画像だけ消している (絵文字が壊れたまま残る)")
}

// **リモート経路でも、弾いた取り込みを片付けること (#2966)。** MIME で拒否した
// 時点で誰からも参照されないので、残すと孤児の drive ファイルになる。
func TestCreateFromRemoteApplicationCleansUpRejectedFile(t *testing.T) {
	remoteHost := "remote.example"
	emojis := newEmojiRepoWith("")
	emojis.Emojis["kusa@remote.example"] = &model.Emoji{
		ID: "src1", Name: "kusa", Host: &remoteHost,
		OriginalURL: "https://remote.example/e/kusa.png",
	}
	fetcher := &stubFetcher{file: &model.DriveFile{
		ID: "sys-bad", URL: "https://x/bad.svg", Type: "image/svg+xml",
	}}
	h := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher)

	app := ownApplication()
	app.Kind = model.EmojiApplicationKindRemote
	app.RemoteHost = &remoteHost
	rname := "kusa"
	app.RemoteName = &rname

	_, err := h.CreateFromApplication(context.Background(), app)
	require.ErrorIs(t, err, emojiapplication.ErrUnsupportedFileType)
	require.Equal(t, []string{"sys-bad"}, fetcher.deletedIDs,
		"弾いた取り込みが drive に残っている")
}

// **リモート経路も複製した id を返すこと (#2966)。** 返さないと、競合に負けた
// ときに取り込んだファイルを片付けられない。
func TestCreateFromRemoteApplicationReturnsDriveFileID(t *testing.T) {
	remoteHost := "remote.example"
	emojis := newEmojiRepoWith("")
	emojis.Emojis["kusa@remote.example"] = &model.Emoji{
		ID: "src1", Name: "kusa", Host: &remoteHost,
		OriginalURL: "https://remote.example/e/kusa.png",
	}
	fetcher := &stubFetcher{file: &model.DriveFile{
		ID: "sys-ok", URL: "https://x/ok.png", Type: "image/png",
	}}
	h := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher)

	app := ownApplication()
	app.Kind = model.EmojiApplicationKindRemote
	app.RemoteHost = &remoteHost
	rname := "kusa"
	app.RemoteName = &rname

	created, err := h.CreateFromApplication(context.Background(), app)
	require.NoError(t, err)
	require.Equal(t, "sys-ok", created.DriveFileID, "取り込んだ drive ファイルを返していない")
	require.NotEmpty(t, created.EmojiID)
}

// **複製した実体の MIME を見ること (#2966 / レビュー M5)。** `Upload` は
// バイト列から型を引き直すので、元の行の宣言と実体がずれていると allowlist 外の
// 型が絵文字として登録される。remote 経路は同じ理由で取り込み後に見ている。
func TestCreateFromApplicationChecksCopiedMIME(t *testing.T) {
	emojis := newEmojiRepoWith("")
	// 元の行は png を名乗るが、複製したら svg だった、という形。
	fetcher := &stubFetcher{copyFile: &model.DriveFile{
		ID: "sys-bad", URL: "https://x/bad.svg", Type: "image/svg+xml",
	}}

	_, err := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrUnsupportedFileType)
	require.Empty(t, emojis.Emojis, "allowlist 外の型で絵文字が作られている")
	require.Equal(t, []string{"sys-bad"}, fetcher.deletedIDs, "弾いた複製が drive に残っている")
}

// 複製の失敗は 400「そんなファイルは無い」に潰さない (#2966 / レビュー M2)。
// ストレージや DB の障害が client error に化けると、監視でも 5xx が立たず、
// モデレーターには却下すべき申請に見える。
func TestCreateFromApplicationCopyFailureIsNotFileGone(t *testing.T) {
	fetcher := &stubFetcher{copyErr: errors.New("storage down")}
	_, err := newCreatorHandlerWithFetcher(t, newEmojiRepoWith(""), newDriveRepoWith(pngFile()), fetcher).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrImageCopyFailed)
	require.NotErrorIs(t, err, emojiapplication.ErrFileGone,
		"ストレージ障害が「ファイルが無い」(400) に化けている")
}

// 大きすぎる画像は、原因が分かる専用のエラーにする (#2966 / レビュー H1)。
func TestCreateFromApplicationCopyTooLarge(t *testing.T) {
	fetcher := &stubFetcher{copyErr: fmt.Errorf("read source file: %w", safehttp.ErrResponseTooLarge)}
	_, err := newCreatorHandlerWithFetcher(t, newEmojiRepoWith(""), newDriveRepoWith(pngFile()), fetcher).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrImageTooLarge)
	require.NotErrorIs(t, err, emojiapplication.ErrFileGone)
}

// **起動時の自己診断が依存する述語なので、定数化されると診断が静かに止まる。**
// 兄弟の `HasQuotaResetRepo` には同じ形のテストがあるのに、こちらだけ
// カバレッジ 0% で `return true` にしても全緑だった (2 周目レビュー L1)。
func TestHasEmojiImageFetcherReflectsWiring(t *testing.T) {
	h := &apiadmin.Handler{}
	require.False(t, h.HasEmojiImageFetcher(), "未配線なのに配線済みと報告している")

	h.SetEmojiImageFetcher(&stubFetcher{})
	require.True(t, h.HasEmojiImageFetcher(), "配線したのに未配線と報告している")
}

// **申請の sensitive 指定が複製へ渡ることを固定する (2 周目レビュー L2)。**
// `stubFetcher` は記録していたがどのテストも見ておらず、handler 側を
// `false` 固定に変える変異が全緑だった。fetcher 単体のテストは `true` を
// 直接渡しているので、受け渡しの経路はここでしか押さえられない。
func TestCreateFromApplicationPassesSensitiveToCopy(t *testing.T) {
	for _, sensitive := range []bool{true, false} {
		t.Run(map[bool]string{true: "sensitive", false: "通常"}[sensitive], func(t *testing.T) {
			app := ownApplication()
			app.IsSensitive = sensitive
			fetcher := &stubFetcher{}

			_, err := newCreatorHandlerWithFetcher(t, newEmojiRepoWith(""), newDriveRepoWith(pngFile()), fetcher).
				CreateFromApplication(context.Background(), app)
			require.NoError(t, err)
			require.Equal(t, []bool{sensitive}, fetcher.copySensitive,
				"申請の sensitive 指定が複製に伝わっていない")
		})
	}
}

// **承認が通ったら取り込んだ画像を消さない** (#2998)。
//
// 弾いた経路と `Create` 失敗の経路は既に固定してあるが、**成功したときに消さない**
// ことは誰も見ていなかった。後始末を `defer` へ動かすような変更で、承認した絵文字の
// 画像が作成直後に消えて「絵文字はピッカーに出るのに画像は 404」になる
// (`DeleteCreatedEmoji` の doc コメントが恒久化を避けると書いているのと同じ状態)。
func TestCreateFromRemoteApplicationKeepsFileOnSuccess(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	fetcher := &stubFetcher{file: &model.DriveFile{
		ID: "sys-ok", URL: "https://local.example/files/sushi.png", Type: "image/png",
	}}
	h := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher)

	created, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.NoError(t, err)
	require.NotEmpty(t, created.EmojiID)
	require.Equal(t, "sys-ok", created.DriveFileID)
	require.Empty(t, fetcher.deletedIDs,
		"承認が通ったのに取り込んだ画像を消している (絵文字だけ残って画像が 404 になる)")
}

// `related` はサービス層を通らずリポジトリを直叩きするので、申請 ID の guard を
// ここにも置いてある (#3022 のレビュー M1)。**NUL を含む id は SELECT が
// その時点で落ち、`IsNotFound` でもないので 500 になっていた。**
func TestEmojiApplicationRelatedRejectsUnstorableID(t *testing.T) {
	// 実 DB と同じ壊れ方をさせる — not-found を返す stub だと、guard を外しても
	// 同じ 404 になって検出できない。
	repo := &stubAppsRepo{findErr: errors.New("invalid message format")}
	h := &apiadmin.Handler{}
	h.SetEmojiApplicationRepo(repo)

	// **JSON では NUL をエスケープで送る。** 生バイトを body に載せると JSON
	// として不正になり、`Bind` の 400 で guard まで届かない。
	rec := doPost(h.EmojiApplicationRelated, `{"applicationId":"a\u0000b"}`, adminUser)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"存在しえない id で申請を引きに行っている (SELECT がそこで落ちる)")
	require.Contains(t, rec.Body.String(), "NO_SUCH_APPLICATION")
}
