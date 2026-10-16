package repository

import (
	"database/sql"
	"time"

	"gorm.io/gorm"
)

// UserRelationState is the viewer->target relation read by
// UserRelationReader. Each flag means exactly what the per-table Exists of
// the matching repository reports for the same pair (muting counts only
// mutes that have not expired).
type UserRelationState struct {
	// IsFollowing reports a following row viewer->target. FollowNotify and
	// FollowWithReplies are that row's columns (nil / false without a row).
	IsFollowing       bool
	FollowNotify      *string
	FollowWithReplies bool
	// IsFollowed reports a following row target->viewer.
	IsFollowed bool
	IsBlocking bool
	IsBlocked  bool
	// IsMuted reports a mute viewer->target whose expiresAt is NULL or later
	// than the time passed to ReadUserRelation.
	IsMuted       bool
	IsRenoteMuted bool
	// HasPendingFrom / HasPendingTo report a follow request viewer->target /
	// target->viewer.
	HasPendingFrom bool
	HasPendingTo   bool
	// HasMemo reports a user_memo row of the viewer about the target; Memo
	// is its text (which may be empty).
	HasMemo bool
	Memo    string
}

// UserRelationReader reads the whole viewer->target relation block of
// users/show in one statement. The values do not go through the per-table
// repositories; see userRelationSQL for the methods whose conditions it
// copies.
type UserRelationReader interface {
	// ReadUserRelation returns the relation from viewerID to targetID. now is
	// the instant mutes are checked against.
	ReadUserRelation(viewerID, targetID string, now time.Time) (UserRelationState, error)
}

type userRelationRepository struct{ db *gorm.DB }

// NewUserRelationRepository constructs a UserRelationReader backed by db.
func NewUserRelationRepository(db *gorm.DB) UserRelationReader {
	return &userRelationRepository{db: db}
}

// userRelationSQL は users/show の関係を 1 文で引く。以前は表ごとの Exists を
// 10 本の goroutine で同時に投げており、1 要求で DB の接続を 10 本使っていた
// (#3608)。値は各 repo を通らないので、条件は次のメソッドの WHERE と同じに
// する。片方を変えたらもう片方も変える:
//   - following: followingRepository.Exists / FindByPair (following.go)
//   - blocking: blockingRepository.Exists (blocking.go)
//   - muting: mutingRepository.Exists (muting.go。期限の条件を含む)
//   - renote_muting: renoteMutingRepository.Exists (muting.go)
//   - follow_request: followRequestRepository.Exists (follow_request.go)
//   - user_memo: userMemoRepository.FindByPair (user_memo.go)
//
// following と user_memo はどちらも (from, to) が一意だが、TS 版の DB から
// 引き継いだときに索引の形が違っても行が増えないよう LIMIT 1 で受ける。
const userRelationSQL = `SELECT
	f."id" IS NOT NULL,
	f."notify",
	COALESCE(f."withReplies", false),
	EXISTS(SELECT 1 FROM "following" WHERE "followerId" = @target AND "followeeId" = @viewer),
	EXISTS(SELECT 1 FROM "blocking" WHERE "blockerId" = @viewer AND "blockeeId" = @target),
	EXISTS(SELECT 1 FROM "blocking" WHERE "blockerId" = @target AND "blockeeId" = @viewer),
	EXISTS(SELECT 1 FROM "muting" WHERE "muterId" = @viewer AND "muteeId" = @target AND ("expiresAt" IS NULL OR "expiresAt" > @now)),
	EXISTS(SELECT 1 FROM "renote_muting" WHERE "muterId" = @viewer AND "muteeId" = @target),
	EXISTS(SELECT 1 FROM "follow_request" WHERE "followerId" = @viewer AND "followeeId" = @target),
	EXISTS(SELECT 1 FROM "follow_request" WHERE "followerId" = @target AND "followeeId" = @viewer),
	m."id" IS NOT NULL,
	COALESCE(m."memo", '')
FROM (SELECT 1) AS "one"
LEFT JOIN LATERAL (
	SELECT "id", "notify", "withReplies" FROM "following"
	WHERE "followerId" = @viewer AND "followeeId" = @target LIMIT 1
) AS f ON true
LEFT JOIN LATERAL (
	SELECT "id", "memo" FROM "user_memo"
	WHERE "userId" = @viewer AND "targetUserId" = @target LIMIT 1
) AS m ON true`

func (r *userRelationRepository) ReadUserRelation(viewerID, targetID string, now time.Time) (UserRelationState, error) {
	var s UserRelationState
	// NUL を含む id はどの行とも一致しないうえ、bind するとクエリごと落ちる
	// (nul_guard.go)。一致しえないので「関係なし」が正しい答え。
	if !storable(viewerID) || !storable(targetID) {
		return s, nil
	}
	var notify sql.NullString
	err := r.db.Raw(userRelationSQL, map[string]any{
		"viewer": viewerID,
		"target": targetID,
		"now":    now,
	}).Row().Scan(
		&s.IsFollowing, &notify, &s.FollowWithReplies,
		&s.IsFollowed,
		&s.IsBlocking, &s.IsBlocked,
		&s.IsMuted, &s.IsRenoteMuted,
		&s.HasPendingFrom, &s.HasPendingTo,
		&s.HasMemo, &s.Memo,
	)
	if err != nil {
		return UserRelationState{}, err
	}
	if notify.Valid {
		v := notify.String
		s.FollowNotify = &v
	}
	return s, nil
}
