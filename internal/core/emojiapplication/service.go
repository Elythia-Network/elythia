// Package emojiapplication implements the lifecycle of custom emoji
// registration requests (#2934).
//
// 承認までは `emoji` 行を作らない。詳細は migration/000086 と
// internal/model/emoji_application.go を参照。
package emojiapplication

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Errors returned by Service. 呼び出し側 (API 層) が HTTP の形へ落とす。
var (
	// ErrInvalidName is returned when the requested name is not usable.
	ErrInvalidName = errors.New("invalid emoji name")
	// ErrLicenseRequired is returned when the license field is empty.
	ErrLicenseRequired = errors.New("license is required")
	// ErrFileRequired is returned when an "own" application has no file.
	ErrFileRequired = errors.New("file is required")
	// ErrInvalidKind is returned when the requested kind is not recognised.
	ErrInvalidKind = errors.New("invalid application kind")
	// ErrRemoteRequired is returned when a "remote" application has no host/name.
	ErrRemoteRequired = errors.New("remote host and name are required")
	// ErrRemoteFetchFailed is returned when the emoji image could not be stored.
	ErrRemoteFetchFailed = errors.New("failed to fetch remote emoji image")
	// ErrNoSuchRemoteEmoji is returned when the referenced remote emoji is not
	// known to this instance.
	//
	// **リモート絵文字の行はキャッシュに近い。** 使われなくなると消えるので、
	// 申請の時点で引けることを確かめる (審査時にもう一度取り直す)。
	ErrNoSuchRemoteEmoji = errors.New("no such remote emoji")
	// ErrDuplicateName is returned when a local emoji already uses the name.
	ErrDuplicateName = errors.New("duplicate emoji name")
	// ErrAlreadyPending is returned when the applicant already has a pending
	// application under the same name.
	ErrAlreadyPending = errors.New("application already pending")
	// ErrNotFound is returned when the application does not exist.
	ErrNotFound = errors.New("no such application")
	// ErrNotPending is returned when the application was already processed.
	ErrNotPending = errors.New("application is not pending")
	// ErrForbidden is returned when the caller does not own the application.
	ErrForbidden = errors.New("not the applicant")
	// ErrUnsupportedFileType is returned when the drive file is not an image
	// type the emoji pipeline accepts.
	ErrUnsupportedFileType = errors.New("unsupported file type")
	// ErrTooLong is returned when a field does not fit its column.
	//
	// 列に入らない値をそのまま DB へ渡すと、長さ超過は SQLSTATE 22001、NUL は
	// 22021 (本番の pgx extended protocol) が生で返り、利用者の入力で 5xx が立つ。
	// **長さだけではない** — 判定は `colfit.Fits` で、NUL もここに落ちる (#3022)。
	ErrTooLong = errors.New("field is too long")
	// ErrForeignFile is returned when the drive file belongs to someone else.
	//
	// **他人のファイルを指定させない (#2934 レビュー H2)。** 許すと (a) 応答に
	// 含まれる URL から他人のファイルを読める (`/files/:accessKey` は認証なしの
	// GET で、URL そのものが capability)、(b) 承認すると他人の画像がサーバーの
	// 絵文字として登録される。`core/user` の applyMediaUpdate が avatar / banner
	// に対して同じ検証をしている。
	ErrForeignFile = errors.New("drive file belongs to another user")
	// ErrImageCopyFailed is returned when the approved image could not be
	// duplicated into a system-owned drive file (#2966).
	//
	// **`ErrFileGone` に潰さない。** ストレージ障害や DB 障害を「そんな
	// ファイルは無い」(400) に化けさせると、モデレーターには却下すべき申請に
	// 見え、監視でも 5xx が立たない (#2792 が禁じている形)。リモート経路が
	// `ErrRemoteFetchFailed` を 500 に倒しているのと揃える。
	ErrImageCopyFailed = errors.New("failed to copy emoji image")
	// ErrImageURLTooLong is returned when the stored image URL does not fit the
	// emoji columns (#3023)。
	//
	// `drive_file.url` は varchar(1024) だが `emoji.originalUrl` / `publicUrl` は
	// varchar(512) で、長い prefix のオブジェクトストレージ構成では超えうる。
	// 通すと `Create` が SQLSTATE 22001 で落ち、承認が 5xx になる。
	ErrImageURLTooLong = errors.New("emoji image URL is too long to store")
	// ErrImageTooLarge is returned when the approved image exceeds the copy
	// limit (#2966).
	//
	// **利用者に伝わる形にする。** drive が受け取れる大きさと複製の上限が
	// 食い違うと、承認だけが恒久的に失敗する。原因が分かる文面を返す。
	ErrImageTooLarge = errors.New("emoji image is too large to register")
	// ErrFileGone is returned when the drive file backing the request cannot be
	// used: deleted between applying and approving, or an id that cannot exist.
	//
	// 申請者が drive から消せるので普通に起きる。500 にしない。**列に入らない
	// `fileId` もここに落ちる** (#3022) — 存在しえない id なので「無い」と同じ。
	ErrFileGone = errors.New("drive file is gone")
)

// MaxEmojiCopyBytes caps the local drive file duplicated on approval (#2966).
//
// **申請側と承認側で同じ値を見る。** ここが食い違うと「申請はできたのに承認
// だけが恒久的に失敗する」サイズ帯が生まれる (2 周目レビュー M2)。drive が
// 受け取る上限は role policy の `maxFileSizeMb` (既定 30) が決めていて、
// これより上へ設定できる。
//
// **リモート取得の上限 (8 MiB) を流用しない。** あちらは相手サーバーが
// いくらでも送れるので低く抑えているが、こちらは自分の drive が既に受け取った
// ファイル。既定の 30 MB に余裕を足した値にする。
const MaxEmojiCopyBytes int64 = 32 << 20

// namePattern mirrors the constraint upstream's admin/emoji/add enforces.
//
// **申請側で先に弾く。** 承認まで通してから登録で落ちると、モデレーターが
// 押した後でエラーになり、申請者にも審査者にも何も残らない。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// The accepted set mirrors upstream FILE_TYPE_IMAGE (const.ts).
//
// **prefix 判定 ("image/") にしない。** それでは `image/svg+xml` を通してしまい、
// 絵文字は本文中にそのまま埋め込まれるので XSS になる。admin 側の
// `isAllowedEmojiImageType` と**同じ集合を共有する** — 申請側が prefix、承認側が
// allowlist という食い違いがあると、申請は通るのに承認で必ず落ちる形になり、
// 「押した後にエラーになるのを無くす」という申請時検証の目的が svg でだけ
// 達成できない (レビュー R3)。
// IsAllowedImageType reports whether the MIME type may back a custom emoji.
//
// **map を公開しない (レビュー Low 8)。** 公開 mutable な map だと、どこからでも
// 書き換えられる。関数越しにすれば集合の共有はそのままで、書き換えの経路が消える。
func IsAllowedImageType(mime string) bool { return allowedImageTypes[mime] }

// AllowedImageTypes returns the accepted MIME types.
//
// **コピーを返す。** 公開 mutable な map にすると、どこからでも書き換えられる
// (`IsAllowedImageType` を関数越しにしたのと同じ理由)。fork frontend の
// ドロップ判定 (#2959) と突き合わせるゲートが使う。
func AllowedImageTypes() []string {
	out := make([]string, 0, len(allowedImageTypes))
	for mime := range allowedImageTypes {
		out = append(out, mime)
	}
	sort.Strings(out)
	return out
}

var allowedImageTypes = map[string]bool{
	"image/png":    true,
	"image/gif":    true,
	"image/jpeg":   true,
	"image/webp":   true,
	"image/avif":   true,
	"image/apng":   true,
	"image/bmp":    true,
	"image/tiff":   true,
	"image/x-icon": true,
}

// maxAliases caps how many aliases a single application may carry.
//
// 検索用の別名で、際限なく受けると emoji 行の配列がそのまま膨らむ。
const maxAliases = 16

// 列幅は migration/000086_emoji_application (と 000089 の quota reset) の定義に
// 対応する。変えるときは DDL と揃えること — 突き合わせは
// `internal/repository/emoji_application_column_limits_test.go`。
//
// **判定は `colfit.Fits` に寄せる (#3022)。** 長さと NUL を 1 つの述語で見ないと、
// 片方だけ足した状態に戻りやすい。NUL は長さに関わらず列に入らず
// (本番の pgx extended protocol では SQLSTATE 22021)、**同じ書き込みに乗っている
// 他の列まで巻き添えになる**ので、通すと利用者の入力で 5xx が立つ。
const (
	applicationNameMaxRunes         = 128
	applicationCategoryMaxRunes     = 128
	applicationAliasMaxRunes        = 128
	applicationLicenseMaxRunes      = 1024
	applicationCommentMaxRunes      = 2048
	applicationRemoteHostMaxRunes   = 128
	applicationRemoteNameMaxRunes   = 128
	applicationRejectReasonMaxRunes = 2048
	applicationFileIDMaxRunes       = 32
	applicationIDMaxRunes           = 32
	// `emoji_application_quota_reset.reason` (migration/000089)。
	quotaResetReasonMaxLen = 1024
)

// FitsApplicationID reports whether id can be stored in `emoji_application.id`.
//
// **列に入らない id は引く前に弾く (#3022)。** NUL を含む id を SELECT のパラメータに
// 載せるとその時点で落ち、`IsNotFound` でもないので 500 になる。実在する id は必ず
// この述語を通る (列が varchar(32) で、NUL は text 型に入らない) ので、弾かれるのは
// **存在しえない id だけ**。
//
// サービス層を通らずリポジトリを直叩きする経路 (`admin/emoji-application/related`)
// からも同じ規則を使えるように公開している。
func FitsApplicationID(id string) bool {
	return colfit.Fits(id, applicationIDMaxRunes)
}

// IDGenerator issues new row IDs.
type IDGenerator interface {
	Generate(t time.Time) string
}

// EmojiCreator turns an approved application into a real emoji row.
//
// **admin/emoji/add と同じ経路を通すための穴。** ここで独自に emoji を作ると、
// MIME の allowlist や webpublic variant の優先といった検証が申請経路だけ
// 抜ける — 承認が「検証を迂回して絵文字を登録する方法」になってしまう。
// 実装は admin handler 側に置き、既存の追加処理と 1 本にまとめる。
type EmojiCreator interface {
	// CreateFromApplication registers the emoji and returns what it created.
	//
	// **drive ファイルの id も返す (#2966)。** 承認時に画像を system 所有の
	// drive ファイルとして複製するので、競合に負けたときは絵文字と一緒に
	// それも片付ける必要がある。返さないと孤児が残る。
	CreateFromApplication(ctx context.Context, app *model.EmojiApplication) (CreatedEmoji, error)
	// DeleteCreatedEmoji removes what CreateFromApplication created for an
	// application that then lost the race to another moderator.
	//
	// **作ったものを片付ける口 (レビュー R5)。** 承認が条件付き UPDATE に
	// 負けると、申請は「却下」なのに絵文字だけ登録済みで使える状態が残る。
	// M1 前の「絵文字があるのに申請は却下」と症状が同じで、確率が下がっただけ。
	DeleteCreatedEmoji(ctx context.Context, created CreatedEmoji) error
}

// CreatedEmoji is what an approval created (#2966).
type CreatedEmoji struct {
	EmojiID string
	// DriveFileID は承認時に作った system 所有の drive ファイル。空のことも
	// ある (リモート経路で取り込みが未配線のとき、既存データの経路)。
	DriveFileID string
}

// ResultNotifier tells the applicant that their request was processed.
//
// 通知は副作用なので失敗しても審査自体は成立させる (呼び出し側で握る)。
type ResultNotifier interface {
	NotifyEmojiApplicationProcessed(ctx context.Context, app *model.EmojiApplication) error
}

// SetReceivedNotifier wires the reviewer notification emitted on Create (#2987).
// Optional — nil leaves申請の受付を通知しない (既存の挙動)。
func (s *Service) SetReceivedNotifier(n ReceivedNotifier) { s.receivedNotifier = n }

// DriveFileLookup resolves the drive file backing an "own" application.
type DriveFileLookup interface {
	FindByID(id string) (*model.DriveFile, error)
}

// EmojiLookup is the only thing this package needs from the emoji repository.
//
// **広い interface を取らない。** repository.EmojiRepository は CRUD を一式
// 持つが、ここで要るのは重複判定だけ。狭く取ると、テストの偽物が本物の面を
// 全部埋める必要が無くなり、依存の実態も読んで分かる。
type EmojiLookup interface {
	FindByNameAndHost(name string, host *string) (*model.Emoji, error)
}

// Service owns the state transitions of emoji applications.
type Service struct {
	apps     repository.EmojiApplicationRepository
	emojis   EmojiLookup
	files    DriveFileLookup
	idGen    IDGenerator
	creator  EmojiCreator
	notifier ResultNotifier
	// receivedNotifier は申請の受付を審査できる人へ知らせる (#2987)。
	// 未配線なら通知を出さない (既存の挙動)。
	receivedNotifier ReceivedNotifier
	nowFunc          func() time.Time
	// policies は申請の期間上限を引く先 (#2958)。**未配線なら上限を掛けない** —
	// 掛けられないのに掛けたつもりになると、ロールを設定した運営者が
	// 「効いている」と誤解する。既定値も 0 (無制限) なので挙動は変わらない。
	policies role.PolicyProvider
	// resets は枠の手動リセット (#2962) を読み書きする先。**未配線なら
	// リセットは無かったものとして数える** — 配線を忘れた構成で枠が勝手に
	// 広がるより、従来どおり厳しい側に倒れるほうが安全。
	resets repository.EmojiApplicationQuotaResetRepository
}

// SetPolicyProvider wires the role policy source used for the rolling quota.
func (s *Service) SetPolicyProvider(p role.PolicyProvider) { s.policies = p }

// HasPolicyProvider reports whether the quota can be enforced.
func (s *Service) HasPolicyProvider() bool { return s.policies != nil }

// SetQuotaResetRepo wires the manual quota reset store (#2962).
func (s *Service) SetQuotaResetRepo(r repository.EmojiApplicationQuotaResetRepository) {
	s.resets = r
}

// HasQuotaResetRepo reports whether the manual quota reset is available.
//
// **起動時の自己診断に載せるため (レビュー M2)。** 静的ゲートは
// `SetQuotaResetRepo(` という呼び出しの存在しか見ないので、`nil` を渡す形は
// 素通りする。未配線だとリセットは 500 で失敗し続け、**過去に戻した枠も
// 読めなくなる** (再び満杯に見える)。
func (s *Service) HasQuotaResetRepo() bool { return s.resets != nil }

// QuotaExceededError is returned when a rolling window is full (#2958).
// 呼び出し元はこれを HTTP 429 に翻訳し、期間・使用数・上限・再試行時刻を返す。
type QuotaExceededError struct {
	Period string
	Used   int
	Limit  int
	// RetryAt はゼロ値のことがある (#2977)。審査待ちの上限も同時に満杯だと
	// その時刻でも通らないので、時刻を予告できない。
	RetryAt time.Time
}

func (e *QuotaExceededError) Error() string { return "emoji application quota exceeded" }

// PendingLimitExceededError is returned when too many applications from the
// same user are still awaiting review (#2977).
//
// **再試行時刻を持たない。** 空くのはモデレーターが処理したときなので予告
// できない。呼び出し元はこれを 400 系へ翻訳し、`Retry-After` を付けない。
type PendingLimitExceededError struct {
	Used  int
	Limit int
}

func (e *PendingLimitExceededError) Error() string {
	return "emoji application pending limit exceeded"
}

// quotaLimits builds every per-user limit from the user's role policies.
//
// **ローリング期間にする。** 固定暦だとタイムゾーン依存になり、切り替わりの
// 直前と直後に連続で申請できてしまう。
func (s *Service) quotaLimits(userID string) repository.QuotaLimits {
	limits, _ := s.quotaLimitsWithReset(userID)
	return limits
}

// quotaLimitsWithReset builds the limits and also returns the reset row they
// were built from.
//
// **リセットは 1 回だけ引く (#2962)。** 境界に使う値と画面に出す値を別々に
// 引くと、その間にリセットが入ったときに**「最後のリセット」として表示した時刻と、
// 実際に数えた境界が食い違う**。
func (s *Service) quotaLimitsWithReset(userID string) (repository.QuotaLimits, *model.EmojiApplicationQuotaReset) {
	reset := s.lastReset(userID)
	resetAt := time.Time{}
	if reset != nil {
		resetAt = reset.CreatedAt
	}
	if s.policies == nil {
		// policy が無くてもリセットの境界は要る — 上限が 0 (無制限) でも、
		// 件数の表示 (#2961) はリセット後の数であるべき。
		return repository.QuotaLimits{ResetAt: resetAt}, reset
	}
	// **errorless 版を使う。** ロール解決に失敗すると base が返る。base には
	// `meta.policies` (管理画面のベースロール) まで載っているので、そちらに
	// 上限を入れていればそれが効き、入れていなければ既定の 0 = 無制限に
	// 倒れる。他の policy consumer と揃えた fail-soft で、DB 全断ならこの
	// 直後の INSERT も落ちる。
	p := s.policies.GetUserPolicies(userID)
	if p == nil {
		return repository.QuotaLimits{ResetAt: resetAt}, reset
	}
	// 窓は狭い順に並べてある。**ただし repository 側は満杯のものを全部評価して
	// いちばん遅く空くものを返す**ので、順序は結果を変えない (読みやすさのため)。
	windows := defaultQuotaWindows()
	for i := range windows {
		windows[i].Max = policyMax(p, quotaWindowDefs[i].policyKey)
	}
	return repository.QuotaLimits{
		Windows: windows,
		// **審査待ちの上限には効かない (#2962)。** あれは「今まさに審査待ちの
		// 件数」で、枠を戻しても申請は審査待ちのまま残るので意味を持たない。
		MaxPending: policyMax(p, role.PolicyEmojiApplicationMaxPending),
		ResetAt:    resetAt,
	}, reset
}

// quotaResetReasonMaxLen mirrors `emoji_application_quota_reset.reason`
// (varchar(1024)).

// ErrResetReasonRequired is returned when a quota reset carries no reason.
//
// **監査ログに残る唯一の文脈なので必須。** 空を許すと「誰かが戻した」以上の
// ことが後から分からない。
var ErrResetReasonRequired = errors.New("emoji application quota reset reason is required")

// ErrQuotaResetUnavailable is returned when the reset store is not wired.
var ErrQuotaResetUnavailable = errors.New("emoji application quota reset is not available")

// ResetQuota clears the user's rolling quota by recording a reset (#2962).
//
// **申請の行は触らない。** 履歴を消して枠を空けると、過去の判断 (#2960 が
// 審査の材料として出している却下理由や同じ画像かどうか) も同時に消える。
//
// 戻り値は**リセット前**の使用状況。監査ログに「何件使っていた人を戻したか」を
// 残すために呼び出し元が使う — 後から採ると必ず 0 になり、記録として意味が無い。
func (s *Service) ResetQuota(userID, moderatorID, reason string) (UserSummary, *model.EmojiApplicationQuotaReset, error) {
	if s.resets == nil {
		return UserSummary{}, nil, ErrQuotaResetUnavailable
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return UserSummary{}, nil, ErrResetReasonRequired
	}
	// **列の長さを超える入力を DB に渡さない (レビュー M1)。** 渡すと
	// SQLSTATE 22001 が生のまま返り、**利用者の入力で 5xx が立つ** — 画面は
	// 「もう一度お試しください」と案内するが、何度やっても同じ結果になる。
	// このパッケージが license に対して既に採っている扱いと揃える。
	if !colfit.Fits(reason, quotaResetReasonMaxLen) {
		return UserSummary{}, nil, ErrTooLong
	}

	// **先に読む。** 書いてから読むと、返すのはリセット後の 0 件になる。
	before, err := s.UserSummary(userID)
	if err != nil {
		return UserSummary{}, nil, err
	}

	row := &model.EmojiApplicationQuotaReset{
		ID:        s.idGen.Generate(s.nowFunc()),
		UserID:    userID,
		ResetByID: moderatorID,
		Reason:    reason,
		CreatedAt: s.nowFunc(),
	}
	if err := s.resets.Create(row); err != nil {
		return UserSummary{}, nil, err
	}
	return before, row, nil
}

// lastReset returns the most recent manual reset, or nil.
//
// **引けなかったら nil に倒す (fail-closed)。** リセットを「あった」ことに
// すると枠が勝手に広がる。読めないときは従来どおり厳しい側で数える。
func (s *Service) lastReset(userID string) *model.EmojiApplicationQuotaReset {
	if s.resets == nil {
		return nil
	}
	row, err := s.resets.LatestByUser(userID)
	if err != nil {
		slog.Warn("emoji application: quota reset lookup failed",
			"userId", userID, "err", err)
		return nil
	}
	return row
}

// UserSummary is the moderation-screen view of one user's applications (#2961).
type UserSummary struct {
	Counts repository.StatusCounts
	// Windows は期間上限の使用状況。**上限なしの窓も落とさない** — 「無制限」
	// であることも審査の材料なので、返さずに画面側で補うと 0 件と区別できない。
	Windows []QuotaWindowUsage
	// Pending / MaxPending は審査待ちの上限 (#2977) の使用状況。
	//
	// **窓だけでは足りない (レビュー H1)。** 審査待ちが満杯なら、期間の窓に
	// 空きがあっても申請は 400 で弾かれる。これを出さないと画面は
	// 「24時間: 2 / 10 (空きあり)」と描き、**実際には出せない人を出せると
	// 案内する** — この機能が塞ごうとしている失敗形そのもの。
	Pending    int
	MaxPending int
	// LastReset は最後に枠を手動で戻した操作 (#2962)。未実施なら nil。
	LastReset *model.EmojiApplicationQuotaReset
}

// QuotaWindowUsage is one window's usage for the moderation screen (#2961).
type QuotaWindowUsage struct {
	Period string
	Used   int
	// Limit は 0 なら無制限。**0 を「上限 0 件」と読ませない**ため、API 側は
	// `unlimited` を別に立てる。
	Limit int
	// RetryAt は満杯の窓が空く時刻。空きがあればゼロ値。
	RetryAt time.Time
}

// UserSummary collects the status breakdown and the rolling-window usage.
//
// **使用状況は作成側と同じ計算を使う (repository.QuotaUsage)。** 画面用に
// 数え直すと「空きありと出ているのに弾かれる」という形でずれる。
func (s *Service) UserSummary(userID string) (UserSummary, error) {
	counts, err := s.apps.CountByUserStatus(userID)
	if err != nil {
		return UserSummary{}, err
	}
	limits, reset := s.quotaLimitsWithReset(userID)
	// **policy が引けなくても窓は返す。** 窓が消えると画面は「期間上限の設定が
	// 無い」と描くので、上限が効いているのに無いと見える。上限 0 = 無制限として
	// 出すほうが、少なくとも件数は正しい。
	windows := limits.Windows
	if len(windows) == 0 {
		windows = defaultQuotaWindows()
	}
	usage, err := s.apps.QuotaUsage(userID, repository.QuotaLimits{
		Windows: windows,
		// **`ResetAt` を落とさない (#2962)。** 組み直すときに落とすと、画面だけ
		// リセット前の件数を出し続ける (作成側は戻っているのに画面は満杯)。
		ResetAt: limits.ResetAt,
		// **審査待ちの上限も渡す。** 渡さないと、両方満杯のときに
		// repository が `RetryAt` を落とす規則が働かず、**その時刻に叩いても
		// 通らない時刻**を画面に広告することになる (レビュー H1)。
		MaxPending: limits.MaxPending,
	}, s.nowFunc())
	if err != nil {
		return UserSummary{}, err
	}
	out := UserSummary{
		Counts:     counts,
		Windows:    make([]QuotaWindowUsage, 0, len(usage.Windows)),
		Pending:    usage.Pending,
		MaxPending: usage.MaxPending,
		LastReset:  reset,
	}
	for _, u := range usage.Windows {
		out.Windows = append(out.Windows, QuotaWindowUsage{
			Period:  u.Window.Name,
			Used:    u.Used,
			Limit:   u.Window.Max,
			RetryAt: u.RetryAt,
		})
	}
	return out, nil
}

// quotaWindowDefs is the single definition of the rolling windows (#2958 /
// #2961).
//
// **一覧を 2 つ持たない (レビュー L1)。** policy が引けるときと引けないときで
// 別々に書いていたので、片方だけ足すと画面の行数や期間が食い違う。
var quotaWindowDefs = []struct {
	name      string
	duration  time.Duration
	policyKey string
}{
	{"day", 24 * time.Hour, role.PolicyEmojiApplicationMaxPerDay},
	{"week", 7 * 24 * time.Hour, role.PolicyEmojiApplicationMaxPerWeek},
	{"month", 30 * 24 * time.Hour, role.PolicyEmojiApplicationMaxPerMonth},
}

// defaultQuotaWindows returns the windows with no limit applied.
func defaultQuotaWindows() []repository.QuotaWindow {
	out := make([]repository.QuotaWindow, 0, len(quotaWindowDefs))
	for _, d := range quotaWindowDefs {
		out = append(out, repository.QuotaWindow{Name: d.name, Duration: d.duration})
	}
	return out
}

// policyMax reads one numeric quota policy (期間の窓と審査待ちの両方で使う)。
// 0 以下・未設定・読めない値はすべて「無制限」(0) に倒す。
//
// **小数は切り捨てる。** `PolicyNumber` は 2.5 のような値をそのまま返すので、
// 切り上げると運営者が意図した上限より 1 件多く通る。
func policyMax(p map[string]any, key string) int {
	v, ok := role.PolicyNumber(p[key])
	if !ok || v <= 0 {
		return 0
	}
	if v > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int(math.Floor(v))
}

// NewService constructs the service. creator / notifier may be nil in tests and
// in configurations that do not wire them.
func NewService(
	apps repository.EmojiApplicationRepository,
	emojis EmojiLookup,
	files DriveFileLookup,
	idGen IDGenerator,
	creator EmojiCreator,
	notifier ResultNotifier,
) *Service {
	return &Service{
		apps:     apps,
		emojis:   emojis,
		files:    files,
		idGen:    idGen,
		creator:  creator,
		notifier: notifier,
		nowFunc:  time.Now,
	}
}

// CreateInput is the applicant-supplied part of a new application.
type CreateInput struct {
	UserID string
	// Kind is "own" (default) or "remote".
	Kind        string
	Name        string
	Category    string
	Aliases     []string
	License     string
	IsSensitive bool
	FileID      string
	// RemoteHost / RemoteName identify the emoji to import when Kind == remote.
	RemoteHost string
	RemoteName string
	Comment    string
}

// Create records a new pending application.
func (s *Service) Create(in CreateInput) (*model.EmojiApplication, error) {
	name := strings.TrimSpace(in.Name)
	// name は ASCII のみ (namePattern) なので文字数 = バイト数だが、数え方を
	// 揃えておく。**NUL は pattern が既に弾く**が、`colfit.Fits` を通しておくと
	// pattern を緩めたときに穴が開かない (テストで両方を固定してある)。
	if name == "" || !colfit.Fits(name, applicationNameMaxRunes) || !namePattern.MatchString(name) {
		return nil, ErrInvalidName
	}
	license := strings.TrimSpace(in.License)
	// **列に入らない入力を DB に渡さない (レビュー M8 / #3022)。** 渡すと
	// SQLSTATE 22001 (長さ) / 22021 (NUL) が生のまま返り、利用者の入力で 5xx が立つ。
	//
	// **文字数で数える (レビュー R4)。** varchar(N) は文字数だが len() は
	// バイト数なので、バイトで見ると日本語は列の約 1/3 しか使えず、正当な
	// 入力が 400 になる。`colfit.Fits` が rune で数え、NUL も同時に落とす。
	if !colfit.Fits(license, applicationLicenseMaxRunes) {
		return nil, ErrTooLong
	}
	if !colfit.Fits(strings.TrimSpace(in.Category), applicationCategoryMaxRunes) ||
		!colfit.Fits(strings.TrimSpace(in.Comment), applicationCommentMaxRunes) {
		return nil, ErrTooLong
	}
	// **素材の検証だけが kind で分かれる。** 名前・ライセンス・長さ・重複は
	// 共通で、ここから下だけが「自作画像」と「リモート絵文字」で違う。
	// どちらも**申請の時点で**検証する — 承認まで通してから落ちると、
	// モデレーターが押した後にエラーになり、申請者にも審査者にも何も残らない。
	kind := in.Kind
	if kind == "" {
		kind = model.EmojiApplicationKindOwn
	}

	var fileID, remoteHost, remoteName, fileHash string
	switch kind {
	case model.EmojiApplicationKindOwn:
		fileID = strings.TrimSpace(in.FileID)
		if fileID == "" {
			return nil, ErrFileRequired
		}
		// **id も列に入るかを見る (#3022)。** `remoteHost` / `remoteName` と同じで、
		// **書き込みより前に SELECT で使う** — NUL を含む id を渡すと
		// `FindByID` がその時点で落ち、`IsNotFound` でもないので 500 になる。
		// 存在しえない id なので「そんなファイルは無い」で返す。
		if !colfit.Fits(fileID, applicationFileIDMaxRunes) {
			return nil, ErrFileGone
		}
		// 所有権は security の問題で、MIME は体験の問題。
		var err error
		if fileHash, err = s.checkFile(fileID, in.UserID); err != nil {
			return nil, err
		}
	case model.EmojiApplicationKindRemote:
		remoteHost = strings.TrimSpace(in.RemoteHost)
		remoteName = strings.TrimSpace(in.RemoteName)
		if remoteHost == "" || remoteName == "" {
			return nil, ErrRemoteRequired
		}
		// **書き込みより前に SELECT で使う** (`checkRemoteEmoji`) ので、NUL が
		// あると照会の時点で落ちる (#3022)。
		if !colfit.Fits(remoteHost, applicationRemoteHostMaxRunes) ||
			!colfit.Fits(remoteName, applicationRemoteNameMaxRunes) {
			return nil, ErrTooLong
		}
		if err := s.checkRemoteEmoji(remoteName, remoteHost); err != nil {
			return nil, err
		}
	default:
		return nil, ErrInvalidKind
	}

	// **ライセンスの必須は自作のときだけ (レビュー M1)。** リモートは取り込み元
	// (host + name) 自体が出典で、相手の絵文字が `_misskey_license` を持っていれば
	// 承認時にそれが入る。申請者は `fetch-remote-meta` を叩けないので、参照できない
	// 値の記入を必須にすると「それらしい嘘」を書かせることになる。
	//
	// **kind を確定させてから見る (レビュー Low 4)。** 正規化前の `in.Kind` で
	// 判定していると、未知の kind + ライセンス空が ErrInvalidKind ではなく
	// ErrLicenseRequired になり、診断が「ライセンスを書け」と事実と違うことを指す。
	if license == "" && kind != model.EmojiApplicationKindRemote {
		return nil, ErrLicenseRequired
	}

	// **既に同名の絵文字があるなら受け付けない。** 受けてしまうと、審査で
	// 承認を押した瞬間に DUPLICATE_NAME で落ちる。押す前に分かるほうがよい。
	// DB 障害を「重複なし」に丸めない (#2792)。
	existing, err := s.emojis.FindByNameAndHost(name, nil)
	if err != nil && !repository.IsNotFound(err) {
		return nil, err
	}
	if err == nil && existing != nil {
		return nil, ErrDuplicateName
	}

	now := s.nowFunc()
	app := &model.EmojiApplication{
		ID:          s.idGen.Generate(now),
		UserID:      in.UserID,
		Kind:        kind,
		Status:      model.EmojiApplicationPending,
		Name:        name,
		Aliases:     normalizeAliases(in.Aliases),
		License:     license,
		IsSensitive: in.IsSensitive,
		Comment:     optionalString(in.Comment),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if cat := strings.TrimSpace(in.Category); cat != "" {
		app.Category = &cat
	}
	if fileID != "" {
		app.FileID = &fileID
	}
	// **空は載せない。** drive が md5 を持たない行 (古い取り込み等) を空文字で
	// 保存すると、空同士が「同じ画像」として一致してしまう。
	if fileHash != "" {
		app.FileHash = &fileHash
	}
	if remoteHost != "" {
		app.RemoteHost = &remoteHost
		app.RemoteName = &remoteName
	}

	// **数えるのと作るのを 1 つのトランザクションでやる (#2958)。** COUNT →
	// INSERT に分けると、同じ利用者から同時に来たリクエストが両方とも
	// 「空きあり」を読んで両方通る。
	if err := s.apps.CreateWithQuota(app, s.quotaLimits(in.UserID)); err != nil {
		if errors.Is(err, repository.ErrEmojiApplicationDuplicatePending) {
			return nil, ErrAlreadyPending
		}
		var qe *repository.QuotaExceededError
		if errors.As(err, &qe) {
			return nil, &QuotaExceededError{
				Period: qe.Window.Name, Used: qe.Used, Limit: qe.Window.Max, RetryAt: qe.RetryAt,
			}
		}
		var pe *repository.PendingLimitExceededError
		if errors.As(err, &pe) {
			return nil, &PendingLimitExceededError{Used: pe.Used, Limit: pe.Limit}
		}
		return nil, err
	}
	// **申請は永続化済みなので、通知の失敗で申請を失わせない。** 呼び出し元の
	// ctx も使わない — クライアントが切断しても審査側への通知は出す必要がある
	// (abuseReport #2868 と同じ判断)。
	if s.receivedNotifier != nil {
		if err := s.receivedNotifier.NotifyEmojiApplicationReceived(context.Background(), app); err != nil {
			slog.Warn("emoji-application: notify reviewers failed", "application", app.ID, "err", err)
		}
	}
	return app, nil
}

// checkFile resolves the drive file, asserts the applicant owns a usable image,
// and returns its MD5 for the snapshot (#2960).
//
// **userId が NULL のものも拒否する。** 未紐付けのファイルは誰のものとも
// 言えないので、申請の素材にはしない (applyMediaUpdate と同じ判断)。
//
// **ハッシュもここで返す。** 審査画面の関連履歴 (#2960) で使うが、そのために
// drive をもう一度引くと申請 1 件あたりのクエリが増える。ここは既に引いている。
func (s *Service) checkFile(fileID, userID string) (string, error) {
	if s.files == nil {
		// 未配線の構成では検証できない。**通さない** — 検証していないものを
		// 通すと、配線を落とした瞬間に穴が開く (fail-closed)。
		return "", ErrFileGone
	}
	f, err := s.files.FindByID(fileID)
	if err != nil {
		if repository.IsNotFound(err) {
			return "", ErrFileGone
		}
		// DB 障害を not-found に丸めない (#2792)。
		return "", err
	}
	if f.UserID == nil || *f.UserID != userID {
		// **「他人のもの」とは答えない。** 区別できると、ファイル ID の
		// 存在確認に使える。存在しないのと同じ応答にする。
		return "", ErrFileGone
	}
	if !IsAllowedImageType(f.Type) {
		return "", ErrUnsupportedFileType
	}
	// **承認時に複製できない大きさは申請の時点で断る (2 周目レビュー M2)。**
	// 承認側は実体を読んで複製するので上限があるが、drive が受け取る上限
	// (role policy の `maxFileSizeMb`) はそれより上げられる。見ないと、申請は
	// 通るのに承認だけが `EMOJI_IMAGE_TOO_LARGE` で永久に失敗する — 申請者には
	// 直しようが無く、モデレーターには却下すべき申請に見える。
	if int64(f.Size) > MaxEmojiCopyBytes {
		return "", ErrImageTooLarge
	}
	return f.MD5, nil
}

// checkRemoteEmoji asserts the referenced remote emoji is known to this instance.
//
// **リモート絵文字の行はキャッシュに近い。** 使われなくなると消えるので、
// 申請の時点で引けることを確かめる。承認時にはもう一度取り直すので、審査を
// 待つ間に消えても申請自体は無意味にならない (だから `emojiId` ではなく
// host + name で持つ)。
func (s *Service) checkRemoteEmoji(name, host string) error {
	e, err := s.emojis.FindByNameAndHost(name, &host)
	if err != nil {
		if repository.IsNotFound(err) {
			return ErrNoSuchRemoteEmoji
		}
		// DB 障害を not-found に丸めない (#2792)。
		return err
	}
	if e == nil {
		return ErrNoSuchRemoteEmoji
	}
	return nil
}

// Approve registers the emoji and closes the application.
//
// **emoji を作ってから申請を閉じる。** 逆順にすると、作成に失敗したときに
// 「承認済みなのに絵文字が無い」申請が残り、押し直す手段も無くなる。
func (s *Service) Approve(ctx context.Context, id, moderatorID string) (*model.EmojiApplication, error) {
	app, err := s.pending(id)
	if err != nil {
		return nil, err
	}
	if s.creator == nil {
		return nil, errors.New("emojiapplication: creator is not wired")
	}

	created, err := s.creator.CreateFromApplication(ctx, app)
	if err != nil {
		return nil, err
	}

	now := s.nowFunc()
	app.Status = model.EmojiApplicationApproved
	app.EmojiID = &created.EmojiID
	app.ProcessedByID = &moderatorID
	app.ProcessedAt = &now
	app.UpdatedAt = now
	// **条件付きで書く。** 読んでから書くまでの間に他のモデレーターが処理して
	// いたら、上書きせずに ErrNotPending を返す (レビュー M1)。
	ok, err := s.apps.UpdateIfPending(app)
	if err != nil {
		// **障害で書けなかったときも作ったものを片付ける (レビュー M1)。**
		// 残すと、申請は `pending` のままなのに絵文字だけ存在するので、もう一度
		// 承認を押すと**自分がさっき作った絵文字**が重複として当たり
		// `DUPLICATE_NAME` になる — その申請は二度と承認できない。しかも
		// 取り残した絵文字が複製した drive ファイルを孤児 cleanup から守って
		// しまうので、ストレージも回収されない。
		//
		// **書けていたかどうかを先に確かめる。** 稀に「commit は通ったが応答が
		// 返らなかった」ことがあり、そのとき消すと承認済みの申請が存在しない
		// 絵文字を指す。読み直して自分の承認が載っていれば何も消さない。
		if !s.approvalLanded(app.ID, created.EmojiID) {
			if derr := s.creator.DeleteCreatedEmoji(ctx, created); derr != nil {
				slog.Warn("emojiapplication: 書き込みに失敗した承認の後始末に失敗した",
					"applicationId", app.ID, "emojiId", created.EmojiID,
					"driveFileId", created.DriveFileID, "err", derr)
			}
		}
		return nil, err
	}
	if !ok {
		// **負けたら作った emoji を片付ける (レビュー R5)。** 残すと、申請は
		// 「却下」で通知も却下なのに絵文字だけ使える状態になる。削除に失敗しても
		// 審査の結果は変わらないので、ErrNotPending をそのまま返す。
		// **drive ファイルも一緒に消す (#2966)。** 絵文字だけ消すと、複製した
		// system 所有のファイルが誰からも参照されないまま残る。
		if derr := s.creator.DeleteCreatedEmoji(ctx, created); derr != nil {
			slog.Warn("emojiapplication: 競合で負けた承認の後始末に失敗した",
				"applicationId", app.ID, "emojiId", created.EmojiID,
				"driveFileId", created.DriveFileID, "err", derr)
		}
		return nil, ErrNotPending
	}
	s.notify(ctx, app)
	return app, nil
}

// approvalLanded reports whether the approval actually reached the DB despite
// UpdateIfPending returning an error (#2966 / レビュー M1)。
//
// **読めなければ「載っていない」に倒す。** 確かめられないまま残すと、申請は
// 二度と承認できないうえ複製したファイルも回収されない。消す側に倒すと、稀な
// ケースで「承認済みなのに絵文字が無い」になるが、そちらは画面から見えて手で
// 直せる。黙って詰むより良い。
func (s *Service) approvalLanded(appID, emojiID string) bool {
	cur, err := s.apps.FindByID(appID)
	if err != nil || cur == nil {
		return false
	}
	return cur.Status == model.EmojiApplicationApproved &&
		cur.EmojiID != nil && *cur.EmojiID == emojiID
}

// Reject closes the application without registering anything.
func (s *Service) Reject(ctx context.Context, id, moderatorID, reason string) (*model.EmojiApplication, error) {
	// **却下理由も列に入るかを見る (#3022)。** `rejectReason` は varchar(2048) で、
	// ここは長さも NUL も見ていなかった。モデレーターの入力だが、落ちると
	// **押した却下が保存されない**まま 500 になる。
	// **保存されるのは trim 後** (`optionalString`) なので、そちらで判定する。
	// 生の値で見ると、末尾の改行だけで上限ちょうどの理由が弾かれる。
	if !colfit.Fits(strings.TrimSpace(reason), applicationRejectReasonMaxRunes) {
		return nil, ErrTooLong
	}
	app, err := s.pending(id)
	if err != nil {
		return nil, err
	}
	now := s.nowFunc()
	app.Status = model.EmojiApplicationRejected
	app.ProcessedByID = &moderatorID
	app.ProcessedAt = &now
	app.RejectReason = optionalString(reason)
	app.UpdatedAt = now
	ok, err := s.apps.UpdateIfPending(app)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotPending
	}
	s.notify(ctx, app)
	return app, nil
}

// Cancel lets the applicant withdraw their own pending application.
func (s *Service) Cancel(id, userID string) error {
	// **所有者の確認を pending の判定より先に行う (レビュー M2)。** 逆順だと、
	// 他人の「処理済み」申請に対して ErrNotPending が返り、存在しない ID
	// (ErrNotFound) と区別できてしまう — ID 列挙のオラクルになる。呼び出し側が
	// ErrForbidden を 404 に潰しても、この順序でなければ塞がらない。
	//
	// **列に入らない id は引く前に弾く (#3022)。** 存在しえない id なので
	// not-found で返す — 所有者の確認より前だが、**申請の存在にも所有者にも
	// 依存しない判定**なので上の列挙オラクルは開かない (実在する id は列に入る
	// 以上、必ずこの述語を通る)。
	if !FitsApplicationID(id) {
		return ErrNotFound
	}
	app, err := s.apps.FindByID(id)
	if err != nil {
		if repository.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	if app.UserID != userID {
		return ErrForbidden
	}
	if !app.IsPending() {
		return ErrNotPending
	}
	now := s.nowFunc()
	app.Status = model.EmojiApplicationCanceled
	app.UpdatedAt = now
	ok, err := s.apps.UpdateIfPending(app)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotPending
	}
	return nil
}

// pending loads an application and asserts it still awaits review.
func (s *Service) pending(id string) (*model.EmojiApplication, error) {
	// **列に入らない id は引く前に弾く (#3022)。** NUL を含む id を渡すと
	// SELECT がその時点で落ち、`IsNotFound` でもないので 500 になる
	// (`fileId` と同じ形)。存在しえない id なので not-found で返す。
	if !FitsApplicationID(id) {
		return nil, ErrNotFound
	}
	app, err := s.apps.FindByID(id)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !app.IsPending() {
		return nil, ErrNotPending
	}
	return app, nil
}

// notify is best-effort: 通知に失敗しても審査の結果は確定している。
func (s *Service) notify(ctx context.Context, app *model.EmojiApplication) {
	if s.notifier == nil {
		return
	}
	_ = s.notifier.NotifyEmojiApplicationProcessed(ctx, app)
}

// normalizeAliases trims, drops empties and duplicates, and caps the count.
func normalizeAliases(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		// 列に入らない要素は落とす。**切らない** — 切ると別の名前になり、
		// リアクションの照合に使えないものが混ざる (#3018 / #3022)。
		if a == "" || !colfit.Fits(a, applicationAliasMaxRunes) {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
		if len(out) >= maxAliases {
			break
		}
	}
	return out
}

func optionalString(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}
