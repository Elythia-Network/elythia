package emojiapplication

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

type fakeApps struct {
	rows      map[string]*model.EmojiApplication
	createErr error
	updateErr error
	findErr   error
	// loseRace makes the next UpdateIfPending report "someone else wrote first"
	// while FindByID still sees the row as pending. **読んだ後に負ける**状況は
	// これでしか作れない (両方が同じ map を見ているため)。
	loseRace bool
	// updateLands makes a failing UpdateIfPending still persist the row, so the
	// "commit は通ったが応答が返らなかった" case can be exercised (#2966)。
	updateLands bool
	// updateLandsAs replaces the row instead, so approvalLanded can be exercised
	// against a row written by **someone else** (2 周目レビュー M3)。
	updateLandsAs *model.EmojiApplication
	// findErrAfter makes FindByID fail from the Nth call onward (1-origin).
	// **2 回目だけ落とす**のに要る — Approve は自分で 1 回読んでから
	// approvalLanded でもう 1 回読む。
	findErrAfter int
	findCalls    int
	// quotaLimits records what Create asked for so the policy plumbing can be
	// asserted without a database (#2958 / #2977).
	quotaLimits repository.QuotaLimits
	quotaCalls  int
	quotaErr    error

	// #2961 (ユーザーモデレーション画面の集計)
	userCounts      repository.StatusCounts
	countErr        error
	usage           []repository.QuotaWindowUsage
	usageErr        error
	countUserID     string
	usageUserID     string
	usageWindows    []repository.QuotaWindow
	usageNow        time.Time
	usageMaxPending int
	usagePending    int
	usageResetAt    time.Time
	onUsage         func()
}

func newFakeApps() *fakeApps { return &fakeApps{rows: map[string]*model.EmojiApplication{}} }

func (f *fakeApps) Create(a *model.EmojiApplication) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.rows[a.ID] = a
	return nil
}

func (f *fakeApps) FindRelated(*model.EmojiApplication, int, string) ([]repository.RelatedApplication, error) {
	return nil, nil
}

func (f *fakeApps) CountRelated(*model.EmojiApplication) (repository.RelatedCounts, error) {
	return repository.RelatedCounts{}, nil
}

func (f *fakeApps) ListByUserFiltered(string, string, string, int, string) ([]model.EmojiApplication, error) {
	return nil, nil
}

// #2961. **err はメソッドごとに分ける** — 1 つを共有すると、片方の分岐を
// 消してももう片方の err で同じ結果になり、テストが揃って空虚になる。
func (f *fakeApps) CountByUserStatus(userID string) (repository.StatusCounts, error) {
	f.countUserID = userID
	return f.userCounts, f.countErr
}

func (f *fakeApps) QuotaUsage(userID string, limits repository.QuotaLimits, now time.Time) (repository.QuotaUsage, error) {
	if f.onUsage != nil {
		f.onUsage()
	}
	f.usageUserID, f.usageWindows, f.usageNow = userID, limits.Windows, now
	f.usageMaxPending = limits.MaxPending
	f.usageResetAt = limits.ResetAt
	if f.usageErr != nil {
		return repository.QuotaUsage{}, f.usageErr
	}
	out := repository.QuotaUsage{
		Windows:    f.usage,
		Pending:    f.usagePending,
		MaxPending: limits.MaxPending,
	}
	if out.Windows == nil {
		out.Windows = make([]repository.QuotaWindowUsage, 0, len(limits.Windows))
		for _, w := range limits.Windows {
			out.Windows = append(out.Windows, repository.QuotaWindowUsage{Window: w})
		}
	}
	return out, nil
}

func (f *fakeApps) CreateWithQuota(a *model.EmojiApplication, l repository.QuotaLimits) error {
	f.quotaLimits = l
	f.quotaCalls++
	if f.quotaErr != nil {
		return f.quotaErr
	}
	return f.Create(a)
}

func (f *fakeApps) FindByID(id string) (*model.EmojiApplication, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.findErrAfter > 0 && f.findCalls >= f.findErrAfter {
		return nil, errBoom
	}
	a, ok := f.rows[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	// **コピーを返す。** 同じポインタを返すと service の書き換えが保存済みの
	// 行にも反映され、条件付き UPDATE の検証が成立しない (実 DB はコピーを返す)。
	cp := *a
	return &cp, nil
}

func (f *fakeApps) List(string, int, string) ([]model.EmojiApplication, error)       { return nil, nil }
func (f *fakeApps) ListByUser(string, int, string) ([]model.EmojiApplication, error) { return nil, nil }
func (f *fakeApps) CountPending() (int64, error)                                     { return 0, nil }
func (f *fakeApps) UpdateIfPending(a *model.EmojiApplication) (bool, error) {
	if f.updateErr != nil {
		// **「書けたが応答が返らなかった」を作る (#2966)。** 行には反映された
		// まま err を返すと、呼び出し側が読み直して気付けるかを試せる。
		if f.updateLands {
			cp := *a
			f.rows[a.ID] = &cp
		}
		if f.updateLandsAs != nil {
			cp := *f.updateLandsAs
			f.rows[a.ID] = &cp
		}
		return false, f.updateErr
	}
	if f.loseRace {
		f.loseRace = false
		return false, nil
	}
	// 条件付き UPDATE を模す。読んだ後に他で処理されていたら書かない。
	cur, ok := f.rows[a.ID]
	if !ok || cur.Status != model.EmojiApplicationPending {
		return false, nil
	}
	f.rows[a.ID] = a
	return true, nil
}

type fakeEmojis struct {
	existing *model.Emoji
	err      error
	// remote は host 付きの検索に答える (kind = remote の検証用)。
	remote *model.Emoji
	// 期待する引数。空なら検査しない。
	remoteName string
	remoteHost string
}

// **name も host も見る。** 握り潰すと、呼び出し側が引数を入れ替えても
// 気付けない (レビュー M5)。実測でその変異が素通りした。
func (f *fakeEmojis) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	if f.err != nil {
		return nil, f.err
	}
	if host != nil {
		if f.remote == nil {
			return nil, gorm.ErrRecordNotFound
		}
		// 引数が入れ替わっていれば期待と合わない。
		if f.remoteName != "" && name != f.remoteName {
			return nil, gorm.ErrRecordNotFound
		}
		if f.remoteHost != "" && *host != f.remoteHost {
			return nil, gorm.ErrRecordNotFound
		}
		return f.remote, nil
	}
	if f.existing == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return f.existing, nil
}

// okFiles returns a drive file owned by u1 so the happy path passes.
type okFiles struct {
	file *model.DriveFile
	err  error
}

func (f *okFiles) FindByID(string) (*model.DriveFile, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.file != nil {
		return f.file, nil
	}
	owner := "u1"
	return &model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png"}, nil
}

type fixedID struct{ n int }

func (f *fixedID) Generate(time.Time) string { f.n++; return "id" + string(rune('0'+f.n)) }

type fakeCreator struct {
	id string
	// driveFileID は承認時に作った system 所有の drive ファイル (#2966)。
	driveFileID string
	err         error
	called      bool
	deleted     []CreatedEmoji
	deleteErr   error
}

func (c *fakeCreator) CreateFromApplication(context.Context, *model.EmojiApplication) (CreatedEmoji, error) {
	c.called = true
	if c.err != nil {
		return CreatedEmoji{}, c.err
	}
	return CreatedEmoji{EmojiID: c.id, DriveFileID: c.driveFileID}, nil
}

func (c *fakeCreator) DeleteCreatedEmoji(_ context.Context, created CreatedEmoji) error {
	c.deleted = append(c.deleted, created)
	return c.deleteErr
}

// deletedEmojiIDs renders the emoji ids handed to DeleteCreatedEmoji.
func (c *fakeCreator) deletedEmojiIDs() []string {
	out := make([]string, 0, len(c.deleted))
	for _, d := range c.deleted {
		out = append(out, d.EmojiID)
	}
	return out
}

func newService(t *testing.T, apps *fakeApps, emojis *fakeEmojis, creator EmojiCreator) *Service {
	t.Helper()
	return NewService(apps, emojis, &okFiles{}, &fixedID{}, creator, nil)
}

func validInput() CreateInput {
	return CreateInput{UserID: "u1", Name: "sushi", License: "自作", FileID: "f1"}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateInput)
		want   error
	}{
		{"名前が空", func(in *CreateInput) { in.Name = "" }, ErrInvalidName},
		{"名前に記号", func(in *CreateInput) { in.Name = "su-shi" }, ErrInvalidName},
		{"名前に全角", func(in *CreateInput) { in.Name = "すし" }, ErrInvalidName},
		{"ライセンスが空", func(in *CreateInput) { in.License = "  " }, ErrLicenseRequired},
		{"ファイルが無い", func(in *CreateInput) { in.FileID = "" }, ErrFileRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			_, err := newService(t, newFakeApps(), &fakeEmojis{}, nil).Create(in)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// **同名が既にあるなら受け付けない。** 受けると、審査で承認を押した瞬間に
// DUPLICATE_NAME で落ちる。押す前に分かるほうがよい。
func TestCreateRejectsExistingName(t *testing.T) {
	_, err := newService(t, newFakeApps(), &fakeEmojis{existing: &model.Emoji{ID: "e1"}}, nil).Create(validInput())
	require.ErrorIs(t, err, ErrDuplicateName)
}

// **DB 障害を「重複なし」に丸めない (#2792)。** 丸めると、確認できていないのに
// 申請を受け、承認時に落ちる。
func TestCreateSurfacesLookupFailure(t *testing.T) {
	boom := errors.New("db down")
	_, err := newService(t, newFakeApps(), &fakeEmojis{err: boom}, nil).Create(validInput())
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrDuplicateName)
}

func TestCreateMapsDuplicatePending(t *testing.T) {
	apps := newFakeApps()
	apps.createErr = repository.ErrEmojiApplicationDuplicatePending
	_, err := newService(t, apps, &fakeEmojis{}, nil).Create(validInput())
	require.ErrorIs(t, err, ErrAlreadyPending)
}

func TestCreateNormalizesAliases(t *testing.T) {
	in := validInput()
	in.Aliases = []string{"おすし", " ", "おすし", "寿司"}
	in.Category = "  たべもの  "
	app, err := newService(t, newFakeApps(), &fakeEmojis{}, nil).Create(in)
	require.NoError(t, err)
	// 空白と重複は落ちる。
	require.Equal(t, []string{"おすし", "寿司"}, []string(app.Aliases))
	require.Equal(t, "たべもの", *app.Category)
}

func TestApproveRegistersThenCloses(t *testing.T) {
	apps := newFakeApps()
	app := &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.rows["a1"] = app
	creator := &fakeCreator{id: "e9"}

	out, err := newService(t, apps, &fakeEmojis{}, creator).Approve(context.Background(), "a1", "mod")
	require.NoError(t, err)
	require.True(t, creator.called, "emoji が作られていない")
	require.Equal(t, model.EmojiApplicationApproved, out.Status)
	require.Equal(t, "e9", *out.EmojiID)
	require.Equal(t, "mod", *out.ProcessedByID)
	require.NotNil(t, out.ProcessedAt)
}

// **emoji の作成に失敗したら申請は pending のまま。** 閉じてしまうと
// 「承認済みなのに絵文字が無い」行が残り、押し直す手段も無くなる。
func TestApproveKeepsPendingWhenCreationFails(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", Status: model.EmojiApplicationPending}
	boom := errors.New("unsupported")
	creator := &fakeCreator{err: boom}

	_, err := newService(t, apps, &fakeEmojis{}, creator).Approve(context.Background(), "a1", "mod")
	require.ErrorIs(t, err, boom)
	require.Equal(t, model.EmojiApplicationPending, apps.rows["a1"].Status)
	require.Nil(t, apps.rows["a1"].ProcessedAt)
}

func TestRejectRecordsReason(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", Status: model.EmojiApplicationPending}

	out, err := newService(t, apps, &fakeEmojis{}, nil).Reject(context.Background(), "a1", "mod", " 潰れて読めません ")
	require.NoError(t, err)
	require.Equal(t, model.EmojiApplicationRejected, out.Status)
	require.Equal(t, "潰れて読めません", *out.RejectReason)
}

// **処理済みの申請は二度処理できない。** 2 人のモデレーターが同時に押した
// 場合もここに来る。
func TestApproveAndRejectRequirePending(t *testing.T) {
	for _, status := range []string{model.EmojiApplicationApproved, model.EmojiApplicationRejected, model.EmojiApplicationCanceled} {
		apps := newFakeApps()
		apps.rows["a1"] = &model.EmojiApplication{ID: "a1", Status: status}
		svc := newService(t, apps, &fakeEmojis{}, &fakeCreator{id: "e1"})

		_, err := svc.Approve(context.Background(), "a1", "mod")
		require.ErrorIs(t, err, ErrNotPending, status)
		_, err = svc.Reject(context.Background(), "a1", "mod", "x")
		require.ErrorIs(t, err, ErrNotPending, status)
	}
}

// **他人の申請を取り下げさせない。** id は申請者に見えるので、所有者の確認を
// 落とすと誰でも他人の申請を消せる。
func TestCancelChecksOwner(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "owner", Status: model.EmojiApplicationPending}
	svc := newService(t, apps, &fakeEmojis{}, nil)

	require.ErrorIs(t, svc.Cancel("a1", "someone-else"), ErrForbidden)
	require.Equal(t, model.EmojiApplicationPending, apps.rows["a1"].Status, "他人が取り下げられてしまった")

	require.NoError(t, svc.Cancel("a1", "owner"))
	require.Equal(t, model.EmojiApplicationCanceled, apps.rows["a1"].Status)
}

func TestNotFound(t *testing.T) {
	svc := newService(t, newFakeApps(), &fakeEmojis{}, &fakeCreator{})
	_, err := svc.Approve(context.Background(), "missing", "mod")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, svc.Cancel("missing", "u1"), ErrNotFound)
}

// **他人の drive ファイルを申請の素材にさせない (レビュー H2)。**
//
// 許すと (a) 応答に含まれる URL から他人のファイルを読める
// (`/files/:accessKey` は認証なしの GET で URL そのものが capability)、
// (b) 承認すると他人の画像がサーバーの絵文字として登録される。
// aidx は連番を含むので、他人のファイル ID は隣接探索で当たる。
func TestCreateRejectsForeignFile(t *testing.T) {
	other := "someone-else"
	files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: &other, Type: "image/png"}}
	svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)

	_, err := svc.Create(validInput())
	// **「他人のもの」とは答えない。** 区別できると ID の存在確認に使える。
	require.ErrorIs(t, err, ErrFileGone)
}

// 未紐付け (userId が NULL) も拒否する。誰のものとも言えないファイルは
// 申請の素材にしない (applyMediaUpdate と同じ判断)。
func TestCreateRejectsUnownedFile(t *testing.T) {
	files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: nil, Type: "image/png"}}
	svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
	_, err := svc.Create(validInput())
	require.ErrorIs(t, err, ErrFileGone)
}

// **申請の時点で MIME を見る。** 承認まで通してから落ちると、モデレーターが
// 押した後にエラーになる。
func TestCreateRejectsNonImage(t *testing.T) {
	owner := "u1"
	files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: &owner, Type: "video/mp4"}}
	svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
	_, err := svc.Create(validInput())
	require.ErrorIs(t, err, ErrUnsupportedFileType)
}

// **承認時に複製できない大きさは申請の時点で断る (2 周目レビュー M2)。**
// drive が受け取る上限 (role policy の `maxFileSizeMb`) はこの値より上げられる
// ので、ここで見ないと「申請はできたのに承認だけが恒久的に失敗する」帯が残る。
// 申請者には何も直しようが無く、モデレーターには却下すべき申請に見える。
func TestCreateRejectsFileLargerThanCopyLimit(t *testing.T) {
	owner := "u1"
	files := &okFiles{file: &model.DriveFile{
		ID: "f1", UserID: &owner, Type: "image/png",
		Size: int(MaxEmojiCopyBytes) + 1,
	}}
	svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
	_, err := svc.Create(validInput())
	require.ErrorIs(t, err, ErrImageTooLarge, "承認できない大きさの申請が通っている")
}

// 上限ちょうどは通す (境界を片側に寄せない)。
func TestCreateAcceptsFileAtCopyLimit(t *testing.T) {
	owner := "u1"
	files := &okFiles{file: &model.DriveFile{
		ID: "f1", UserID: &owner, Type: "image/png",
		Size: int(MaxEmojiCopyBytes),
	}}
	svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
	_, err := svc.Create(validInput())
	require.NoError(t, err, "上限ちょうどの申請が弾かれている")
}

// **未配線なら通さない (fail-closed)。** 検証できないものを通すと、配線を
// 落とした瞬間に穴が開く。
func TestCreateRejectsWhenFilesUnwired(t *testing.T) {
	svc := NewService(newFakeApps(), &fakeEmojis{}, nil, &fixedID{}, nil, nil)
	_, err := svc.Create(validInput())
	require.ErrorIs(t, err, ErrFileGone)
}

// DB 障害を not-found に丸めない (#2792)。
func TestCreateSurfacesFileLookupFailure(t *testing.T) {
	boom := errors.New("db down")
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{err: boom}, &fixedID{}, nil, nil)
	_, err := svc.Create(validInput())
	require.ErrorIs(t, err, boom)
}

// **申請者は入力ではなく呼び出し元で決まる (レビュー H7)。**
func TestCreateRecordsCallerAsApplicant(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)

	app, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Equal(t, "u1", app.UserID)
	require.Equal(t, "u1", apps.rows[app.ID].UserID)
}

// **2 人が同じ申請を開いていた場合、後から押した側は負ける (レビュー M1)。**
//
// 読んでから書くだけだと last-write-wins になり、承認で emoji を作った直後に
// 却下が被さって「絵文字は存在するのに申請は却下、emojiId も消える」状態になる。
// 窓は「一覧を開いてから押すまで」の全期間なので実際に起きる。
func TestConcurrentReviewLosesGracefully(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	svc := newService(t, apps, &fakeEmojis{}, &fakeCreator{id: "e9"})

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.NoError(t, err)

	_, err = svc.Reject(context.Background(), "a1", "mod2", "潰れて読めません")
	require.ErrorIs(t, err, ErrNotPending, "後から押した却下が承認を上書きしている")

	final := apps.rows["a1"]
	require.Equal(t, model.EmojiApplicationApproved, final.Status)
	require.Equal(t, "e9", *final.EmojiID, "emojiId が消えている")
	require.Equal(t, "mod1", *final.ProcessedByID)
	require.Nil(t, final.RejectReason, "却下理由が混ざっている")
}

// 取り下げも同じ。処理済みの申請は取り下げられない。
func TestCancelLosesToReview(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	svc := newService(t, apps, &fakeEmojis{}, &fakeCreator{id: "e1"})

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.NoError(t, err)
	require.ErrorIs(t, svc.Cancel("a1", "u1"), ErrNotPending)
	require.Equal(t, model.EmojiApplicationApproved, apps.rows["a1"].Status)
}

// **DB 障害を「他の人が処理済み」に丸めない。** 丸めると、障害中にモデレーターが
// 「誰かが先に処理した」と誤解して一覧を引き直し続けることになる。
func TestReviewSurfacesUpdateFailure(t *testing.T) {
	boom := errors.New("db down")
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{"承認", func(s *Service) error { _, e := s.Approve(context.Background(), "a1", "mod"); return e }},
		{"却下", func(s *Service) error { _, e := s.Reject(context.Background(), "a1", "mod", "x"); return e }},
		{"取り下げ", func(s *Service) error { return s.Cancel("a1", "u1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := newFakeApps()
			apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
			apps.updateErr = boom
			svc := newService(t, apps, &fakeEmojis{}, &fakeCreator{id: "e1"})

			err := tc.call(svc)
			require.ErrorIs(t, err, boom)
			require.NotErrorIs(t, err, ErrNotPending, "DB 障害を「処理済み」に丸めている")
		})
	}
}

// creator が未配線なら承認しない。emoji を作れないのに申請だけ閉じると、
// 「承認済みなのに絵文字が無い」行が残る。
func TestApproveRequiresCreator(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", Status: model.EmojiApplicationPending}
	_, err := newService(t, apps, &fakeEmojis{}, nil).Approve(context.Background(), "a1", "mod")
	require.Error(t, err)
	require.Equal(t, model.EmojiApplicationPending, apps.rows["a1"].Status)
}

// FindByID の DB 障害を not-found に丸めない (#2792)。
func TestPendingSurfacesLookupFailure(t *testing.T) {
	boom := errors.New("db down")
	apps := newFakeApps()
	apps.findErr = boom
	svc := newService(t, apps, &fakeEmojis{}, &fakeCreator{})

	_, err := svc.Approve(context.Background(), "a1", "mod")
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrNotFound)
}

// nil を渡しても落ちない (未配線の構成)。
func TestNewResultNotifierAcceptsNil(t *testing.T) {
	require.Nil(t, NewResultNotifier(nil))
}

// **他人の申請は、処理済みでも「無い」と同じ応答にする (レビュー M2)。**
//
// pending の判定を所有者確認より先に行うと、他人の処理済み申請に対して
// ErrNotPending が返り、存在しない ID (ErrNotFound) と区別できてしまう。
// ID 列挙のオラクルになる。
func TestCancelDoesNotLeakOthersApplications(t *testing.T) {
	apps := newFakeApps()
	apps.rows["mine"] = &model.EmojiApplication{ID: "mine", UserID: "u1", Status: model.EmojiApplicationApproved}
	apps.rows["theirs-pending"] = &model.EmojiApplication{ID: "theirs-pending", UserID: "other", Status: model.EmojiApplicationPending}
	apps.rows["theirs-done"] = &model.EmojiApplication{ID: "theirs-done", UserID: "other", Status: model.EmojiApplicationApproved}
	svc := newService(t, apps, &fakeEmojis{}, nil)

	// 他人のものは pending でも処理済みでも同じ error になること。
	pendingErr := svc.Cancel("theirs-pending", "u1")
	doneErr := svc.Cancel("theirs-done", "u1")
	require.ErrorIs(t, pendingErr, ErrForbidden)
	require.ErrorIs(t, doneErr, ErrForbidden,
		"他人の処理済み申請が ErrNotPending として区別できている (ID 列挙のオラクル)")

	// 自分のものなら、処理済みだと分かってよい。
	require.ErrorIs(t, svc.Cancel("mine", "u1"), ErrNotPending)
}

// **申請側と承認側で MIME の判定が一致していること (レビュー R3)。**
//
// 申請が prefix 判定だと `image/svg+xml` が通り、承認で必ず落ちる。
// svg は本文中にそのまま埋め込まれるので XSS になり、承認側は allowlist で
// 止めている — 2 層が別のルールを持つと「押した後にエラー」が svg でだけ残る。
func TestCreateUsesTheSameAllowlistAsApproval(t *testing.T) {
	for _, mime := range []string{"image/svg+xml", "image/heic", "image/jxl", "image/anything"} {
		t.Run(mime, func(t *testing.T) {
			owner := "u1"
			files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: &owner, Type: mime}}
			svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
			_, err := svc.Create(validInput())
			require.ErrorIs(t, err, ErrUnsupportedFileType,
				"%s が申請で通っている (承認で必ず落ちる)", mime)
		})
	}
	// allowlist にあるものは通ること。
	for _, mime := range []string{"image/png", "image/gif", "image/webp"} {
		owner := "u1"
		files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: &owner, Type: mime}}
		svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
		_, err := svc.Create(validInput())
		require.NoError(t, err, "%s が申請で弾かれている", mime)
	}
}

// **長さは文字数で見る (レビュー R4)。** varchar(N) は文字数なのに len() は
// バイト数なので、バイトで数えると日本語は列の約 1/3 しか使えない。
func TestCreateCountsLengthInRunes(t *testing.T) {
	svc := func(apps *fakeApps) *Service {
		return NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	}

	// 1024 文字 (3072 バイト) は列に入るので通ること。
	in := validInput()
	in.License = strings.Repeat("あ", 1024)
	_, err := svc(newFakeApps()).Create(in)
	require.NoError(t, err, "1024 文字のライセンスが弾かれている (バイトで数えている)")

	// 1025 文字は弾くこと。
	in.License = strings.Repeat("あ", 1025)
	_, err = svc(newFakeApps()).Create(in)
	require.ErrorIs(t, err, ErrTooLong)

	// category も同じ。
	in = validInput()
	in.Category = strings.Repeat("あ", 128)
	_, err = svc(newFakeApps()).Create(in)
	require.NoError(t, err, "128 文字のカテゴリが弾かれている")

	in.Category = strings.Repeat("あ", 129)
	_, err = svc(newFakeApps()).Create(in)
	require.ErrorIs(t, err, ErrTooLong)
}

// **承認が競合に負けたら、作った emoji を片付ける (レビュー R5)。**
//
// 残すと、申請は「却下」で通知も却下なのに絵文字だけ登録済みで使える状態になる。
// M1 前の「絵文字があるのに申請は却下」と症状が同じ。
func TestApproveCleansUpWhenLosingRace(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	// **drive の複製も一緒に渡ること (#2966 / レビュー M3)。** 絵文字だけ
	// 片付けると、承認時に作った system 所有のファイルが誰からも参照されない
	// まま残り、しかも孤児 cleanup にも拾われない。
	creator := &fakeCreator{id: "e-orphan", driveFileID: "sys-orphan"}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	// **読んだ時点では pending だが、書く直前に他のモデレーターが処理する。**
	apps.loseRace = true

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.ErrorIs(t, err, ErrNotPending)
	require.True(t, creator.called, "emoji が作られていない (前提が崩れている)")
	require.Equal(t, []CreatedEmoji{{EmojiID: "e-orphan", DriveFileID: "sys-orphan"}}, creator.deleted,
		"drive の複製が片付けられていない (#2966)")
	require.Equal(t, []string{"e-orphan"}, creator.deletedEmojiIDs(),
		"競合に負けた承認の emoji が残っている")

	// 申請は書き換わっていないこと。
	require.Equal(t, model.EmojiApplicationPending, apps.rows["a1"].Status)
}

// 片付けに失敗しても審査の結果は変わらない (通知や状態を壊さない)。
func TestApproveSurvivesCleanupFailure(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.loseRace = true
	creator := &fakeCreator{id: "e1", deleteErr: errors.New("delete failed")}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.ErrorIs(t, err, ErrNotPending, "削除の失敗が審査の結果に混ざっている")
}

// **審査したら必ず通知すること (レビュー Low 3)。** 呼び出しを落としても
// 状態は正しいままなので、テストが無いと「結果が届かない」回帰が緑で通る。
func TestReviewNotifiesApplicant(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{"承認", func(s *Service) error { _, e := s.Approve(context.Background(), "a1", "mod"); return e }},
		{"却下", func(s *Service) error { _, e := s.Reject(context.Background(), "a1", "mod", "x"); return e }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := newFakeApps()
			apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
			notifier := &recordingNotifier{}
			svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, &fakeCreator{id: "e1"}, notifier)

			require.NoError(t, tc.call(svc))
			require.Len(t, notifier.sent, 1, "申請者に通知していない")
			require.Equal(t, "a1", notifier.sent[0])
		})
	}
}

// 取り下げでは通知しない。自分で押したので届ける意味が無い。
func TestCancelDoesNotNotify(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	notifier := &recordingNotifier{}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, notifier)

	require.NoError(t, svc.Cancel("a1", "u1"))
	require.Empty(t, notifier.sent)
}

// **取り下げも競合に負けたら 204 を返さない (レビュー Low 4)。**
func TestCancelReportsLostRace(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.loseRace = true
	svc := newService(t, apps, &fakeEmojis{}, nil)

	require.ErrorIs(t, svc.Cancel("a1", "u1"), ErrNotPending,
		"競合に負けたのに成功を返している")
}

type recordingNotifier struct{ sent []string }

func (r *recordingNotifier) NotifyEmojiApplicationProcessed(_ context.Context, app *model.EmojiApplication) error {
	r.sent = append(r.sent, app.ID)
	return nil
}

func remoteInput() CreateInput {
	return CreateInput{
		UserID: "u1", Kind: model.EmojiApplicationKindRemote,
		Name: "sushi", License: "リモートから取り込み",
		RemoteHost: "example.com", RemoteName: "sushi_remote",
	}
}

// **リモート絵文字の申請も同じ共通検証を通ること (#2935)。**
func TestCreateRemote(t *testing.T) {
	apps := newFakeApps()
	emojis := &fakeEmojis{remote: &model.Emoji{ID: "e-remote", Name: "sushi_remote"}}
	svc := NewService(apps, emojis, &okFiles{}, &fixedID{}, nil, nil)

	app, err := svc.Create(remoteInput())
	require.NoError(t, err)
	require.Equal(t, model.EmojiApplicationKindRemote, app.Kind)
	require.Equal(t, "example.com", *app.RemoteHost)
	require.Equal(t, "sushi_remote", *app.RemoteName)
	// **自作用の列は空のまま。** kind を取り違えると承認で別の経路に入る。
	require.Nil(t, app.FileID)
}

// **host / name が要ること。** 無いと承認時に取り直す手がかりが無い。
func TestCreateRemoteRequiresHostAndName(t *testing.T) {
	emojis := &fakeEmojis{remote: &model.Emoji{ID: "e1"}}
	for _, tc := range []struct {
		name   string
		mutate func(*CreateInput)
	}{
		{"host が空", func(in *CreateInput) { in.RemoteHost = "" }},
		{"name が空", func(in *CreateInput) { in.RemoteName = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := remoteInput()
			tc.mutate(&in)
			_, err := NewService(newFakeApps(), emojis, &okFiles{}, &fixedID{}, nil, nil).Create(in)
			require.ErrorIs(t, err, ErrRemoteRequired)
		})
	}
}

// **知らないリモート絵文字は受け付けない。** 承認時に取り直せないので、
// 申請の時点で引けることを確かめる。
func TestCreateRemoteRejectsUnknownEmoji(t *testing.T) {
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	_, err := svc.Create(remoteInput())
	require.ErrorIs(t, err, ErrNoSuchRemoteEmoji)
}

// **リモートでも同名のローカル絵文字があれば弾く。** 承認を押してから
// DUPLICATE_NAME で落ちるのを防ぐ (own と同じ理由)。
func TestCreateRemoteRejectsDuplicateLocalName(t *testing.T) {
	emojis := &fakeEmojis{
		existing: &model.Emoji{ID: "e-local"},
		remote:   &model.Emoji{ID: "e-remote"},
	}
	svc := NewService(newFakeApps(), emojis, &okFiles{}, &fixedID{}, nil, nil)
	_, err := svc.Create(remoteInput())
	require.ErrorIs(t, err, ErrDuplicateName)
}

// **未知の kind は弾く。** 既定 (空文字) は own に倒す。
//
// **ライセンスを空にして試す (レビュー Low 4)。** fixture の
// `remoteInput()` はライセンスを埋めているので、そのままだと「kind より先に
// ライセンスを見る」順序の誤りを隠す。リモートのダイアログは license を任意と
// して空で送るので、**実際に通るのは空のほう**。
func TestCreateRejectsUnknownKind(t *testing.T) {
	for _, license := range []string{"リモートから取り込み", ""} {
		in := remoteInput()
		in.Kind = "whatever"
		in.License = license
		_, err := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).Create(in)
		require.ErrorIsf(t, err, ErrInvalidKind, "license=%q", license)
	}
}

// host / name の長さも列に収める。
func TestCreateRemoteChecksLength(t *testing.T) {
	emojis := &fakeEmojis{remote: &model.Emoji{ID: "e1"}}
	in := remoteInput()
	in.RemoteHost = strings.Repeat("あ", 129)
	_, err := NewService(newFakeApps(), emojis, &okFiles{}, &fixedID{}, nil, nil).Create(in)
	require.ErrorIs(t, err, ErrTooLong)
}

// **DB 障害を「知らない絵文字」に丸めない (#2792)。**
func TestCreateRemoteSurfacesLookupFailure(t *testing.T) {
	boom := errors.New("db down")
	svc := NewService(newFakeApps(), &fakeEmojis{err: boom}, &okFiles{}, &fixedID{}, nil, nil)
	_, err := svc.Create(remoteInput())
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrNoSuchRemoteEmoji)
}

// **name と host を取り違えていないこと (レビュー M5)。**
//
// 入れ替えると本番では全リモート申請が NO_SUCH_EMOJI になるが、偽物が引数を
// 握り潰していると気付けない。非対称な値を与えて突き合わせる。
func TestCreateRemotePassesArgumentsInOrder(t *testing.T) {
	emojis := &fakeEmojis{
		remote:     &model.Emoji{ID: "e-remote"},
		remoteName: "sushi_remote",
		remoteHost: "example.com",
	}
	svc := NewService(newFakeApps(), emojis, &okFiles{}, &fixedID{}, nil, nil)

	_, err := svc.Create(remoteInput())
	require.NoError(t, err, "name と host が入れ替わっている")
}

// リモートではライセンスを必須にしない (レビュー M1)。
func TestCreateRemoteAllowsEmptyLicense(t *testing.T) {
	emojis := &fakeEmojis{remote: &model.Emoji{ID: "e1"}}
	in := remoteInput()
	in.License = ""

	app, err := NewService(newFakeApps(), emojis, &okFiles{}, &fixedID{}, nil, nil).Create(in)
	require.NoError(t, err, "リモートでライセンスが必須になっている")
	require.Equal(t, "", app.License)
}

// 自作では引き続き必須。
func TestCreateOwnStillRequiresLicense(t *testing.T) {
	in := validInput()
	in.License = ""
	_, err := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).Create(in)
	require.ErrorIs(t, err, ErrLicenseRequired)
}

// stubPolicies serves a fixed effective-policy map (#2958).
type stubPolicies struct {
	p     map[string]any
	calls []string
}

func (s *stubPolicies) GetUserPolicies(userID string) map[string]any {
	s.calls = append(s.calls, userID)
	return s.p
}

// **ポリシーを 3 つとも窓へ渡すこと (#2958)。** どれか 1 つを取り違えると、
// その期間の上限だけが黙って効かなくなる。
func TestCreateBuildsQuotaWindowsFromPolicies(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	pol := &stubPolicies{p: map[string]any{
		"emojiApplicationMaxPerDay":   3,
		"emojiApplicationMaxPerWeek":  float64(10),
		"emojiApplicationMaxPerMonth": int64(20),
	}}
	svc.SetPolicyProvider(pol)

	_, err := svc.Create(validInput())
	require.NoError(t, err)

	require.Equal(t, 1, apps.quotaCalls)
	require.Equal(t, []repository.QuotaWindow{
		{Name: "day", Duration: 24 * time.Hour, Max: 3},
		{Name: "week", Duration: 7 * 24 * time.Hour, Max: 10},
		{Name: "month", Duration: 30 * 24 * time.Hour, Max: 20},
	}, apps.quotaLimits.Windows)
	// **上限は申請者のロールで決まる。** 別人の ID で引くと、権限の弱い人が
	// 強い人の枠を使える。
	require.Equal(t, []string{"u1"}, pol.calls)
}

// **未配線なら上限を掛けない (#2958)。** 掛けられないのに掛けたつもりに
// なるより、既定 (0 = 無制限) と同じ挙動に倒す。配線の欠落は criticalWiring
// が起動時に落とす。
func TestCreateWithoutPolicyProviderAppliesNoQuota(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	require.False(t, svc.HasPolicyProvider())

	_, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Equal(t, 1, apps.quotaCalls)
	require.Nil(t, apps.quotaLimits.Windows)
}

// ポリシーが引けなかったときも上限を掛けない (resolvePolicies は fail-soft で
// nil を返しうる)。
func TestCreateWithNilPoliciesAppliesNoQuota(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: nil})

	_, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Nil(t, apps.quotaLimits.Windows)
	require.Zero(t, apps.quotaLimits.MaxPending)
}

// **0 以下・未設定・読めない値は無制限に倒す。** ここを上限として扱うと、
// 既定値のままのインスタンスで申請が全て塞がる。
func TestCreateTreatsNonPositivePoliciesAsUnlimited(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want int
	}{
		{"zero", 0, 0},
		{"negative", -1, 0},
		{"missing", nil, 0},
		{"string", "5", 0},
		// **小数は切り捨てる。** 切り上げると運営者の意図より 1 件多く通る。
		{"fraction", 2.9, 2},
		{"fraction below one", 0.9, 0},
		{"positive", 4, 4},
		// **int の幅を超える値で桁溢れさせない。** 32bit 環境で負に回ると
		// 上限 0 = 無制限に化ける。
		{"absurdly large", 1e18, math.MaxInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apps := newFakeApps()
			svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
			svc.SetPolicyProvider(&stubPolicies{p: map[string]any{
				"emojiApplicationMaxPerDay": tc.val,
			}})

			_, err := svc.Create(validInput())
			require.NoError(t, err)
			require.Equal(t, tc.want, apps.quotaLimits.Windows[0].Max)
		})
	}
}

// **上限超過は専用の型で返すこと (#2958)。** 汎用エラーに潰すと API 層が 500 に
// 倒し、利用者にはいつ空くかが伝わらない。
func TestCreateTranslatesQuotaExceeded(t *testing.T) {
	retryAt := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	apps := newFakeApps()
	apps.quotaErr = &repository.QuotaExceededError{
		Window: repository.QuotaWindow{Name: "week", Duration: 7 * 24 * time.Hour, Max: 5},
		// **Used と Limit を別の値にする。** 同じ値だと取り違えたまま緑になる
		// (同時実行で上限を跨いだときは Used > Limit になりうる)。
		Used:    7,
		RetryAt: retryAt,
	}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{"emojiApplicationMaxPerWeek": 5}})

	_, err := svc.Create(validInput())
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, "week", qe.Period)
	require.Equal(t, 7, qe.Used)
	require.Equal(t, 5, qe.Limit)
	require.Equal(t, retryAt, qe.RetryAt)
}

// 上限とは無関係な失敗を上限超過に化けさせない。
func TestCreateKeepsOtherQuotaPathErrors(t *testing.T) {
	apps := newFakeApps()
	apps.quotaErr = errors.New("db down")
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)

	_, err := svc.Create(validInput())
	require.EqualError(t, err, "db down")
	var qe *QuotaExceededError
	require.False(t, errors.As(err, &qe))
}

// 番兵の文面が「上限に達した」と読めること。errors.As で分岐しない
// 呼び出し元 (ログ) が唯一の手がかりにする。
func TestQuotaExceededErrorMessage(t *testing.T) {
	err := &QuotaExceededError{Period: "day", Used: 4, Limit: 3}
	require.Contains(t, err.Error(), "quota")
}

// 審査待ちの重複は上限経路でも番兵のまま通すこと。
func TestCreateKeepsDuplicatePendingThroughQuotaPath(t *testing.T) {
	apps := newFakeApps()
	apps.quotaErr = repository.ErrEmojiApplicationDuplicatePending
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)

	_, err := svc.Create(validInput())
	require.ErrorIs(t, err, ErrAlreadyPending)
}

// **審査待ちの上限もポリシーから引くこと (#2977)。** 期間上限と同じ経路で
// 渡さないと、設定しても効かないまま気付けない。
func TestCreateBuildsPendingLimitFromPolicies(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{
		"emojiApplicationMaxPending": 4,
	}})

	_, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Equal(t, 4, apps.quotaLimits.MaxPending)
}

// 未配線・ポリシーなしなら審査待ちの上限も掛けない。
func TestCreateWithoutPolicyProviderAppliesNoPendingLimit(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)

	_, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Zero(t, apps.quotaLimits.MaxPending)
}

// **審査待ちの上限は専用の型で返すこと (#2977)。** 期間上限と同じ型に潰すと、
// API 層が 429 と `Retry-After` を付けてしまい「待てば通る」と誤解させる。
func TestCreateTranslatesPendingLimitExceeded(t *testing.T) {
	apps := newFakeApps()
	apps.quotaErr = &repository.PendingLimitExceededError{Used: 5, Limit: 3}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{"emojiApplicationMaxPending": 3}})

	_, err := svc.Create(validInput())
	var pe *PendingLimitExceededError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, 5, pe.Used)
	require.Equal(t, 3, pe.Limit)
	// 期間上限とは別の型であること (API 層の分岐がこれに依存する)。
	var qe *QuotaExceededError
	require.False(t, errors.As(err, &qe))
}

// 番兵の文面が審査待ちを指すこと。
func TestPendingLimitExceededErrorMessage(t *testing.T) {
	err := &PendingLimitExceededError{Used: 3, Limit: 3}
	require.Contains(t, err.Error(), "pending")
}

// **allowlist は関数越しに公開する (#2959)。** 公開 mutable な map にすると
// どこからでも書き換えられ、申請の検証を実行時に緩められる。
func TestAllowedImageTypesReturnsACopy(t *testing.T) {
	got := AllowedImageTypes()
	require.NotEmpty(t, got)
	require.True(t, sort.StringsAreSorted(got), "順序が安定しないとゲートの診断が毎回変わる")
	for _, mime := range got {
		require.True(t, IsAllowedImageType(mime), "%s が allowlist と食い違っている", mime)
	}

	// **2 回呼んで別インスタンスであることを見る。** `IsAllowedImageType` は
	// map を見るので、slice への書き込みが届くことはどんな実装でもありえない
	// = 反証不能なアサーションだった。package 変数を使い回す実装 (呼び出し側
	// から破壊できる) を検出できず、テスト名の `ReturnsACopy` が空虚だった。
	before := AllowedImageTypes()
	got[0] = "mutated"
	after := AllowedImageTypes()
	require.Equal(t, before, after, "呼び出し側の書き換えが次の呼び出しに残っている")
	require.NotContains(t, after, "mutated")
}

// **申請時点の画像ハッシュを載せること (#2960)。** 載らないと、審査画面の
// 「同じ画像の過去申請」が静かに効かなくなる。症状は「関連なし」と出るだけで、
// 正常時と区別が付かない。
func TestCreateSnapshotsFileHash(t *testing.T) {
	owner := "u1"
	apps := newFakeApps()
	files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png", MD5: "d41d8cd98f00b204e9800998ecf8427e"}}
	svc := NewService(apps, &fakeEmojis{}, files, &fixedID{}, nil, nil)

	app, err := svc.Create(validInput())
	require.NoError(t, err)
	require.NotNil(t, app.FileHash, "ハッシュが載っていない")
	require.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", *app.FileHash)
	// 保存された行にも載っていること (返り値だけ埋めても意味が無い)。
	require.NotNil(t, apps.rows[app.ID].FileHash)
	require.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", *apps.rows[app.ID].FileHash)
}

// **空は載せない (#2960)。** drive が md5 を持たない行を空文字で保存すると、
// 空同士が「同じ画像」として一致し、無関係な履歴がまとめて並ぶ。
func TestCreateOmitsEmptyFileHash(t *testing.T) {
	owner := "u1"
	apps := newFakeApps()
	files := &okFiles{file: &model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png", MD5: ""}}
	svc := NewService(apps, &fakeEmojis{}, files, &fixedID{}, nil, nil)

	app, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Nil(t, app.FileHash, "空のハッシュを載せている")
}

// リモート絵文字の申請には drive のファイルが無いので、ハッシュも載らない。
func TestCreateRemoteHasNoFileHash(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{remote: &model.Emoji{ID: "e1"}}, &okFiles{}, &fixedID{}, nil, nil)

	in := validInput()
	in.Kind = model.EmojiApplicationKindRemote
	in.FileID = ""
	in.RemoteHost = "remote.example"
	in.RemoteName = "kusa"
	app, err := svc.Create(in)
	require.NoError(t, err)
	require.Nil(t, app.FileHash)
}

// fakeResets is the #2962 quota reset store.
type fakeResets struct {
	rows      []model.EmojiApplicationQuotaReset
	latestErr error
	createErr error
	created   []model.EmojiApplicationQuotaReset
	calls     int
	onCreate  func()
}

func (f *fakeResets) Create(row *model.EmojiApplicationQuotaReset) error {
	if f.onCreate != nil {
		f.onCreate()
	}
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, *row)
	f.rows = append(f.rows, *row)
	return nil
}

func (f *fakeResets) LatestByUser(userID string) (*model.EmojiApplicationQuotaReset, error) {
	f.calls++
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	var out *model.EmojiApplicationQuotaReset
	for i := range f.rows {
		if f.rows[i].UserID != userID {
			continue
		}
		if out == nil || f.rows[i].CreatedAt.After(out.CreatedAt) {
			out = &f.rows[i]
		}
	}
	return out, nil
}

// errBoom stands in for any repository failure (#2961).
var errBoom = errors.New("boom")

// **ロールの上限をそのまま窓へ渡すこと (#2961)。** 画面が出す「3 / 5」は
// ここで組んだ窓が元なので、取り違えるとモデレーターが別の期間の数字を見て
// 判断する。作成側 (#2958) と同じ policy から組む。
func TestUserSummaryBuildsWindowsFromPolicies(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{
		"emojiApplicationMaxPerDay":   3,
		"emojiApplicationMaxPerWeek":  float64(10),
		"emojiApplicationMaxPerMonth": int64(20),
	}})

	got, err := svc.UserSummary("u1")
	require.NoError(t, err)
	require.Equal(t, "u1", apps.usageUserID, "別のユーザーを数えている")
	// **時計を渡していること (レビュー L2)。** 渡さないと repository が
	// `time.Now()` に落ちるので production では等価だが、注入した時計が
	// 効かないとテストで窓の境界を作れない。
	require.False(t, apps.usageNow.IsZero(), "現在時刻が渡っていない")
	require.Len(t, apps.usageWindows, 3)
	require.Equal(t, "day", apps.usageWindows[0].Name)
	require.Equal(t, 3, apps.usageWindows[0].Max)
	require.Equal(t, 24*time.Hour, apps.usageWindows[0].Duration)
	require.Equal(t, "week", apps.usageWindows[1].Name)
	require.Equal(t, 10, apps.usageWindows[1].Max)
	require.Equal(t, 7*24*time.Hour, apps.usageWindows[1].Duration)
	require.Equal(t, "month", apps.usageWindows[2].Name)
	require.Equal(t, 20, apps.usageWindows[2].Max)
	require.Equal(t, 30*24*time.Hour, apps.usageWindows[2].Duration)

	require.Len(t, got.Windows, 3)
	require.Equal(t, []string{"day", "week", "month"},
		[]string{got.Windows[0].Period, got.Windows[1].Period, got.Windows[2].Period})
	require.Equal(t, 3, got.Windows[0].Limit, "上限が出力に載っていない")
}

// 件数と使用状況がそのまま出ること。
func TestUserSummaryReportsCountsAndUsage(t *testing.T) {
	apps := newFakeApps()
	apps.userCounts = repository.StatusCounts{Total: 7, Pending: 1, Approved: 2, Rejected: 3, Canceled: 1}
	at := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	apps.usage = []repository.QuotaWindowUsage{
		{Window: repository.QuotaWindow{Name: "day", Max: 5}, Used: 5, RetryAt: at},
		{Window: repository.QuotaWindow{Name: "week", Max: 20}, Used: 8},
		{Window: repository.QuotaWindow{Name: "month", Max: 0}, Used: 24},
	}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)

	got, err := svc.UserSummary("u1")
	require.NoError(t, err)
	require.Equal(t, "u1", apps.countUserID)
	require.Equal(t, apps.userCounts, got.Counts)
	require.Len(t, got.Windows, 3)
	// **満杯の窓だけ時刻が載る。** 空きのある窓に時刻が出ると「今は出せない」
	// と読める。
	require.Equal(t, 5, got.Windows[0].Used)
	require.Equal(t, at, got.Windows[0].RetryAt)
	require.True(t, got.Windows[1].RetryAt.IsZero(), "空きのある窓に次回可能時刻が入っている")
	// **上限なしでも件数は出す。** 0 件と「無制限」を混同させない。
	require.Equal(t, 24, got.Windows[2].Used)
	require.Equal(t, 0, got.Windows[2].Limit)
}

// **policy が引けなくても窓を返すこと (#2961)。** 窓が消えると画面は
// 「期間上限の設定が無い」と描くので、上限が効いているのに無いと見える。
func TestUserSummaryWithoutPolicyProviderStillReturnsWindows(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)

	got, err := svc.UserSummary("u1")
	require.NoError(t, err)
	require.Len(t, got.Windows, 3, "policy が無いと窓が消えている")
	require.Equal(t, []string{"day", "week", "month"},
		[]string{got.Windows[0].Period, got.Windows[1].Period, got.Windows[2].Period})
	for i := range got.Windows {
		require.Equal(t, 0, got.Windows[i].Limit, "上限なしとして返っていない")
	}
	// **期間まで見る。** 取り違えると、policy が引けないときだけ「7日間」の
	// 欄に 24 時間ぶんの件数が出る (数字はもっともらしいので気付けない)。
	require.Len(t, apps.usageWindows, 3)
	require.Equal(t, []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour},
		[]time.Duration{
			apps.usageWindows[0].Duration,
			apps.usageWindows[1].Duration,
			apps.usageWindows[2].Duration,
		}, "既定の窓の期間が違う")
}

// **審査待ちの上限も渡すこと (レビュー H1)。** 渡さないと repository の
// 「両方満杯なら時刻を落とす」規則が働かず、**その時刻に叩いても通らない時刻**を
// 画面に広告する。窓に空きがあっても審査待ちが満杯なら申請は通らないので、
// 件数そのものも出力に載せる。
func TestUserSummaryPassesPendingLimit(t *testing.T) {
	apps := newFakeApps()
	apps.usagePending = 2
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{
		"emojiApplicationMaxPerDay":  10,
		"emojiApplicationMaxPending": 2,
	}})

	got, err := svc.UserSummary("u1")
	require.NoError(t, err)
	require.Equal(t, 2, apps.usageMaxPending, "審査待ちの上限が repository に渡っていない")
	require.Equal(t, 2, got.Pending, "審査待ちの件数が出力に載っていない")
	require.Equal(t, 2, got.MaxPending, "審査待ちの上限が出力に載っていない")
}

// **障害を握り潰さない。** 0 件として描くと、実際には申請があるユーザーを
// 「履歴なし」と判断する。件数と使用状況は別々に検査する。
func TestUserSummarySurfacesErrors(t *testing.T) {
	countFail := newFakeApps()
	countFail.countErr = errBoom
	_, err := NewService(countFail, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).UserSummary("u1")
	require.ErrorIs(t, err, errBoom, "件数の取得失敗を握り潰している")

	usageFail := newFakeApps()
	usageFail.usageErr = errBoom
	_, err = NewService(usageFail, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).UserSummary("u1")
	require.ErrorIs(t, err, errBoom, "使用状況の取得失敗を握り潰している")
}

// **リセットの境界が窓へ渡ること (#2962)。** 渡らないと、戻したはずの枠が
// 戻らない (作成側) / 画面が古い件数を出す (読み取り側)。
func TestQuotaLimitsCarriesResetBoundary(t *testing.T) {
	apps := newFakeApps()
	resets := &fakeResets{}
	at := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	resets.rows = []model.EmojiApplicationQuotaReset{
		{ID: "r1", UserID: "u1", ResetByID: "m1", Reason: "古い", CreatedAt: at.Add(-time.Hour)},
		{ID: "r2", UserID: "u1", ResetByID: "m2", Reason: "新しい", CreatedAt: at},
		{ID: "r3", UserID: "other", ResetByID: "m1", Reason: "別人", CreatedAt: at.Add(time.Hour)},
	}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(resets)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{"emojiApplicationMaxPerDay": 3}})

	_, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Equal(t, at, apps.quotaLimits.ResetAt, "最新のリセットが窓へ渡っていない")
}

// **未配線なら「リセット無し」に倒す (fail-closed)。** あったことにすると
// 枠が勝手に広がる。
func TestQuotaLimitsWithoutResetRepoIsZero(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{"emojiApplicationMaxPerDay": 3}})

	_, err := svc.Create(validInput())
	require.NoError(t, err)
	require.True(t, apps.quotaLimits.ResetAt.IsZero(), "未配線でリセットが効いている")
}

// **引けなかったときも「リセット無し」に倒す。** 読めないときに枠を広げると、
// DB 障害のたびに上限が消える。
func TestQuotaLimitsToleratesResetLookupFailure(t *testing.T) {
	apps := newFakeApps()
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(&fakeResets{latestErr: errBoom})
	svc.SetPolicyProvider(&stubPolicies{p: map[string]any{"emojiApplicationMaxPerDay": 3}})

	_, err := svc.Create(validInput())
	require.NoError(t, err, "リセットが引けないだけで申請が落ちている")
	require.True(t, apps.quotaLimits.ResetAt.IsZero(), "引けなかったのにリセットが効いている")
}

// **リセット前の使用状況を返すこと (#2962)。** 監査ログに「何件使っていた人を
// 戻したか」を残すためで、後から採ると必ず 0 になる。
func TestResetQuotaReturnsUsageBeforeReset(t *testing.T) {
	apps := newFakeApps()
	apps.usage = []repository.QuotaWindowUsage{
		{Window: repository.QuotaWindow{Name: "day", Max: 5}, Used: 5},
		{Window: repository.QuotaWindow{Name: "week", Max: 20}, Used: 8},
	}
	resets := &fakeResets{}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(resets)

	// **順序を固定する。** 使用状況を後から採ると必ず 0 になり、監査ログの
	// 「何件使っていた人を戻したか」が意味を失う。fake の戻り値は固定なので、
	// 呼ばれた順そのものを見ないとこの取り違えを検出できない (実測)。
	seq, usageSeq, createSeq := 0, 0, 0
	apps.onUsage = func() { seq++; usageSeq = seq }
	resets.onCreate = func() { seq++; createSeq = seq }

	before, row, err := svc.ResetQuota("u1", "mod1", "  修正後の画像で再申請してもらうため  ")
	require.NoError(t, err)
	require.Equal(t, 5, before.Windows[0].Used, "リセット前の使用状況になっていない")
	require.Equal(t, 8, before.Windows[1].Used)

	require.NotNil(t, row)
	require.Equal(t, "u1", row.UserID)
	require.Equal(t, "mod1", row.ResetByID)
	// 前後の空白は落とす (空白だけの理由を弾くのと同じ扱い)。
	require.Equal(t, "修正後の画像で再申請してもらうため", row.Reason)
	require.Len(t, resets.created, 1, "リセットが記録されていない")
	require.Greater(t, usageSeq, 0, "使用状況を採っていない")
	require.Less(t, usageSeq, createSeq, "リセットを記録してから使用状況を採っている (必ず 0 になる)")
	require.False(t, row.CreatedAt.IsZero(), "リセット時刻が入っていない")
}

// **理由は必須。** 監査ログに残る唯一の文脈。
func TestResetQuotaRequiresReason(t *testing.T) {
	resets := &fakeResets{}
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(resets)

	for _, reason := range []string{"", "   ", "\t\n"} {
		_, _, err := svc.ResetQuota("u1", "mod1", reason)
		require.ErrorIs(t, err, ErrResetReasonRequired, "理由 %q が通っている", reason)
	}
	require.Empty(t, resets.created, "弾いたのに記録が残っている")
}

// 未配線なら明示的に失敗する (黙って何もしない、にしない)。
func TestResetQuotaWithoutRepoFails(t *testing.T) {
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	_, _, err := svc.ResetQuota("u1", "mod1", "理由")
	require.ErrorIs(t, err, ErrQuotaResetUnavailable)
}

// 記録に失敗したら握り潰さない (成功したように見せない)。
func TestResetQuotaSurfacesCreateFailure(t *testing.T) {
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(&fakeResets{createErr: errBoom})
	_, _, err := svc.ResetQuota("u1", "mod1", "理由")
	require.ErrorIs(t, err, errBoom)
}

// **`UserSummary` は最後のリセットを 1 回だけ引くこと (#2962)。** 境界に使う値と
// 画面に出す値を別々に引くと、その間にリセットが入ったときに食い違う。
func TestUserSummaryReadsResetOnce(t *testing.T) {
	apps := newFakeApps()
	resets := &fakeResets{}
	at := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	resets.rows = []model.EmojiApplicationQuotaReset{
		{ID: "r1", UserID: "u1", ResetByID: "m1", Reason: "理由", CreatedAt: at},
	}
	svc := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(resets)

	got, err := svc.UserSummary("u1")
	require.NoError(t, err)
	require.NotNil(t, got.LastReset, "最後のリセットが返っていない")
	require.Equal(t, "r1", got.LastReset.ID)
	require.Equal(t, at, apps.usageResetAt, "境界と表示で別の値を使っている")
	require.Equal(t, 1, resets.calls, "リセットを 2 回引いている (間に操作が入ると食い違う)")
}

// **列の長さを超える理由は弾くこと (レビュー M1)。** そのまま DB へ渡すと
// SQLSTATE 22001 が生のまま返り、**利用者の入力で 5xx が立つ** — 画面は
// 「もう一度お試しください」と案内するが、何度やっても同じ結果になる。
func TestResetQuotaRejectsTooLongReason(t *testing.T) {
	resets := &fakeResets{}
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(resets)

	// 1024 文字ちょうどは通る (列の幅と同じ)。
	_, _, err := svc.ResetQuota("u1", "mod1", strings.Repeat("あ", 1024))
	require.NoError(t, err, "列の幅ちょうどで弾いている")

	_, _, err = svc.ResetQuota("u1", "mod1", strings.Repeat("あ", 1025))
	require.ErrorIs(t, err, ErrTooLong)
	require.Len(t, resets.created, 1, "弾いたのに記録が残っている")
}

// **配線の述語を固定する (レビュー L2)。** これは起動時の自己診断
// (`criticalWiring`) が読む値で、`return true` に潰れると「nil 相当の値を
// 渡した構成」を実行時にも検出できなくなる (静的ゲートは `nil` リテラルしか
// 見ない)。sibling の `HasPolicyProvider` と同じ扱い。
func TestHasQuotaResetRepoReflectsWiring(t *testing.T) {
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	require.False(t, svc.HasQuotaResetRepo(), "未配線なのに配線済みと報告している")
	svc.SetQuotaResetRepo(&fakeResets{})
	require.True(t, svc.HasQuotaResetRepo())
}

// **書き込みが障害で失敗したときも作ったものを片付けること (#2966 / レビュー M1)。**
// 残すと、申請は pending のままなのに絵文字だけ存在するので、もう一度承認を
// 押すと自分がさっき作った絵文字が重複として当たり、その申請は二度と承認
// できない。取り残した絵文字が複製した drive ファイルを孤児 cleanup から
// 守るので、ストレージも回収されない。
func TestApproveCleansUpWhenUpdateFails(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.updateErr = errBoom
	creator := &fakeCreator{id: "e-orphan", driveFileID: "sys-orphan"}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, []CreatedEmoji{{EmojiID: "e-orphan", DriveFileID: "sys-orphan"}}, creator.deleted,
		"書き込みに失敗したのに作ったものが残っている (その申請は二度と承認できない)")
}

// **書けていたなら消さない。** 稀に「commit は通ったが応答が返らなかった」ことが
// あり、そのとき消すと承認済みの申請が存在しない絵文字を指す。
func TestApproveKeepsCreatedWhenApprovalLanded(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.updateErr = errBoom
	// 書き込み自体は通っていた形にする。
	apps.updateLands = true
	creator := &fakeCreator{id: "e-landed", driveFileID: "sys-landed"}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.ErrorIs(t, err, errBoom)
	require.Empty(t, creator.deleted, "承認が通っていたのに作ったものを消している")
}

// **読み直せなかったら消す側に倒す (2 周目レビュー M3)。** 判定の方針そのもの
// なので固定する。残す側に倒すと、DB が読めない間に押した承認がすべて
// 「申請は pending のまま絵文字だけ存在する」状態を積み、その申請は次から
// `DUPLICATE_NAME` で二度と承認できなくなる。消す側なら、承認をもう一度
// 押せば作り直せる。
func TestApproveCleansUpWhenReadBackFails(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.updateErr = errBoom
	// Approve 自身の読み取り (1 回目) は通し、approvalLanded の読み直し
	// (2 回目) だけ落とす。
	apps.findErrAfter = 2
	creator := &fakeCreator{id: "e-unknown", driveFileID: "sys-unknown"}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.Error(t, err)
	require.Equal(t, []CreatedEmoji{{EmojiID: "e-unknown", DriveFileID: "sys-unknown"}}, creator.deleted,
		"書けたか確かめられないのに作ったものを残している")
}

// **載っているのが他人の承認なら、自分が作ったものは消す (2 周目レビュー M3)。**
// 先に別のモデレーターの承認が載った状態で「approved だから消さない」と判断すると、
// 自分が作った絵文字と複製が誰からも参照されないまま残り、しかも取り残した絵文字が
// 複製を孤児 cleanup から守るのでストレージも回収されない。local emoji は
// `host IS NULL` なので一意制約が効かず (Postgres は NULL を distinct 扱い)、
// 同名の行が 2 つできる形は実在する。
func TestApproveCleansUpWhenAnotherApprovalLanded(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.updateErr = errBoom
	other := "e-other"
	apps.updateLandsAs = &model.EmojiApplication{
		ID: "a1", UserID: "u1", Status: model.EmojiApplicationApproved, EmojiID: &other,
	}
	creator := &fakeCreator{id: "e-mine", driveFileID: "sys-mine"}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, []CreatedEmoji{{EmojiID: "e-mine", DriveFileID: "sys-mine"}}, creator.deleted,
		"載っているのは他人の承認なのに自分が作ったものを残している")
}

// **status が pending のままなら消す (2 周目レビュー M3)。** emojiId だけ
// 一致していても承認は載っていない。残すと上と同じ孤児になる。
func TestApproveCleansUpWhenRowStillPending(t *testing.T) {
	apps := newFakeApps()
	apps.rows["a1"] = &model.EmojiApplication{ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending}
	apps.updateErr = errBoom
	mine := "e-mine"
	apps.updateLandsAs = &model.EmojiApplication{
		ID: "a1", UserID: "u1", Status: model.EmojiApplicationPending, EmojiID: &mine,
	}
	creator := &fakeCreator{id: "e-mine", driveFileID: "sys-mine"}
	svc := newService(t, apps, &fakeEmojis{}, creator)

	_, err := svc.Approve(context.Background(), "a1", "mod1")
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, []CreatedEmoji{{EmojiID: "e-mine", DriveFileID: "sys-mine"}}, creator.deleted,
		"申請が pending のままなのに作ったものを残している")
}
