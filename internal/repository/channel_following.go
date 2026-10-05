package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ChannelFollowingRepository provides data access for the
// `channel_following` table.
type ChannelFollowingRepository interface {
	Create(f *model.ChannelFollowing) error
	Delete(f *model.ChannelFollowing) error
	FindByPair(followerID, channelID string) (*model.ChannelFollowing, error)
	Exists(followerID, channelID string) (bool, error)
	// ExistsMany returns a channelID → bool map indicating which of the
	// given channels followerID currently follows. Used by /channels list
	// endpoints to embed isFollowing without N+1 (#522).
	ExistsMany(followerID string, channelIDs []string) (map[string]bool, error)
	// ListFollowed returns channel_following rows for userID with cursor
	// (sinceID/untilID) or offset pagination. Cursor 指定時は offset 無視。
	ListFollowed(userID, sinceID, untilID string, limit, offset int) ([]*model.ChannelFollowing, error)
	// ListFollowerIDsPage returns a batch of follower userIds for a channel,
	// ordered by row id ASC. afterRowID は前ページの nextCursor を渡す
	// (空なら先頭から)。返り値の nextCursor は次回 call にそのまま渡せる
	// (空なら全件走査済み)。#320 の channel unread fanout で使用。
	ListFollowerIDsPage(channelID, afterRowID string, limit int) (ids []string, nextCursor string, err error)
}

type channelFollowingRepository struct {
	db *gorm.DB
}

// NewChannelFollowingRepository creates a new ChannelFollowingRepository.
func NewChannelFollowingRepository(db *gorm.DB) ChannelFollowingRepository {
	return &channelFollowingRepository{db: db}
}

func (r *channelFollowingRepository) Create(f *model.ChannelFollowing) error {
	return r.db.Create(f).Error
}

func (r *channelFollowingRepository) Delete(f *model.ChannelFollowing) error {
	return r.db.Delete(f).Error
}

func (r *channelFollowingRepository) FindByPair(followerID, channelID string) (*model.ChannelFollowing, error) {
	if !storable(followerID) || !storable(channelID) {
		return nil, ErrNotFound
	}
	var f model.ChannelFollowing
	if err := r.db.Where("\"followerId\" = ? AND \"followeeId\" = ?", followerID, channelID).First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *channelFollowingRepository) Exists(followerID, channelID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.ChannelFollowing{}).
		Where("\"followerId\" = ? AND \"followeeId\" = ?", followerID, channelID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// ExistsMany resolves "follower follows X?" for a set of channel IDs in a
// single query. 空入力 / 空 followerID は空 map を返す。
func (r *channelFollowingRepository) ExistsMany(followerID string, channelIDs []string) (map[string]bool, error) {
	if followerID == "" || len(channelIDs) == 0 {
		return map[string]bool{}, nil
	}
	var followed []string
	if err := r.db.Model(&model.ChannelFollowing{}).
		Where(`"followerId" = ? AND "followeeId" IN ?`, followerID, channelIDs).
		Pluck(`"followeeId"`, &followed).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(followed))
	for _, id := range followed {
		out[id] = true
	}
	return out, nil
}

// ListFollowerIDsPage returns a batch of follower userIds for a channel,
// ordered by row id ASC. Used by channel-note fanout (#320) to walk all
// followers without the previous 1000-row cap. Returns (ids, nextCursor,
// err). nextCursor is the last row's id; pass it back to continue.
// 空 slice + 空 cursor で全件走査完了。
func (r *channelFollowingRepository) ListFollowerIDsPage(channelID, afterRowID string, limit int) ([]string, string, error) {
	if limit <= 0 {
		limit = 500
	}
	type row struct {
		ID         string `gorm:"column:id"`
		FollowerID string `gorm:"column:followerId"`
	}
	var rows []row
	q := r.db.Model(&model.ChannelFollowing{}).
		Select(`"id", "followerId"`).
		Where(`"followeeId" = ?`, channelID).
		Order(`"id" ASC`).
		Limit(limit)
	if afterRowID != "" {
		q = q.Where(`"id" > ?`, afterRowID)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.FollowerID)
	}
	return ids, rows[len(rows)-1].ID, nil
}

// ListFollowed returns the channel followings for a given user, ordered by
// followeeId DESC by default with cursor or offset pagination support.
//
// 注意: cursor (sinceID/untilID) は **followeeId** (= channel.id) を基準に
// する。frontend Paginator は response の `id` (= channel.id を返している)
// を次ページの cursor として送ってくるので、ここで channel_following.id
// を使うと domain mismatch で次ページが空になる (#520 review 指摘)。
// upstream Misskey の channels/followed.ts も makePaginationQuery の 6 番目
// 引数に 'followeeId' を渡して同じ列で paginate している。
func (r *channelFollowingRepository) ListFollowed(userID, sinceID, untilID string, limit, offset int) ([]*model.ChannelFollowing, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"followerId" = ?`, userID)
	if sinceID != "" {
		q = q.Where(`"followeeId" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"followeeId" < ?`, untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, `"followeeId"`)).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.ChannelFollowing
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
