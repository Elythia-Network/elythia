package repository

import (
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
)

// Emoji application filter values accepted by EmojiApplicationRepository.List.
//
// Declared as plain string constants so callers (admin handlers, testutil
// mocks) can pass literals without depending on this package's type system.
const (
	// EmojiApplicationFilterAll returns every application.
	EmojiApplicationFilterAll = "all"
	// EmojiApplicationFilterPending returns applications awaiting review.
	EmojiApplicationFilterPending = "pending"
	// EmojiApplicationFilterProcessed returns applications that no longer need
	// action (approved / rejected / canceled).
	EmojiApplicationFilterProcessed = "processed"
)

// ErrEmojiApplicationDuplicatePending is returned when the applicant already
// has a pending application under the same name.
//
// **番兵で区別する。** 一意制約違反をそのまま 500 にすると、利用者には
// 「サーバーエラー」としか見えず、二重送信なのか障害なのか分からない。
var ErrEmojiApplicationDuplicatePending = errors.New("emoji application already pending for this name")

// EmojiApplicationRepository reads and writes `emoji_application` rows (#2934).
type EmojiApplicationRepository interface {
	// Create bypasses the rolling windows. **申請の作成は CreateWithQuota を
	// 使うこと (#2958)。** こちらは上限が 1 つも設定されていないときの委譲先
	// とテストの seed 用。
	Create(app *model.EmojiApplication) error
	FindByID(id string) (*model.EmojiApplication, error)
	// List returns applications newest first. filter is one of the
	// EmojiApplicationFilter* constants; anything else is treated as "all".
	List(filter string, limit int, untilID string) ([]model.EmojiApplication, error)
	ListByUser(userID string, limit int, untilID string) ([]model.EmojiApplication, error)
	CountPending() (int64, error)
	// UpdateIfPending writes the row only while it is still pending, and
	// reports whether it did.
	//
	// **条件付き UPDATE でないと last-write-wins になる (#2934 レビュー M1)。**
	// 読んでから書くだけだと、2 人のモデレーターが同じ申請を開いていた場合に
	// 後の書き込みが前を上書きする — 承認で emoji を作った直後に却下が被さり、
	// **絵文字は存在するのに申請は「却下」で emojiId も消える**。窓は「一覧を
	// 開いてから押すまで」の全期間なので、実際に起きる。
	UpdateIfPending(app *model.EmojiApplication) (bool, error)
	// CreateWithQuota inserts the row only while every limit still has room:
	// the rolling windows (#2958) and the awaiting-review cap (#2977).
	//
	// **COUNT してから INSERT では足りない。** 同じ利用者から同時に来た
	// リクエストが両方とも「空きあり」を読んで両方 INSERT できる。数えるのも
	// 作るのも 1 つのトランザクションに入れ、**利用者単位のアドバイザリロック**
	// で直列化する (行ロックでは「まだ無い行」を守れない)。
	//
	// limits の内訳は QuotaLimits を参照。収まらなければ
	// *QuotaExceededError / *PendingLimitExceededError を返し、行は作らない。
	CreateWithQuota(app *model.EmojiApplication, limits QuotaLimits) error
	// FindRelated returns past applications that look like the given one (#2960).
	//
	// **審査の材料。** 同じ名前・同じリモート元・同じ画像で過去に出された
	// 申請を、却下理由ごとモデレーターに見せる。**自動拒否には使わない** —
	// ライセンスの変更・画像の修正・運用方針の変更がありうる。
	//
	// 自分自身は含めない。ページングは id の降順 + untilID。
	FindRelated(app *model.EmojiApplication, limit int, untilID string) ([]RelatedApplication, error)
	// CountRelated returns how many past applications match, by status (#2960).
	//
	// **一覧では呼ばない。** 全行ぶん引くと N+1 になるので、申請の詳細を
	// 開いたときだけ 1 回呼ぶ。
	CountRelated(app *model.EmojiApplication) (RelatedCounts, error)
	// ListByUserFiltered returns one user's applications for the moderation
	// screen (#2961), newest first.
	//
	// status は "" / "all" で全件、それ以外は model.EmojiApplication* の
	// いずれか。**未知の値は全件に倒さない** — 絞ったつもりで全部出るほうが
	// 危険側なので 0 件を返す。
	//
	// query は名前 / remoteHost / remoteName の部分一致。**LIKE のメタ文字は
	// エスケープする** — 素通しすると `foo_bar` が `fooXbar` に当たり、`%` の
	// 1 文字で全件返る。
	ListByUserFiltered(userID, status, query string, limit int, untilID string) ([]model.EmojiApplication, error)
	// CountByUserStatus breaks one user's applications down by status (#2961).
	CountByUserStatus(userID string) (StatusCounts, error)
	// QuotaUsage reports how much of every per-user limit is in use (#2961).
	//
	// **作成側と同じ計算を使う。** 別の SQL で組み直すと、画面が「空きあり」と
	// 言っているのに実際は弾かれる、という形でずれる。満杯の窓には `RetryAt`
	// が入るが、**審査待ちも満杯なら時刻は落ちる** — その時刻に叩いてもまだ
	// 通らないため (#2977 と同じ規則を共有する)。
	QuotaUsage(userID string, limits QuotaLimits, now time.Time) (QuotaUsage, error)
}

// RelatedApplication is one past application plus why it matched (#2960).
type RelatedApplication struct {
	model.EmojiApplication
	// MatchedName / MatchedRemote / MatchedHash は SQL 側で判定する。
	//
	// **WHERE と同じ式を使う。** Go 側で組み直すと、片方だけ直したときに
	// 「一致したのに理由が空」「理由はあるのに出てこない」がすり抜ける。
	MatchedName   bool `gorm:"column:matched_name"`
	MatchedRemote bool `gorm:"column:matched_remote"`
	MatchedHash   bool `gorm:"column:matched_hash"`
}

// MatchedBy renders the reasons in a stable order for the API.
func (r *RelatedApplication) MatchedBy() []string {
	out := make([]string, 0, 3)
	if r.MatchedName {
		out = append(out, "name")
	}
	if r.MatchedRemote {
		out = append(out, "remoteSource")
	}
	if r.MatchedHash {
		out = append(out, "fileHash")
	}
	return out
}

// StatusCounts breaks a set of applications down by status.
//
// 関連する過去の申請 (#2960) とユーザー単位の集計 (#2961) で同じ形を使う。
type StatusCounts struct {
	Total    int `json:"total"`
	Pending  int `json:"pending"`
	Approved int `json:"approved"`
	Rejected int `json:"rejected"`
	Canceled int `json:"canceled"`
}

// RelatedCounts is the #2960 spelling of StatusCounts.
type RelatedCounts = StatusCounts

// QuotaLimits collects every per-user limit checked before an application is
// created. ゼロ値は「上限なし」。
type QuotaLimits struct {
	// Windows はローリング期間の上限 (#2958)。**全ステータスを数える**ので、
	// 却下・取り下げでも枠は戻らない。上限 0 の窓は無制限として飛ばす。
	Windows []QuotaWindow
	// ResetAt はモデレーターが枠を手動で戻した時刻 (#2962)。ゼロ値なら未実施。
	//
	// **窓の開始をこれで押し上げる** (`max(期間の開始, ResetAt)`)。申請の行は
	// 消さないので、履歴 (#2960 の審査材料) は残したまま枠だけ戻せる。
	//
	// **審査待ちの上限には効かない。** あれは「今まさに審査待ちの件数」で、
	// 戻しても申請は審査待ちのまま残るので意味を持たない。
	ResetAt time.Time
	// MaxPending は同時に審査待ちにできる件数 (#2977)。0 は無制限。
	//
	// **Windows とは数え方が逆で、絞っているものも違う。** こちらは
	// `status = 'pending'` だけを数えるので**却下・取り下げで枠が戻る**。
	// Windows が絞るのは「出せる総量」、こちらが絞るのは「モデレーターが
	// 見る一覧の長さ」で、片方だけでは両方を制御できない。
	MaxPending int
}

// QuotaWindow is one rolling limit: "過去 Duration に Max 件まで" (#2958)。
type QuotaWindow struct {
	// Name は API が返す期間の識別子 ("day" / "week" / "month")。
	Name     string
	Duration time.Duration
	Max      int
}

// QuotaExceededError reports which rolling window was full.
type QuotaExceededError struct {
	Window QuotaWindow
	Used   int
	// RetryAt は**申請が通るようになる時刻**。窓が複数あるときは、いちばん
	// 遅く空くものに揃える (どれか 1 つでも満杯なら申請は通らない)。
	//
	// その窓の中では「`used - Max` 件飛ばした行が窓を出る時刻」。**最古では
	// 足りない** — `used > Max` のときは 1 件抜けても `used-1 >= Max` のまま。
	//
	// **審査待ちの上限 (#2977) も満杯ならゼロ値になる。** そのとき通るのは
	// モデレーターが処理した後で、時刻を予告できない。呼び出し元はゼロ値なら
	// `retryAt` も `Retry-After` も出さないこと。
	RetryAt time.Time
}

func (e *QuotaExceededError) Error() string {
	return "emoji application quota exceeded for window " + e.Window.Name
}

// PendingLimitExceededError reports that too many applications from the same
// user are still awaiting review (#2977).
//
// **RetryAt を持たない。** 空くのはモデレーターが処理したときで、時刻を
// 予告できない。`QuotaExceededError` と同じ形にして `Retry-After` を付けると
// 「待てば通る」と誤解させる。利用者が取れる行動は取り下げか、結果を待つか。
type PendingLimitExceededError struct {
	Used  int
	Limit int
}

func (e *PendingLimitExceededError) Error() string {
	return "too many emoji applications awaiting review"
}

type emojiApplicationRepository struct {
	db *gorm.DB
}

// NewEmojiApplicationRepository constructs the GORM-backed repository.
func NewEmojiApplicationRepository(db *gorm.DB) EmojiApplicationRepository {
	return &emojiApplicationRepository{db: db}
}

// Create inserts an application without checking any rolling window.
//
// **新しい呼び出し元は `CreateWithQuota` を使うこと (#2958)。** こちらを直に
// 呼ぶとロール別の期間上限を素通りする。残してあるのは、上限が 1 つも
// 設定されていないときに `CreateWithQuota` が委譲する先だから。
func (r *emojiApplicationRepository) Create(app *model.EmojiApplication) error {
	err := r.db.Create(app).Error
	if err == nil {
		return nil
	}
	// 部分一意索引 (userId, name) WHERE status = 'pending' の違反だけを
	// 区別する。ほかの制約違反は素通しして呼び出し側で 500 にする。
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "IDX_emoji_application_pending_name" {
		return ErrEmojiApplicationDuplicatePending
	}
	return err
}

// quotaLockNamespace keeps the advisory lock key from colliding with other
// features that lock on the same user id.
const quotaLockNamespace = 2958

// QuotaUsage is the read-side view of every per-user limit (#2961).
type QuotaUsage struct {
	Windows []QuotaWindowUsage
	// Worst は満杯の窓のうちいちばん遅く空くもの。満杯が無ければ nil。
	// **Windows の要素を指す**ので、`RetryAt` の抑制は両方に効く。
	Worst *QuotaWindowUsage
	// Pending は審査待ちの件数、MaxPending はその上限 (0 は無制限)。
	//
	// **窓と一緒に返す (レビュー H1)。** これを捨てると、審査待ちが満杯なのに
	// 画面が「24時間: 2 / 10 (空きあり)」と描き、実際の申請は 400 で弾かれる。
	Pending     int
	MaxPending  int
	PendingFull bool
}

// QuotaWindowUsage is one rolling window's current usage (#2961).
type QuotaWindowUsage struct {
	Window QuotaWindow
	Used   int
	// RetryAt is when the window frees up. **満杯のときだけ入る** — ゼロ値は
	// 「まだ空きがある」で、「時刻が分からない」ではない。
	RetryAt time.Time
}

// evaluateQuotaLimits evaluates every per-user limit at once (#2961).
//
// **`RetryAt` を落とす規則までここに置く (レビュー H1)。** 窓と審査待ちの
// 両方が満杯なら、窓が空く時刻に叩いてもまだ通らない。作成側だけがこの規則を
// 持っていたとき、読み取り側は**その時刻でも通らない時刻を画面に広告して
// いた** (実測)。「同じ計算を共有する」の対象は窓の件数だけではない。
func evaluateQuotaLimits(tx *gorm.DB, userID string, limits QuotaLimits, now time.Time) (QuotaUsage, error) {
	out := QuotaUsage{MaxPending: limits.MaxPending}
	if limits.MaxPending > 0 {
		pending, err := countPendingApplications(tx, userID)
		if err != nil {
			return QuotaUsage{}, err
		}
		out.Pending = pending
		out.PendingFull = pending >= limits.MaxPending
	}

	windows, err := evaluateQuotaWindows(tx, userID, limits.Windows, now, limits.ResetAt)
	if err != nil {
		return QuotaUsage{}, err
	}
	// **満杯の窓のうち、いちばん遅く空くものを採る。** 申請が通るのは全部の窓に
	// 空きができてからなので、最初に見つけたもので返すと早すぎる時刻になる
	// (#2958 で実測 78 時間ずれた)。
	for i := range windows {
		if windows[i].RetryAt.IsZero() {
			continue
		}
		if out.Worst == nil || windows[i].RetryAt.After(out.Worst.RetryAt) {
			out.Worst = &windows[i]
		}
	}
	if out.PendingFull {
		// **時刻を出さない。** 審査待ちが空くのはモデレーターが処理したときで
		// 予告できないので、嘘の時刻を広告するより「いつ空くか分からない」と
		// 伝えるほうが正確 (#2977)。
		for i := range windows {
			windows[i].RetryAt = time.Time{}
		}
	}
	out.Windows = windows
	return out, nil
}

// evaluateQuotaWindows counts each window and, for the full ones, works out when
// they free up.
//
// **作成側と読み取り側で同じ計算を使うためのヘルパー (#2961)。** モデレーション
// 画面に出す「3 / 5」や「次に出せる日時」を別の SQL で組み直すと、**画面は
// 空きありと言っているのに実際は弾かれる**という形でずれる。#2958 の retryAt は
// 2 段階の誤りを経て今の形 (満杯の窓を全部評価し、`used - Max` 件飛ばした行が
// 窓を出る時刻を採る) になっており、それを read 側で再現し直さない。
//
// tx にはトランザクションでも素の DB でも渡せる。作成側はアドバイザリロックの
// 中で呼ぶが、読み取り側は数えるだけなので囲まない (囲うと、画面を開いただけで
// 申請の直列化に割り込む)。
func evaluateQuotaWindows(tx *gorm.DB, userID string, windows []QuotaWindow, now, resetAt time.Time) ([]QuotaWindowUsage, error) {
	out := make([]QuotaWindowUsage, 0, len(windows))
	for _, w := range windows {
		u := QuotaWindowUsage{Window: w}
		if w.Duration <= 0 {
			// 期間が無い窓は数えようがない。0 件として返す (呼び出し側が
			// 上限の有無を表示できるように、窓そのものは落とさない)。
			out = append(out, u)
			continue
		}
		// **リセットの境界を当てる (#2962)。** 作成側と読み取り側で同じ関数を
		// 通すので、画面の「0 / 5」と実際の判定がずれない。
		since := quotaWindowSince(now, w.Duration, resetAt)
		var used int64
		// **全ステータスを数える。** 却下・取り下げで枠が戻ると、申請と
		// 取り下げを繰り返して審査通知と履歴を大量に作れる。
		if err := tx.Model(&model.EmojiApplication{}).
			Where(`"userId" = ? AND "createdAt" >= ?`, userID, since).
			Count(&used).Error; err != nil {
			return nil, err
		}
		u.Used = int(used)
		if w.Max <= 0 || used < int64(w.Max) {
			out = append(out, u)
			continue
		}
		// **「最古の 1 件」では足りない。** 空くのは used が Max を下回った
		// ときなので、`used - Max + 1` 件が窓を出るまで待つ必要がある。
		// 最古だけを見ると、上限を後から下げたときや上限の緩いロールを
		// 外したときに**広告した時刻に叩いてもまた弾かれる**。
		// offset は 0 起算なので `used - Max` 件飛ばした行が最後の 1 件。
		var freed []time.Time
		if err := tx.Model(&model.EmojiApplication{}).
			Select(`"createdAt"`).
			Where(`"userId" = ? AND "createdAt" >= ?`, userID, since).
			Order(`"createdAt" ASC`).
			Offset(int(used - int64(w.Max))).
			Limit(1).
			Scan(&freed).Error; err != nil {
			return nil, err
		}
		if len(freed) == 0 {
			// **fail-closed に倒す。** ここへ来るのは残り行数が `used - Max`
			// 以下のときで、「窓が空いた」とは限らない (used=10 / Max=3 なら
			// 7 行残っていても空になる)。COUNT を取り直さない以上、満杯の
			// まま通す側へは倒さない。`now` を置くと最大限保守的な時刻に
			// なる。**現状は到達しない** — `emoji_application` を消す
			// production 経路が無く、FK も張っていないので利用者削除の
			// CASCADE でも消えない。
			freed = []time.Time{now}
		}
		// **1ms 足す。** 窓の判定は `createdAt >= since` なので、境界ちょうど
		// の行はまだ窓の中にいる。しかも `entity.ISOMillis` は切り捨てなので、
		// 境界をそのまま広告すると**その値で再スケジュールするクライアントが
		// 必ず 1 回空振りする**。
		u.RetryAt = freed[0].Add(w.Duration + time.Millisecond)
		out = append(out, u)
	}
	return out, nil
}

func (r *emojiApplicationRepository) CreateWithQuota(app *model.EmojiApplication, limits QuotaLimits) error {
	active := make([]QuotaWindow, 0, len(limits.Windows))
	for _, w := range limits.Windows {
		if w.Max > 0 && w.Duration > 0 {
			active = append(active, w)
		}
	}
	if len(active) == 0 && limits.MaxPending <= 0 {
		return r.Create(app)
	}
	return r.db.Transaction(func(tx *gorm.DB) error {
		// **利用者単位で直列化する。** 行ロックだと「これから作る行」を
		// 守れないので、アドバイザリロックで数える側ごと囲う。
		// トランザクション版なので commit / rollback で自動的に解放される。
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?, hashtext(?))`,
			quotaLockNamespace, app.UserID).Error; err != nil {
			return err
		}
		now := app.CreatedAt
		if now.IsZero() {
			now = time.Now()
		}
		// **審査待ちの件数と窓を 1 か所で評価する。** 両方満杯のときに審査待ちを
		// 返すと「取り下げれば出せる」と案内することになるが、**期間上限は全
		// ステータスを数えるので取り下げた行は枠を占有したまま戻らない** —
		// 案内に従うと申請を 1 件失ったうえに枠も消費する。だから満杯の窓が
		// あればそちらを優先し、時刻だけを落とす。
		// **`ResetAt` を落とさない (#2962)。** 組み直すときに落とすと、読み取り側
		// だけリセットが効いて「画面は空きありなのに実際は弾かれる」になる。
		usage, err := evaluateQuotaLimits(tx, app.UserID, QuotaLimits{
			Windows:    active,
			MaxPending: limits.MaxPending,
			ResetAt:    limits.ResetAt,
		}, now)
		if err != nil {
			return err
		}
		if usage.Worst != nil {
			return &QuotaExceededError{
				Window:  usage.Worst.Window,
				Used:    usage.Worst.Used,
				RetryAt: usage.Worst.RetryAt,
			}
		}
		if usage.PendingFull {
			return &PendingLimitExceededError{Used: usage.Pending, Limit: limits.MaxPending}
		}
		if err := tx.Create(app).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				pgErr.ConstraintName == "IDX_emoji_application_pending_name" {
				return ErrEmojiApplicationDuplicatePending
			}
			return err
		}
		return nil
	})
}

func (r *emojiApplicationRepository) FindByID(id string) (*model.EmojiApplication, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var app model.EmojiApplication
	if err := r.db.Where(`"id" = ?`, id).First(&app).Error; err != nil {
		return nil, err
	}
	return &app, nil
}

func (r *emojiApplicationRepository) List(filter string, limit int, untilID string) ([]model.EmojiApplication, error) {
	q := r.db.Model(&model.EmojiApplication{})
	switch filter {
	case EmojiApplicationFilterPending:
		q = q.Where(`"status" = ?`, model.EmojiApplicationPending)
	case EmojiApplicationFilterProcessed:
		q = q.Where(`"status" <> ?`, model.EmojiApplicationPending)
	}
	return r.scanPage(q, limit, untilID)
}

func (r *emojiApplicationRepository) ListByUser(userID string, limit int, untilID string) ([]model.EmojiApplication, error) {
	q := r.db.Model(&model.EmojiApplication{}).Where(`"userId" = ?`, userID)
	return r.scanPage(q, limit, untilID)
}

// scanPage applies the shared ordering and keyset pagination.
//
// **id で切るのは createdAt で切らないため。** aidx は時刻順に単調なので同じ
// 並びになり、同一時刻の行を取りこぼさない。
func (r *emojiApplicationRepository) scanPage(q *gorm.DB, limit int, untilID string) ([]model.EmojiApplication, error) {
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	if limit <= 0 {
		limit = 30
	}
	var out []model.EmojiApplication
	if err := q.Order(`"id" DESC`).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (r *emojiApplicationRepository) CountPending() (int64, error) {
	var n int64
	err := r.db.Model(&model.EmojiApplication{}).
		Where(`"status" = ?`, model.EmojiApplicationPending).
		Count(&n).Error
	return n, err
}

func (r *emojiApplicationRepository) UpdateIfPending(app *model.EmojiApplication) (bool, error) {
	// Save ではなく Updates + Where。Save は主キーだけで WHERE を組むので
	// 条件を足せない。
	res := r.db.Model(&model.EmojiApplication{}).
		Where(`"id" = ? AND "status" = ?`, app.ID, model.EmojiApplicationPending).
		Updates(map[string]any{
			"status":        app.Status,
			"emojiId":       app.EmojiID,
			"processedById": app.ProcessedByID,
			"processedAt":   app.ProcessedAt,
			"rejectReason":  app.RejectReason,
			"updatedAt":     app.UpdatedAt,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// relatedMatchSQL is the shared match expression for FindRelated / CountRelated.
//
// **1 箇所に持つのが要点 (#2960)。** SELECT の理由列・WHERE の絞り・件数の
// 集計で別々に書くと、片方だけ直したときに「一致したのに理由が空」「理由は
// あるのに一覧に出てこない」「件数と中身が合わない」がすり抜ける。
//
// 名前付き引数で渡すのは、同じ値を何度も使うため。
const (
	// **空名にもガードを付ける。** `Service.Create` が空名を弾くので現状は
	// 到達しないが、remote / hash と非対称のまま残すと「3 条件を同列に扱う」
	// という下のコメントが事実でなくなる。
	relatedMatchName   = `(@name <> '' AND a."name" = @name)`
	relatedMatchRemote = `(@remoteHost <> '' AND a."remoteHost" = @remoteHost AND a."remoteName" = @remoteName)`
	relatedMatchHash   = `(@fileHash <> '' AND a."fileHash" = @fileHash)`
	relatedMatchAny    = relatedMatchName + ` OR ` + relatedMatchRemote + ` OR ` + relatedMatchHash
)

// relatedArgs builds the named arguments shared by both queries.
//
// **NULL ではなく空文字で渡す。** `@x <> ”` は NULL 比較の三値論理を避けられる
// ので、「ハッシュを持たない申請」と「ハッシュが一致しない申請」を取り違えない。
func relatedArgs(app *model.EmojiApplication) map[string]any {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return map[string]any{
		"id":         app.ID,
		"name":       app.Name,
		"remoteHost": deref(app.RemoteHost),
		"remoteName": deref(app.RemoteName),
		"fileHash":   deref(app.FileHash),
	}
}

func (r *emojiApplicationRepository) FindRelated(app *model.EmojiApplication, limit int, untilID string) ([]RelatedApplication, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	args := relatedArgs(app)
	args["limit"] = limit
	args["untilId"] = untilID

	// **自分自身は除く。** 「この申請に関連する過去の申請」なので、開いている
	// 行そのものが並ぶと件数も意味も狂う。
	q := `
SELECT a.*,
	` + relatedMatchName + ` AS matched_name,
	` + relatedMatchRemote + ` AS matched_remote,
	` + relatedMatchHash + ` AS matched_hash
FROM "emoji_application" a
WHERE a."id" <> @id
	AND (` + relatedMatchAny + `)
	AND (@untilId = '' OR a."id" < @untilId)
ORDER BY a."id" DESC
LIMIT @limit`

	var out []RelatedApplication
	if err := r.db.Raw(q, args).Scan(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (r *emojiApplicationRepository) CountRelated(app *model.EmojiApplication) (RelatedCounts, error) {
	q := `
SELECT a."status", count(*) AS n
FROM "emoji_application" a
WHERE a."id" <> @id AND (` + relatedMatchAny + `)
GROUP BY a."status"`

	var rows []statusCountRow
	if err := r.db.Raw(q, relatedArgs(app)).Scan(&rows).Error; err != nil {
		return RelatedCounts{}, err
	}
	return foldStatusCounts(rows), nil
}

// statusCountRow is one `GROUP BY "status"` row.
type statusCountRow struct {
	Status string
	N      int
}

// foldStatusCounts turns grouped rows into the API shape.
//
// **未知の status も Total には数える。** 内訳から漏れても「何件ある」という
// 事実は伝える (status が増えたときに件数が合わなくなるより良い)。
func foldStatusCounts(rows []statusCountRow) StatusCounts {
	var out StatusCounts
	for _, row := range rows {
		out.Total += row.N
		switch row.Status {
		case model.EmojiApplicationPending:
			out.Pending = row.N
		case model.EmojiApplicationApproved:
			out.Approved = row.N
		case model.EmojiApplicationRejected:
			out.Rejected = row.N
		case model.EmojiApplicationCanceled:
			out.Canceled = row.N
		}
	}
	return out
}

func (r *emojiApplicationRepository) ListByUserFiltered(userID, status, query string, limit int, untilID string) ([]model.EmojiApplication, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(query) || !storable(userID) || !storable(status) {
		return []model.EmojiApplication{}, nil
	}
	q := r.db.Model(&model.EmojiApplication{}).Where(`"userId" = ?`, userID)
	switch status {
	case "", EmojiApplicationFilterAll:
	case model.EmojiApplicationPending, model.EmojiApplicationApproved,
		model.EmojiApplicationRejected, model.EmojiApplicationCanceled:
		q = q.Where(`"status" = ?`, status)
	default:
		// **未知の status を全件に倒さない。** 絞ったつもりで全部出るほうが
		// 危険側。handler も弾くが、ここでも閉じる。
		return []model.EmojiApplication{}, nil
	}
	if term := strings.TrimSpace(query); term != "" {
		// **エスケープは既存の実装を使う (レビュー M1)。** この
		// パッケージには `escapeSQLLikePattern` が既にあり、`\` を含む
		// 表テストも持っている。3 つ目を書くと、片方だけ直したときに
		// 「ここだけエスケープが甘い」が残る。
		like := "%" + escapeSQLLikePattern(term) + "%"
		// **remoteHost / remoteName は NULL を取りうる。** ILIKE は NULL に
		// 対して NULL を返すので OR の中では偽として扱われ、own の申請が
		// 検索から落ちることはない (名前側で拾う)。
		q = q.Where(`("name" ILIKE @like OR "remoteHost" ILIKE @like OR "remoteName" ILIKE @like)`,
			map[string]any{"like": like})
	}
	return r.scanPage(q, limit, untilID)
}

func (r *emojiApplicationRepository) CountByUserStatus(userID string) (StatusCounts, error) {
	var rows []statusCountRow
	if err := r.db.Model(&model.EmojiApplication{}).
		Select(`"status", count(*) AS n`).
		Where(`"userId" = ?`, userID).
		Group(`"status"`).
		Scan(&rows).Error; err != nil {
		return StatusCounts{}, err
	}
	return foldStatusCounts(rows), nil
}

func (r *emojiApplicationRepository) QuotaUsage(userID string, limits QuotaLimits, now time.Time) (QuotaUsage, error) {
	if now.IsZero() {
		now = time.Now()
	}
	// **ロックを取らない。** 数えるだけなので、囲うと画面を開いただけで
	// 申請の直列化 (pg_advisory_xact_lock) に割り込む。
	out, err := evaluateQuotaLimits(r.db, userID, limits, now)
	if err != nil {
		return QuotaUsage{}, err
	}
	if limits.MaxPending <= 0 {
		// **上限が無くても件数は返す (レビュー M2)。** 作成側は上限があるときしか
		// 数えないが、読み取り側が 0 のままだと**同じレスポンスの中で
		// `counts.pending` と食い違う** (既定は無制限なので、ほぼ全ての構成が
		// これに当たる)。作成側に余計な COUNT を足さずに読み取り側だけ揃える。
		pending, err := countPendingApplications(r.db, userID)
		if err != nil {
			return QuotaUsage{}, err
		}
		out.Pending = pending
	}
	return out, nil
}

// countPendingApplications counts how many of the user's applications are still
// awaiting review (#2977 / #2961).
func countPendingApplications(tx *gorm.DB, userID string) (int, error) {
	var n int64
	if err := tx.Model(&model.EmojiApplication{}).
		Where(`"userId" = ? AND "status" = ?`, userID, model.EmojiApplicationPending).
		Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}
