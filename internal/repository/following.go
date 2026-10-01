package repository

import (
	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// FollowingRepository provides data access for the `following` table.
type FollowingRepository interface {
	Create(f *model.Following) error
	Delete(f *model.Following) error
	FindByPair(followerID, followeeID string) (*model.Following, error)
	Exists(followerID, followeeID string) (bool, error)
	// FilterFollowingsFromAnchor returns the subset of candidateIDs that
	// anchorID follows (anchor → candidate direction, = rows where
	// followerId=anchorID AND followeeId IN candidates). Used to batch-
	// compute `isFollowing` across a user list (e.g. users/following /
	// users/followers) without N+1 Exists round-trips (#1144).
	FilterFollowingsFromAnchor(anchorID string, candidateIDs []string) ([]string, error)
	// FilterFollowingsToAnchor returns the subset of candidateIDs that
	// follow anchorID (candidate → anchor direction, = rows where
	// followerId IN candidates AND followeeId=anchorID). Used to batch-
	// compute `isFollowed` across a user list (#1144).
	FilterFollowingsToAnchor(anchorID string, candidateIDs []string) ([]string, error)
	ListFollowers(userID string, limit, offset int) ([]*model.Following, error)

	// ListFollowersWithCursor is ListFollowers paginated by sinceId/untilId
	// instead of offset alone (users/followers)。
	//
	// **offset 版と分けてある。** fanout (`core/timeline`) / CSV export
	// (`core/transfer`) / admin (`api/admin`) / stream の followee snapshot
	// (`server/router.go`) は全件を順に舐めるので offset のままでよく、カーソルを
	// 足すと呼び出しが空文字だらけになる。**上限も違う** (fanout 200 / CSV 500)
	// ので、cursor 版の clamp を共有 body に置いてはいけない。
	ListFollowersWithCursor(followeeID, sinceID, untilID string, limit, offset int) ([]*model.Following, error)
	// ListFollowersToNotify returns Following rows where followeeId matches and
	// notify='normal'. Used by note.CreateService to fan out 'note'
	// notifications to followers who opted into post notifications (upstream
	// NoteCreateService の findBy({followeeId, notify:'normal'}))。
	ListFollowersToNotify(userID string) ([]*model.Following, error)
	// ListLocalFollowerIDs returns the followerIds of every local follower of
	// followeeID (= rows where followeeId matches and followerHost IS NULL).
	//
	// アカウント移行のフォロワー引き継ぎ (#2418) 用。upstream は
	// `findBy({followeeId, followerHost: IsNull(), followerId: Not(proxy.id)})`
	// で全件取る。ListFollowers は limit/offset 必須でページングが要るが、
	// 移行は 1 回きりの一括処理で件数上限も無いため、全件を 1 クエリで引く
	// 専用メソッドを分けている (ページング漏れが取りこぼしに直結するため)。
	// proxy account の除外は caller 側で行う。
	ListLocalFollowerIDs(followeeID string) ([]string, error)
	ListFollowing(userID string, limit, offset int) ([]*model.Following, error)

	// ListFollowingWithCursor is ListFollowing paginated by sinceId/untilId
	// instead of offset alone (users/following)。詳細は
	// ListFollowersWithCursor と対称。
	ListFollowingWithCursor(followerID, sinceID, untilID string, limit, offset int) ([]*model.Following, error)
	// ListFolloweeIDs returns every user id the given user follows.
	// HTL の「返信先が followers 限定の投稿」ガードで集合として使うため、
	// ページングせず全件返す。
	ListFolloweeIDs(followerID string) ([]string, error)
	// ListFollowingForList returns Following rows where followerID matches,
	// optionally filtered by `notify IS NOT NULL` (= notification=true) and
	// cursor-paginated by sinceID / untilID. Used by /api/following/list
	// (upstream 2026.5.2 #17385 + #17416).
	ListFollowingForList(followerID, sinceID, untilID string, notification bool, limit int) ([]*model.Following, error)
	// ListFollowersByHost returns Following rows whose followeeHost matches
	// host. Used by federation/followers. 本家 followers.ts は
	// `following.followeeHost = :host` で絞る (= 指定リモートホストの
	// ユーザーがフォローされている = followee 側がそのホスト) ので、列は
	// followeeHost。
	ListFollowersByHost(host string, limit, offset int) ([]*model.Following, error)
	// ListFollowingByHost returns Following rows whose followerHost matches
	// host. Used by federation/following. 本家 following.ts は
	// `following.followerHost = :host` で絞る (= 指定リモートホストの
	// ユーザーがフォローしている = follower 側がそのホスト) ので、列は
	// followerHost。
	ListFollowingByHost(host string, limit, offset int) ([]*model.Following, error)
	// ListFollowersByHostCursor is ListFollowersByHost with id cursor pagination
	// (sinceID/untilID) instead of offset. Used by federation/followers to match
	// upstream makePaginationQuery (#1732)。sinceID のみ指定時は id ASC、
	// それ以外は id DESC。
	//
	// viewer limits the rows to those whose owner (the followee) lets the
	// viewer see their followers list; see model.FollowListViewer.
	ListFollowersByHostCursor(host, sinceID, untilID string, limit int, viewer model.FollowListViewer) ([]*model.Following, error)
	// ListFollowingByHostCursor is ListFollowingByHost with id cursor pagination.
	// Used by federation/following (#1732)。
	//
	// viewer limits the rows to those whose owner (the follower) lets the
	// viewer see their following list; see model.FollowListViewer.
	ListFollowingByHostCursor(host, sinceID, untilID string, limit int, viewer model.FollowListViewer) ([]*model.Following, error)
	// ListFollowersBefore returns Following rows (followeeId = userID) with
	// id < cursor (cursor 空なら最新から), ordered id DESC, up to limit. AP
	// followers collection の cursor pagination 用 (#1877)。
	ListFollowersBefore(userID, cursor string, limit int) ([]*model.Following, error)
	// ListFollowingBefore returns Following rows (followerId = userID) with
	// id < cursor, ordered id DESC, up to limit. AP following collection 用 (#1877)。
	ListFollowingBefore(userID, cursor string, limit int) ([]*model.Following, error)
	// CountRemoteFollowees returns the number of Following rows whose
	// followeeHost is non-NULL (= remote users being followed by anyone).
	// federation/stats の allSubCount (upstream count where followeeHost is
	// not null) に対応 (#1544)。
	CountRemoteFollowees() (int64, error)
	// CountRemoteFollowers returns the number of Following rows whose
	// followerHost is non-NULL (= remote users following anyone). stats の
	// allPubCount (upstream count where followerHost is not null) に対応 (#1544)。
	CountRemoteFollowers() (int64, error)
	// UpdateRelation applies partial updates to a Following row identified
	// by (followerID, followeeID). Used by following/update.
	UpdateRelation(followerID, followeeID string, fields map[string]any) error
	// UpdateAllByFollower applies partial updates to every Following row
	// with the given follower. Used by following/update-all.
	UpdateAllByFollower(followerID string, fields map[string]any) error
	// DeleteAllByUser removes every Following row that involves userID as
	// either the follower or the followee. Returns the total affected rows.
	// Used by cascade account deletion.
	DeleteAllByUser(userID string) (int64, error)
	// ListRemoteFollowerInboxes returns the deduplicated inboxes for remote
	// followers of userID. sharedInbox を持つフォロワーは sharedInbox に集約され
	// (Shared=true)、無いフォロワーは個別 inbox (Shared=false)。Shared フラグは
	// deliver の goneSuspended 判定 (shared inbox の 410) に使う (#1811)。
	ListRemoteFollowerInboxes(userID string) ([]model.RemoteInbox, error)
	// ListFollowingByBirthday returns the followees of followerID whose
	// birthday (mmdd) falls in [beginMMDD, endMMDD] inclusive. Passing
	// beginMMDD > endMMDD treats the range as year-wrapping (e.g. 1225..0105).
	// Results are ordered by mmdd ascending. Used by
	// users/get-following-users-by-birthday.
	ListFollowingByBirthday(followerID string, beginMMDD, endMMDD, limit, offset int) ([]model.FollowingBirthday, error)
}

type followingRepository struct {
	db *gorm.DB
}

// NewFollowingRepository creates a new FollowingRepository.
func NewFollowingRepository(db *gorm.DB) FollowingRepository {
	return &followingRepository{db: db}
}

func (r *followingRepository) Create(f *model.Following) error {
	return r.db.Create(f).Error
}

func (r *followingRepository) Delete(f *model.Following) error {
	return r.db.Delete(f).Error
}

func (r *followingRepository) FindByPair(followerID, followeeID string) (*model.Following, error) {
	if !storable(followerID) || !storable(followeeID) {
		return nil, ErrNotFound
	}
	var f model.Following
	if err := r.db.Where("\"followerId\" = ? AND \"followeeId\" = ?", followerID, followeeID).First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *followingRepository) Exists(followerID, followeeID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.Following{}).
		Where("\"followerId\" = ? AND \"followeeId\" = ?", followerID, followeeID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *followingRepository) FilterFollowingsFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Following{}).
		Where(`"followerId" = ? AND "followeeId" IN ?`, anchorID, candidateIDs).
		Pluck(`"followeeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *followingRepository) FilterFollowingsToAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Following{}).
		Where(`"followerId" IN ? AND "followeeId" = ?`, candidateIDs, anchorID).
		Pluck(`"followerId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *followingRepository) ListFolloweeIDs(followerID string) ([]string, error) {
	if followerID == "" {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Following{}).
		Where(`"followerId" = ?`, followerID).
		Pluck(`"followeeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// ListFollowersWithCursor returns followers paginated by sinceId/untilId.
//
// **カーソルは SQL に掛ける。** LIMIT を掛けたあとに捨てると、2 ページ目の要求で
// DB が 1 ページ目と同じ行を返し、それを全部落とすので**空になる**。一覧が 1 ページ
// 分で止まり、プロフィールの followersCount と食い違って見える (#2711)。
func (r *followingRepository) ListFollowersWithCursor(followeeID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	return r.listRelationPage(`"followeeId" = ?`, followeeID, sinceID, untilID, clampRelationLimit(limit), offset)
}

// ListFollowingWithCursor is ListFollowersWithCursor for the follower side.
func (r *followingRepository) ListFollowingWithCursor(followerID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	return r.listRelationPage(`"followerId" = ?`, followerID, sinceID, untilID, clampRelationLimit(limit), offset)
}

// clampRelationLimit mirrors ListFollowingForList's bounds.
//
// **cursor 版にだけ掛ける。** 共有 body に置くと offset 版の呼び出し元
// (fanout 200 / CSV export 500) が 100 に切り詰められて取りこぼす。
// いまの唯一の呼び出し元は pagination.ResolveLimit が 1..100 を保証しているが、
// interface に生えた public method なので、limit=0 で 0 件・負値で LIMIT 句ごと
// 消えて全件、という壊れ方を塞いでおく (#2712 review LOW-3)。
func clampRelationLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// listRelationPage is the shared body of the relation lookups.
// 並び順は following/list (ListFollowingForList) と同じ paginationOrder に揃える。
func (r *followingRepository) listRelationPage(cond, anchorID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	q := r.db.Where(cond, anchorID)
	if sinceID != "" {
		q = q.Where(`id > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`id < ?`, untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	// cursor 指定時は offset 無視 (本家 makePaginationQuery と同じ)。この package の
	// 他 10 ファイル (clip / flash / page / gallery / announcement 等) と揃える。
	// 併用を許すと同じ入力でここだけ結果が変わる (#2712 review MEDIUM-3)。
	//
	// **同 package に反例がある** — emoji.go の ListWithFilter / ListRemoteWithFilter
	// は cursor 併用時も offset を掛ける。そちらは admin の絞り込み一覧で、
	// upstream に対応する paramDef が無い (#2712 review round 2 LOW-4)。
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Following
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListFollowers returns followers by offset. cursor 版と同じ body に委譲する
// (`paginationOrder("", "", "id")` == `id DESC` なので生成 SQL は変わらない)。
func (r *followingRepository) ListFollowers(userID string, limit, offset int) ([]*model.Following, error) {
	return r.listRelationPage(`"followeeId" = ?`, userID, "", "", limit, offset)
}

// ListFollowersToNotify returns Following rows where followeeId matches and
// notify='normal'. note 通知の fan-out 対象を絞る (#1559)。`notify` 列は
// null=OFF / 'normal'=ON という Misskey 互換 semantics で、TS NoteCreateService は
// findBy({followeeId, notify:'normal'}) と完全一致で絞るため値も 'normal' に揃える。
func (r *followingRepository) ListFollowersToNotify(userID string) ([]*model.Following, error) {
	var rows []*model.Following
	if err := r.db.Where(`"followeeId" = ? AND notify = ?`, userID, "normal").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *followingRepository) ListLocalFollowerIDs(followeeID string) ([]string, error) {
	var ids []string
	if err := r.db.Model(&model.Following{}).
		Where(`"followeeId" = ? AND "followerHost" IS NULL`, followeeID).
		Order(`"followerId"`).
		Pluck(`"followerId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// ListFollowing is ListFollowers for the follower side.
func (r *followingRepository) ListFollowing(userID string, limit, offset int) ([]*model.Following, error) {
	return r.listRelationPage(`"followerId" = ?`, userID, "", "", limit, offset)
}

// ListFollowingForList implements the cursor + notification-filter variant
// used by /api/following/list (upstream 2026.5.2 #17385 + #17416)。
//
// notification=true で `notify IS NOT NULL` 絞り込み。`notify` field 自体は
// 投稿通知の音声 / バイブ等の preference を保持する varchar(32) で、null
// なら通知 OFF / 非 null なら ON という Misskey 互換の semantics。
func (r *followingRepository) ListFollowingForList(followerID, sinceID, untilID string, notification bool, limit int) ([]*model.Following, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"followerId" = ?`, followerID)
	if notification {
		q = q.Where(`notify IS NOT NULL`)
	}
	if sinceID != "" {
		q = q.Where(`id > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`id < ?`, untilID)
	}
	var rows []*model.Following
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListFollowersByHost returns Following rows whose followeeHost matches the
// given remote host. 本家 federation/followers.ts と同じく followeeHost で絞る。
func (r *followingRepository) ListFollowersByHost(host string, limit, offset int) ([]*model.Following, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.Following
	if err := r.db.Where(`"followeeHost" = ?`, host).
		Order("id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListFollowingByHost returns Following rows whose followerHost matches the
// given remote host. 本家 federation/following.ts と同じく followerHost で絞る。
func (r *followingRepository) ListFollowingByHost(host string, limit, offset int) ([]*model.Following, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(host) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.Following
	if err := r.db.Where(`"followerHost" = ?`, host).
		Order("id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// followRelationVisibility restricts following rows to those the viewer may
// see under the list owner's visibility setting. ownerColumn is the owner's
// column on "following" ("followeeId" for a followers list, "followerId" for
// a following list) and visibilityColumn the owner's matching user_profile
// column. Mirrors upstream QueryService.generateFollowingRelationVisibilityQuery.
//
// 本家と同じく持ち主の user_profile を INNER JOIN するので、profile 行の無い
// 持ち主の行は出ない。条件は LIMIT より前 (同じ WHERE) に置く — 取ってから
// 間引くとページが欠け、カーソルも進まなくなる。
func followRelationVisibility(q *gorm.DB, ownerColumn, visibilityColumn string, viewer model.FollowListViewer) *gorm.DB {
	if viewer.Moderator {
		return q
	}
	owner := `"following".` + ownerColumn
	visibility := `"ownerProfile".` + visibilityColumn
	q = q.Joins(`INNER JOIN "user_profile" "ownerProfile" ON "ownerProfile"."userId" = ` + owner)
	if viewer.UserID == "" {
		return q.Where(visibility + ` = 'public'`)
	}
	return q.Where(
		`(`+visibility+` = 'public'`+
			` OR `+owner+` = ?`+
			` OR (`+visibility+` = 'followers' AND `+owner+` IN (SELECT "meFollowing"."followeeId" FROM "following" "meFollowing" WHERE "meFollowing"."followerId" = ?)))`,
		viewer.UserID, viewer.UserID)
}

// ListFollowersByHostCursor is the cursor-paginated variant of
// ListFollowersByHost (federation/followers, #1732)。
func (r *followingRepository) ListFollowersByHostCursor(host, sinceID, untilID string, limit int, viewer model.FollowListViewer) ([]*model.Following, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(host) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Model(&model.Following{}).Select(`"following".*`).Where(`"following"."followeeHost" = ?`, host)
	q = followRelationVisibility(q, `"followeeId"`, `"followersVisibility"`, viewer)
	if sinceID != "" {
		q = q.Where(`"following"."id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"following"."id" < ?`, untilID)
	}
	var rows []*model.Following
	if err := q.Order(paginationOrder(sinceID, untilID, `"following"."id"`)).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListFollowingByHostCursor is the cursor-paginated variant of
// ListFollowingByHost (federation/following, #1732)。
func (r *followingRepository) ListFollowingByHostCursor(host, sinceID, untilID string, limit int, viewer model.FollowListViewer) ([]*model.Following, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(host) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Model(&model.Following{}).Select(`"following".*`).Where(`"following"."followerHost" = ?`, host)
	q = followRelationVisibility(q, `"followerId"`, `"followingVisibility"`, viewer)
	if sinceID != "" {
		q = q.Where(`"following"."id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"following"."id" < ?`, untilID)
	}
	var rows []*model.Following
	if err := q.Order(paginationOrder(sinceID, untilID, `"following"."id"`)).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *followingRepository) ListFollowersBefore(userID, cursor string, limit int) ([]*model.Following, error) {
	q := r.db.Where(`"followeeId" = ?`, userID)
	if cursor != "" {
		q = q.Where("id < ?", cursor)
	}
	var rows []*model.Following
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *followingRepository) ListFollowingBefore(userID, cursor string, limit int) ([]*model.Following, error) {
	q := r.db.Where(`"followerId" = ?`, userID)
	if cursor != "" {
		q = q.Where("id < ?", cursor)
	}
	var rows []*model.Following
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *followingRepository) CountRemoteFollowees() (int64, error) {
	var n int64
	if err := r.db.Model(&model.Following{}).
		Where(`"followeeHost" IS NOT NULL`).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

func (r *followingRepository) CountRemoteFollowers() (int64, error) {
	var n int64
	if err := r.db.Model(&model.Following{}).
		Where(`"followerHost" IS NOT NULL`).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// UpdateRelation applies a partial field update to a single (follower, followee)
// Following row.
func (r *followingRepository) UpdateRelation(followerID, followeeID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Following{}).
		Where(`"followerId" = ? AND "followeeId" = ?`, followerID, followeeID).
		Updates(fields).Error
}

// UpdateAllByFollower applies a partial field update to every Following row
// authored by the given follower.
func (r *followingRepository) UpdateAllByFollower(followerID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Following{}).
		Where(`"followerId" = ?`, followerID).
		Updates(fields).Error
}

// DeleteAllByUser removes every Following row whose follower or followee is
// userID.
func (r *followingRepository) DeleteAllByUser(userID string) (int64, error) {
	tx := r.db.Where(`"followerId" = ? OR "followeeId" = ?`, userID, userID).
		Delete(&model.Following{})
	return tx.RowsAffected, tx.Error
}

// ListRemoteFollowerInboxes returns deduplicated inbox URLs for remote
// followers. sharedInbox を優先し、無い場合のみ個別 inbox を使う。
//
// SQLは以下の流れ:
//  1. follower がリモート (host IS NOT NULL) のフォロワーをjoin
//  2. sharedInbox があれば sharedInbox、無ければ inbox を選択
//  3. NULL/空文字列を除外し DISTINCT で重複排除
func (r *followingRepository) ListRemoteFollowerInboxes(userID string) ([]model.RemoteInbox, error) {
	var rows []struct {
		Inbox  string
		Shared bool
	}
	// shared フラグは sharedInbox が非空のとき true (= COALESCE で sharedInbox 側を
	// 選んだ行)。同 shared inbox を共有する複数フォロワーは DISTINCT で 1 行に集約。
	err := r.db.
		Table(`"following" AS f`).
		Select(`DISTINCT COALESCE(NULLIF(u."sharedInbox", ''), u.inbox) AS inbox, (u."sharedInbox" IS NOT NULL AND u."sharedInbox" != '') AS shared`).
		Joins(`JOIN "user" u ON u.id = f."followerId"`).
		Where(`f."followeeId" = ? AND u.host IS NOT NULL AND (u."sharedInbox" IS NOT NULL OR u.inbox IS NOT NULL)`, userID).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	// COALESCE が NULL を返す可能性があるため、空文字を除去
	out := make([]model.RemoteInbox, 0, len(rows))
	for _, row := range rows {
		if row.Inbox != "" {
			out = append(out, model.RemoteInbox{Inbox: row.Inbox, Shared: row.Shared})
		}
	}
	return out, nil
}

// ListFollowingByBirthday returns followees whose birthday mmdd falls within
// the given range. user_profile.birthday は "YYYY-MM-DD" の char(10) として
// 保存されているため、SUBSTRING で月日を切り出して整数化する。
func (r *followingRepository) ListFollowingByBirthday(followerID string, beginMMDD, endMMDD, limit, offset int) ([]model.FollowingBirthday, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// PostgreSQL SUBSTRING(text FROM start FOR length) は 1-indexed。
	// "YYYY-MM-DD" のうち MM は 6-7 文字目、DD は 9-10 文字目。
	const mmddExpr = `(CAST(SUBSTRING(p.birthday FROM 6 FOR 2) AS INTEGER) * 100 + CAST(SUBSTRING(p.birthday FROM 9 FOR 2) AS INTEGER))`
	q := r.db.
		Table(`"following" AS f`).
		Select(`f."followeeId", p.birthday`).
		Joins(`JOIN "user_profile" p ON p."userId" = f."followeeId"`).
		Where(`f."followerId" = ? AND p.birthday IS NOT NULL`, followerID)
	if beginMMDD <= endMMDD {
		q = q.Where(mmddExpr+` BETWEEN ? AND ?`, beginMMDD, endMMDD)
	} else {
		// 年跨ぎ (例: 12/25 -> 1/5) の場合は begin..1231 と 101..end を OR。
		q = q.Where(`(`+mmddExpr+` BETWEEN ? AND 1231 OR `+mmddExpr+` BETWEEN 101 AND ?)`, beginMMDD, endMMDD)
	}
	var rows []model.FollowingBirthday
	if err := q.Order(mmddExpr + ` ASC`).Limit(limit).Offset(offset).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
