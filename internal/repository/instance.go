package repository

import (
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// InstanceRepository provides data access for the `instance` table.
type InstanceRepository interface {
	Create(i *model.Instance) error
	FindByHost(host string) (*model.Instance, error)
	// FindManyByHosts returns instance rows matching any of the given hosts.
	// Used by entity packers to batch-resolve UserLite.Instance info on
	// timeline hot paths (#277). Empty input returns nil.
	FindManyByHosts(hosts []string) ([]*model.Instance, error)
	UpdateFields(host string, fields map[string]any) error
	IncrementCount(host, column string, delta int) error
	List(filter model.InstanceListFilter) ([]*model.Instance, error)
	// ListPeerHosts returns the host of every instance that is not suspended,
	// for the Mastodon-compatible GET /api/v1/instance/peers endpoint.
	// upstream ApiServerService は `select: {host: true}` /
	// `where: {suspensionState: 'none'}` の全件走査で limit も order も持たない
	// ため、こちらも同じ semantics にする (#2245)。host 列だけを引くので
	// List() の全列 SELECT より軽い。
	ListPeerHosts() ([]string, error)
	// ListForRefresh returns instances whose metadata should be refreshed by
	// the periodic instance-refresh job (#393). Only live instances
	// (suspensionState = 'none' AND isNotResponding = false) whose
	// infoUpdatedAt is older than staleBefore (NULL counts as stale) are
	// returned. Ordered by infoUpdatedAt ASC NULLS FIRST so the oldest data
	// is refreshed first.
	ListForRefresh(staleBefore time.Time, limit int) ([]*model.Instance, error)
	// RecomputeFollowCounts refreshes followersCount / followingCount on
	// every instance row from the live `following` table. 起動時の再計算
	// 用。incremental hook (Increment{Followers,Following}Count) が
	// 入った後も累積誤差 (rare race / direct DB tampering 等) を reset する
	// 安全網として残す (#421 / #596)。
	RecomputeFollowCounts() error
	// IncrementFollowersCount adjusts instance(host).followersCount by delta
	// (typically ±1). Following 行作成 / 削除時に core/following が呼ぶ。
	// 該当 instance 行が無い (= 未知のホスト) 場合は no-op (#596)。
	IncrementFollowersCount(host string, delta int) error
	// IncrementFollowingCount adjusts instance(host).followingCount by delta。
	// 詳細は IncrementFollowersCount と対称 (#596)。
	IncrementFollowingCount(host string, delta int) error
}

type instanceRepository struct {
	db *gorm.DB
}

// NewInstanceRepository creates a new InstanceRepository.
func NewInstanceRepository(db *gorm.DB) InstanceRepository {
	return &instanceRepository{db: db}
}

func (r *instanceRepository) Create(i *model.Instance) error {
	return r.db.Create(i).Error
}

func (r *instanceRepository) FindByHost(host string) (*model.Instance, error) {
	if !storable(host) {
		return nil, ErrNotFound
	}
	var inst model.Instance
	if err := r.db.Where("host = ?", host).First(&inst).Error; err != nil {
		return nil, err
	}
	return &inst, nil
}

func (r *instanceRepository) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	var rows []*model.Instance
	if err := r.db.Where("host IN ?", hosts).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateFields applies a map of column → value updates to the instance row
// keyed by host. 集計列以外の任意フィールド更新に使う。
func (r *instanceRepository) UpdateFields(host string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Instance{}).Where("host = ?", host).Updates(fields).Error
}

// IncrementCount adjusts a counter column on the instance row by delta.
// usersCount / notesCount / followingCount / followersCount などの集計列向け。
func (r *instanceRepository) IncrementCount(host, column string, delta int) error {
	return r.db.Model(&model.Instance{}).
		Where("host = ?", host).
		UpdateColumn(column, gorm.Expr("\""+column+"\" + ?", delta)).Error
}

// ListForRefresh implements the periodic metadata refresh query (#393).
func (r *instanceRepository) ListForRefresh(staleBefore time.Time, limit int) ([]*model.Instance, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var rows []*model.Instance
	err := r.db.
		Where(`"suspensionState" = ?`, string(model.SuspensionStateNone)).
		Where(`"isNotResponding" = ?`, false).
		Where(`"infoUpdatedAt" IS NULL OR "infoUpdatedAt" < ?`, staleBefore).
		Order(`"infoUpdatedAt" ASC NULLS FIRST`).
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ListPeerHosts returns non-suspended instance hosts. See the interface doc.
func (r *instanceRepository) ListPeerHosts() ([]string, error) {
	var hosts []string
	err := r.db.Model(&model.Instance{}).
		Where(`"suspensionState" = ?`, string(model.SuspensionStateNone)).
		Order("host ASC").
		Pluck("host", &hosts).Error
	if err != nil {
		return nil, err
	}
	return hosts, nil
}

// List returns instances matching the filter, ordered by the requested sort.
func (r *instanceRepository) List(filter model.InstanceListFilter) ([]*model.Instance, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(filter.Host) {
		return nil, nil
	}
	q := r.db.Model(&model.Instance{})
	if filter.Host != "" {
		// LIKE metacharacter (% / _ / backslash) を escape して literal 一致にする
		// (upstream instances.ts の sqlLikeEscape 相当、#1777)。escape しないと
		// host に % / _ を含む query が wildcard 解釈される。
		q = q.Where("host ILIKE ?", "%"+escapeLike(filter.Host)+"%")
	}
	if filter.Suspended != nil {
		if *filter.Suspended {
			q = q.Where("\"suspensionState\" <> ?", string(model.SuspensionStateNone))
		} else {
			q = q.Where("\"suspensionState\" = ?", string(model.SuspensionStateNone))
		}
	}
	if filter.NotResponding != nil {
		q = q.Where("\"isNotResponding\" = ?", *filter.NotResponding)
	}
	// federating / subscribing / publishingはhandler側のinstanceToMapと
	// 同じ式で判定する。false指定のときも反対条件でフィルタリングしないと、
	// レスポンス上のfederatingとfilterの意味論が食い違う (本家TSと同じ挙動)。
	if filter.Federating != nil {
		if *filter.Federating {
			// GORMはraw string中のOR条件を自動では括弧で囲まないため、
			// 他の.Where()とANDで連結したときに演算子優先順位で崩れる。
			// 同じ注意点はnote.go:407 / announcement.go:114と同様。
			q = q.Where("(\"followingCount\" > 0 OR \"followersCount\" > 0)")
		} else {
			q = q.Where("\"followingCount\" = 0 AND \"followersCount\" = 0")
		}
	}
	if filter.Subscribing != nil {
		if *filter.Subscribing {
			q = q.Where("\"followersCount\" > 0")
		} else {
			q = q.Where("\"followersCount\" = 0")
		}
	}
	if filter.Publishing != nil {
		if *filter.Publishing {
			q = q.Where("\"followingCount\" > 0")
		} else {
			q = q.Where("\"followingCount\" = 0")
		}
	}
	// blocked / silenced は service が meta から解決した host 一覧との exact
	// IN / NOT IN で突合する (本家 federation/instances と同じ semantics):
	//   - X == true  かつ list 空 → 0 件 (1 = 0)
	//   - X == true  かつ list あり → host IN (list)
	//   - X == false かつ list 空 → 全件 (条件なし)
	//   - X == false かつ list あり → host NOT IN (list)
	if filter.Blocked != nil {
		if *filter.Blocked {
			if len(filter.BlockedHosts) == 0 {
				q = q.Where("1 = 0")
			} else {
				q = q.Where("host IN ?", filter.BlockedHosts)
			}
		} else if len(filter.BlockedHosts) > 0 {
			q = q.Where("host NOT IN ?", filter.BlockedHosts)
		}
	}
	if filter.Silenced != nil {
		if *filter.Silenced {
			if len(filter.SilencedHosts) == 0 {
				q = q.Where("1 = 0")
			} else {
				q = q.Where("host IN ?", filter.SilencedHosts)
			}
		} else if len(filter.SilencedHosts) > 0 {
			q = q.Where("host NOT IN ?", filter.SilencedHosts)
		}
	}
	cursor := filter.SinceID != "" || filter.UntilID != ""
	if filter.SinceID != "" {
		q = q.Where("id > ?", filter.SinceID)
	}
	if filter.UntilID != "" {
		q = q.Where("id < ?", filter.UntilID)
	}
	if cursor {
		q = q.Order(paginationOrder(filter.SinceID, filter.UntilID, "id"))
	} else {
		// sort key の向きは本家 federation/instances に合わせる: 接頭辞 "+" が
		// DESC、"-" が ASC (frontend のラベルも "+notes" = 降順)。pubSub は
		// followingCount → followersCount の複合ソート。+host / -host は本家に
		// 無い mk-go 拡張なので従来どおり host の昇順 / 降順を維持する。
		switch filter.SortBy {
		case "+pubSub":
			q = q.Order("\"followingCount\" DESC").Order("\"followersCount\" DESC")
		case "-pubSub":
			q = q.Order("\"followingCount\" ASC").Order("\"followersCount\" ASC")
		case "+notes":
			q = q.Order("\"notesCount\" DESC")
		case "-notes":
			q = q.Order("\"notesCount\" ASC")
		case "+users":
			q = q.Order("\"usersCount\" DESC")
		case "-users":
			q = q.Order("\"usersCount\" ASC")
		case "+following":
			q = q.Order("\"followingCount\" DESC")
		case "-following":
			q = q.Order("\"followingCount\" ASC")
		case "+followers":
			q = q.Order("\"followersCount\" DESC")
		case "-followers":
			q = q.Order("\"followersCount\" ASC")
		case "+firstRetrievedAt":
			q = q.Order("\"firstRetrievedAt\" DESC")
		case "-firstRetrievedAt":
			q = q.Order("\"firstRetrievedAt\" ASC")
		case "+latestRequestReceivedAt":
			q = q.Order("\"latestRequestReceivedAt\" DESC NULLS LAST")
		case "-latestRequestReceivedAt":
			q = q.Order("\"latestRequestReceivedAt\" ASC NULLS FIRST")
		case "+host":
			q = q.Order("host ASC")
		case "-host":
			q = q.Order("host DESC")
		default:
			// 本家 TS は sort 未指定時 instance.id DESC (= aidx なので新しい順)。
			q = q.Order("id DESC")
		}
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q = q.Limit(limit)
	if !cursor && filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	var rows []*model.Instance
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// RecomputeFollowCounts recomputes the followersCount / followingCount
// columns of every instance row from the live `following` table. Used at
// startup to backfill stale zeros until incremental hooks land (#421)。
//
// 列の意味は本家 Misskey と揃える (UserFollowingService.insertFollowingDoc /
// decrementFollowing が足す向き、#3330)。どちらも「その host の側から見た」数:
//   - `followingCount`: 当該リモートインスタンスの user が**ローカルの user を**
//     何人 follow しているか (= 我々の投稿を購読している側 = publishing)。
//     SQL では follower.host = X かつ followee がローカルの行を数える。
//   - `followersCount`: ローカルの user が**当該インスタンスの user を**何人
//     follow しているか (= 我々が購読している側 = subscribing)。SQL では
//     followee.host = X かつ follower がローカルの行を数える。
//
// federation/instances の subscribing / publishing と federation/stats の上位は
// この意味で読んでいる。以前はここが逆向きに数えていたので、起動のたびに
// TS 版が正しく積んだ値も入れ替わっていた。
//
// Reset → backfill の二段構え: 過去 follow が消えた host (= subquery に
// 出てこない) は subquery JOIN だと UPDATE 対象外になり、古い非ゼロ値が
// 永遠に残ってしまう (#421 Devin review)。先に全 instance を 0 にしてから
// 該当 host のみ再上書きすることで、follow を全部解除した instance も
// 確実に 0 へ戻す。
//
// 3 つの UPDATE は単一トランザクションでまとめる: reset だけ先に走って
// 後段で fail すると全 instance が永続的に 0 になってしまうため (#421
// Devin review)。
func (r *instanceRepository) RecomputeFollowCounts() error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(
			`UPDATE "instance" SET "followersCount" = 0, "followingCount" = 0`,
		).Error; err != nil {
			return err
		}
		// followingCount = remote follower (host X) → local followee の行数。
		const following = `
UPDATE "instance" SET "followingCount" = c.cnt
FROM (
  SELECT fr.host AS host, COUNT(*)::int AS cnt
  FROM "following" f
  JOIN "user" fr ON f."followerId" = fr.id
  JOIN "user" fe ON f."followeeId" = fe.id
  WHERE fr.host IS NOT NULL AND fe.host IS NULL
  GROUP BY fr.host
) c
WHERE "instance".host = c.host`
		if err := tx.Exec(following).Error; err != nil {
			return err
		}
		// followersCount = local follower → remote followee (host X) の行数。
		const followers = `
UPDATE "instance" SET "followersCount" = c.cnt
FROM (
  SELECT fe.host AS host, COUNT(*)::int AS cnt
  FROM "following" f
  JOIN "user" fr ON f."followerId" = fr.id
  JOIN "user" fe ON f."followeeId" = fe.id
  WHERE fe.host IS NOT NULL AND fr.host IS NULL
  GROUP BY fe.host
) c
WHERE "instance".host = c.host`
		return tx.Exec(followers).Error
	})
}

// IncrementFollowersCount は atomic UPDATE で instance(host).followersCount
// を delta (典型的には ±1) 増減する。Following 行作成 / 削除時に呼ばれる
// (#596)。host が空文字 (ローカル user) の場合は no-op。該当 host の instance
// 行が無い場合 (= 未登録の remote host) も UPDATE が 0 行で終わるだけで
// error にしない (best-effort、後段 cron / RecomputeFollowCounts で整合)。
func (r *instanceRepository) IncrementFollowersCount(host string, delta int) error {
	if host == "" || delta == 0 {
		return nil
	}
	return r.db.Exec(
		`UPDATE "instance" SET "followersCount" = "followersCount" + ? WHERE host = ?`,
		delta, host,
	).Error
}

// IncrementFollowingCount は IncrementFollowersCount の followingCount 版。
func (r *instanceRepository) IncrementFollowingCount(host string, delta int) error {
	if host == "" || delta == 0 {
		return nil
	}
	return r.db.Exec(
		`UPDATE "instance" SET "followingCount" = "followingCount" + ? WHERE host = ?`,
		delta, host,
	).Error
}
