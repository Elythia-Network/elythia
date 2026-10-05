package repository

import (
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/pgarray"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserIPRepository provides data access for the `user_ip` table.
type UserIPRepository interface {
	// Observe records that userID was seen from ip at the given time.
	//
	// **`createdAt` は衝突しても更新しない (#3103)。** upstream の
	// `ApiCallService.logIp` が `INSERT ... orIgnore` なので、あの列は初回観測を
	// 意味する。更新するのは mk-go 独自列の `lastSeenAt` / `observationCount`。
	//
	// ip は `ipnorm.Normalize` を通した正規形であることを前提にする。表記揺れの
	// まま入れると同じ端末の観測が別の行に分かれる。
	Observe(userID, ip string, at time.Time) error
	// ListByUser returns IPs for the given user, newest first.
	//
	// **並びは `id` の降順。** upstream `admin/get-user-ips` が
	// `order: { id: 'DESC' }` なのに揃えてある (`createdAt` ではない)。`id` は
	// 行を初めて作ったときに採番されるので、実質「初めて観測した順」になる。
	//
	// **最終観測では並べ替えない。** upstream と順序が変わるうえ、返している値
	// (`createdAt` = 初回観測) と順序の基準が食い違う。ただし上限 30 件なので、
	// 「初めて見たのは古いが今も使っている IP」は一覧から落ちうる (#3103 で
	// `createdAt` の意味を変えたことで、それ以前とは見え方が変わる)。
	ListByUser(userID string, limit int) ([]*model.UserIP, error)
	// DeleteLastSeenBefore removes user_ip rows whose **last** observation is
	// older than t. Run by the daily clean cron to prune IP history older than
	// 90 days (#1563). Returns rows deleted.
	//
	// **基準は `createdAt` ではない (#3103)。** あちらは初回観測なので、基準に
	// すると「初めて見たのは 1 年前だが今も使っている IP」まで消える。
	DeleteLastSeenBefore(t time.Time) (int64, error)
}

type userIPRepository struct {
	db *gorm.DB
}

// NewUserIPRepository constructs the default UserIPRepository.
func NewUserIPRepository(db *gorm.DB) UserIPRepository {
	return &userIPRepository{db: db}
}

func (r *userIPRepository) Observe(userID, ip string, at time.Time) error {
	record := &model.UserIP{
		UserID: userID, IP: ip,
		CreatedAt: at, LastSeenAt: at, ObservationCount: 1,
	}
	return r.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "userId"}, {Name: "ip"}},
		DoUpdates: clause.Assignments(map[string]any{
			// **GREATEST で受ける。** 観測が前後して届くことがある (記録は
			// goroutine で走るので順序の保証が無い)。素直に代入すると古い時刻へ
			// 巻き戻り、保持の刈り取り基準まで古くなる。
			"lastSeenAt":       gorm.Expr(`GREATEST("user_ip"."lastSeenAt", EXCLUDED."lastSeenAt")`),
			"observationCount": gorm.Expr(`"user_ip"."observationCount" + 1`),
		}),
	}).Create(record).Error
}

func (r *userIPRepository) ListByUser(userID string, limit int) ([]*model.UserIP, error) {
	if limit <= 0 {
		limit = 30
	}
	var ips []*model.UserIP
	if err := r.db.Where(`"userId" = ?`, userID).Order(`id DESC`).Limit(limit).Find(&ips).Error; err != nil {
		return nil, err
	}
	return ips, nil
}

func (r *userIPRepository) DeleteLastSeenBefore(t time.Time) (int64, error) {
	res := r.db.Where(`"lastSeenAt" < ?`, t).Delete(&model.UserIP{})
	return res.RowsAffected, res.Error
}

// UserIPAccountRow is one local account observed from a searched IP (#3104).
type UserIPAccountRow struct {
	UserID string
	// FirstSeen / LastSeen は `createdAt` / `lastSeenAt` (#3103)。
	FirstSeen        time.Time
	LastSeen         time.Time
	ObservationCount int
}

// UserIPSearchRepository looks up which accounts were seen from an IP (#3104).
//
// **`UserIPRepository` と分けてある。** あちらは記録と保持 (書き込み側) で、
// こちらはモデレーション用の読み取り。#3105 / #3106 が足す口もこちら側に付く。
type UserIPSearchRepository interface {
	// ListAccountsByIP returns accounts whose last observation of ip is at or
	// after since, newest last-seen first.
	//
	// **ip は `ipnorm.Normalize` を通した正規形であること。** 記録側は正規形しか
	// 書かないので、揺れたまま渡すと当たらない。正規化は NUL も落とすので、
	// 一覧系が nulparam gate の対象外 (#3025) でも列に入らない値は届かない。
	//
	// limit+1 件まで引いて、呼び出し側が「次がある」を判断できるようにする。
	ListAccountsByIP(ip string, since time.Time, limit, offset int) ([]UserIPAccountRow, error)
	// HasAnyHistory reports whether user_ip holds any row at all.
	//
	// **「一致なし」と「そもそも記録が無い」を区別するために要る** (#3066 §7)。
	// 記録が無効だった / 保持期間を過ぎた構成で「関連アカウントなし」と断定しない。
	HasAnyHistory() (bool, error)
	// ListIPsByUser returns the IPs userID was seen from within the window,
	// most recently seen first, at most limit rows.
	//
	// **`ListByUser` とは並びも窓も違う。** あちらは upstream の
	// `admin/get-user-ips` に合わせた `id DESC` (= 初回観測順) の一覧で、
	// こちらは関連候補の起点 (#3105)。**最終観測の新しい順に切る**のは、
	// 打ち切ったときに残るのが**重みの大きい IP** になるようにするため
	// (時間減衰は古いほど軽い)。
	ListIPsByUser(userID string, since time.Time, limit int) ([]UserIPWindowRow, error)
	// ListSharedIPAccounts returns, for each of ips, at most perIP accounts seen
	// from it within the window (the target's own row included), newest first.
	//
	// 呼び出し側は「まだ他にも居る」を判断するために上限 + 1 を渡す規約だが、
	// それは呼び出し側の都合で、ここの契約は「perIP 件まで」。
	//
	// **走査そのものを有界にしてある。** CGNAT や公衆 Wi-Fi の IP は 1 つで数千
	// アカウントが載りうるので、「読んでから絞る」形だと利用者 1 人を指定する
	// だけで無制限の走査を外から回せる。実測では `COUNT(*) OVER (PARTITION BY ip)`
	// を使う形が**返す行を絞っても 227 万行 / 2.4 秒**走り、LATERAL + per-IP LIMIT
	// の形が同じ結果を 21.6 ms で返した。**だから正確な「その IP のアカウント数」は
	// 返せない** — 数えるには全行読む必要がある。呼び出し側は「perIP+1 件まで見えた」
	// という下限として扱うこと (共有回線かの判断にはそれで足りる)。
	//
	// **対象本人を除かない。** 「その IP を使ったアカウント数」は本人を含む数なので、
	// SQL で落とすと数えられなくなる。候補から外すのは呼び出し側。
	ListSharedIPAccounts(ips []string, since time.Time, perIP int) ([]UserIPSharedRow, error)
}

// UserIPWindowRow is one IP a user was seen from, with its last observation.
type UserIPWindowRow struct {
	IP       string
	LastSeen time.Time
}

// UserIPSharedRow is one (candidate, ip) pair the target was also seen from (#3105).
type UserIPSharedRow struct {
	UserID string
	IP     string
	// LastSeen はその IP に対する最終観測。対象側の最終観測は `ListIPsByUser` が
	// 返しているので、突き合わせは呼び出し側で行う (同じ値を 2 本のクエリで 2 回
	// 運ばない)。
	LastSeen time.Time
}

type userIPSearchRepository struct {
	db *gorm.DB
	// lookupTimeout は 1 回の照会に掛ける上限。既定は lookupStatementTimeout。
	//
	// **定数を直接読まずフィールドに持つのは、実際に切れることを試せるようにするため。**
	// 定数のままだと、テストは「10 秒眠って切れる」を試すか、コールバックの中で
	// テスト自身が上限を掛け直すしかない。後者にすると**本番コードが上限を掛けたか
	// を一切見ないテスト**になり、`SET LOCAL` を丸ごと消しても緑のまま通る (実測)。
	lookupTimeout time.Duration
}

// withLookupTimeout bounds one user_ip lookup. 実体は withStatementTimeout。
func (r *userIPSearchRepository) withLookupTimeout(fn func(tx *gorm.DB) error) error {
	return withStatementTimeout(r.db, r.lookupTimeout, fn)
}

// NewUserIPSearchRepository constructs the default UserIPSearchRepository.
func NewUserIPSearchRepository(db *gorm.DB) UserIPSearchRepository {
	return &userIPSearchRepository{db: db, lookupTimeout: lookupStatementTimeout}
}

// userIPAccountsSQL ranks accounts by their last observation of the IP.
//
// **`(ip, lastSeenAt DESC)` の index に乗る形** (migration 000094)。並びは
// 最終観測の降順 → 初回観測の降順 → userId 昇順。`userId` を最後に置くのは、
// 同じ時刻の行で順序が実行ごとに変わらないようにするため (ページングが破綻する)。
//
// **クエリで host を絞っていないが、それで正しい。** `user_ip` は upstream /
// mk-go とも自インスタンスで認証を通した利用者しか書かないので、行そのものが
// ローカル分に閉じている (リモート利用者の行は生じない)。
const userIPAccountsSQL = `
SELECT "userId" AS user_id, "createdAt" AS first_seen, "lastSeenAt" AS last_seen,
	"observationCount" AS observation_count
FROM "user_ip"
WHERE ip = ? AND "lastSeenAt" >= ?
ORDER BY "lastSeenAt" DESC, "createdAt" DESC, "userId" ASC
LIMIT ? OFFSET ?`

func (r *userIPSearchRepository) ListAccountsByIP(ip string, since time.Time, limit, offset int) ([]UserIPAccountRow, error) {
	if limit <= 0 {
		limit = 1
	}
	if offset < 0 {
		offset = 0
	}
	type scan struct {
		UserID           string    `gorm:"column:user_id"`
		FirstSeen        time.Time `gorm:"column:first_seen"`
		LastSeen         time.Time `gorm:"column:last_seen"`
		ObservationCount int       `gorm:"column:observation_count"`
	}
	var rows []scan
	if err := r.withLookupTimeout(func(tx *gorm.DB) error {
		return tx.Raw(userIPAccountsSQL, ip, since, limit, offset).Scan(&rows).Error
	}); err != nil {
		return nil, fmt.Errorf("user_ip accounts by ip: %w", err)
	}
	out := make([]UserIPAccountRow, 0, len(rows))
	for _, s := range rows {
		out = append(out, UserIPAccountRow{
			UserID: s.UserID, FirstSeen: s.FirstSeen,
			LastSeen: s.LastSeen, ObservationCount: s.ObservationCount,
		})
	}
	return out, nil
}

func (r *userIPSearchRepository) HasAnyHistory() (bool, error) {
	var exists bool
	if err := r.withLookupTimeout(func(tx *gorm.DB) error {
		return tx.Raw(`SELECT EXISTS (SELECT 1 FROM "user_ip")`).Scan(&exists).Error
	}); err != nil {
		return false, fmt.Errorf("user_ip has any history: %w", err)
	}
	return exists, nil
}

// userIPWindowSQL lists the IPs a user was seen from inside the window.
//
// `IDX_user_ip_userId` に乗る。並びは最終観測の降順 → `ip` 昇順で、
// `ip` の tiebreaker が無いと同時刻の行で打ち切る位置が実行ごとに変わる。
const userIPWindowSQL = `
SELECT ip, "lastSeenAt" AS last_seen
FROM "user_ip"
WHERE "userId" = ? AND "lastSeenAt" >= ?
ORDER BY "lastSeenAt" DESC, ip ASC
LIMIT ?`

func (r *userIPSearchRepository) ListIPsByUser(userID string, since time.Time, limit int) ([]UserIPWindowRow, error) {
	if limit <= 0 {
		limit = 1
	}
	type scan struct {
		IP       string    `gorm:"column:ip"`
		LastSeen time.Time `gorm:"column:last_seen"`
	}
	var rows []scan
	if err := r.withLookupTimeout(func(tx *gorm.DB) error {
		return tx.Raw(userIPWindowSQL, userID, since, limit).Scan(&rows).Error
	}); err != nil {
		return nil, fmt.Errorf("user_ip ips by user: %w", err)
	}
	out := make([]UserIPWindowRow, 0, len(rows))
	for _, s := range rows {
		out = append(out, UserIPWindowRow{IP: s.IP, LastSeen: s.LastSeen})
	}
	return out, nil
}

// userIPSharedSQL takes the newest rows per IP through a per-IP index scan.
//
// **LATERAL + per-IP LIMIT にしてあるのが要点。** `COUNT(*) OVER (PARTITION BY ip)`
// で数える形は、`ROW_NUMBER` で絞る前に**その IP の全行を読んでソートする**ので、
// 返す行を絞っても走査は有界にならない。この形は各 IP で index scan を `perIP`
// 件で止めるので、起点 50 本 × 201 件 = 最大 1 万行で頭打ちになる。
//
// **並びは `IDX_user_ip_ip_lastSeenAt_userId` (migration 000095) に完全に乗せる。**
// `userId` を index から外すと `lastSeenAt` を presorted key とする incremental
// sort に倒れ、**上限が保証されるかが同値の分布に依る** — 同じ `lastSeenAt` が
// 固まっているとそのグループを全部読む (実測 PG 15、1 IP に 10 万アカウント、
// `LIMIT 201`: 同値が固まっていると 100,000 行 / 33.7 ms、ばらけていれば 202 行
// / 0.14 ms。index に `userId` を含めるとどちらでも 201 行 / 0.11-0.17 ms)。
//
// `COUNT(*) OVER (PARTITION BY ip)` で数える形は分布に関係なく全行読む
// (200 万行の構成で 6,020 ms、temp に 176 MB)。
//
// **`userId` を並びから外して逃げない。** 同じ最終観測が並んだときにどの候補を
// 残すかが実行ごとに変わり、同じ検索が違う結果を返す。
//
// **対象本人も返す。** 「その IP を使ったアカウント数」は本人を含む数なので、
// ここで落とすと数えられなくなる。候補から外すのは呼び出し側。
const userIPSharedSQL = `
SELECT t.ip AS ip, s."userId" AS user_id, s."lastSeenAt" AS last_seen
FROM unnest(?::text[]) AS t(ip)
CROSS JOIN LATERAL (
	SELECT "userId", "lastSeenAt"
	FROM "user_ip"
	WHERE ip = t.ip AND "lastSeenAt" >= ?
	ORDER BY "lastSeenAt" DESC, "userId" ASC
	LIMIT ?
) s
ORDER BY t.ip ASC, s."lastSeenAt" DESC, s."userId" ASC`

func (r *userIPSearchRepository) ListSharedIPAccounts(ips []string, since time.Time, perIP int) ([]UserIPSharedRow, error) {
	// **無駄な往復を省くだけの保険。** 空でも SQL としては成立する。
	if len(ips) == 0 {
		return nil, nil
	}
	if perIP <= 0 {
		perIP = 1
	}
	type scan struct {
		IP       string    `gorm:"column:ip"`
		UserID   string    `gorm:"column:user_id"`
		LastSeen time.Time `gorm:"column:last_seen"`
	}
	var rows []scan
	if err := r.withLookupTimeout(func(tx *gorm.DB) error {
		return tx.Raw(userIPSharedSQL, pgarray.StringArray(ips), since, perIP).Scan(&rows).Error
	}); err != nil {
		return nil, fmt.Errorf("user_ip shared accounts: %w", err)
	}
	out := make([]UserIPSharedRow, 0, len(rows))
	for _, s := range rows {
		out = append(out, UserIPSharedRow{UserID: s.UserID, IP: s.IP, LastSeen: s.LastSeen})
	}
	return out, nil
}
