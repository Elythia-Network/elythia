package repository

import "gorm.io/gorm"

// MeUnreadFlags is the part of the /api/i unread block that lives in
// PostgreSQL tables and is a plain yes/no per user.
//
// The values do not go through the per-table repositories. When the
// condition of one of the methods named below changes, change
// meUnreadFlagsSQL with it.
type MeUnreadFlags struct {
	// HasPendingReceivedFollowRequest: FollowRequestRepository.CountReceived > 0
	// (follow_request.go).
	HasPendingReceivedFollowRequest bool
	// HasUnreadChatMessages: ChatRepository.CountUnread > 0 (chat.go).
	HasUnreadChatMessages bool
	// HasUnreadAntenna: AntennaNoteUnreadRepository.HasAnyByUser (unread.go).
	HasUnreadAntenna bool
	// HasUnreadChannel: ChannelNoteUnreadRepository.HasAnyByUser (unread.go).
	HasUnreadChannel bool
	// HasUnreadSpecifiedNotes: NoteUnreadRepository.HasAnySpecified
	// (unread.go), which notification.Service.UnreadSummary reads when it has
	// a note_unread repository.
	HasUnreadSpecifiedNotes bool
}

// MeUnreadFlagsReader reads MeUnreadFlags in one statement.
type MeUnreadFlagsReader interface {
	ReadMeUnreadFlags(userID string) (MeUnreadFlags, error)
}

type meUnreadFlagsRepository struct{ db *gorm.DB }

// NewMeUnreadFlagsRepository constructs a MeUnreadFlagsReader backed by db.
func NewMeUnreadFlagsRepository(db *gorm.DB) MeUnreadFlagsReader {
	return &meUnreadFlagsRepository{db: db}
}

// meUnreadFlagsSQL は /api/i の未読の真偽値を 1 文で引く。以前は表ごとに
// 1 本ずつ順に投げていた (#3608)。条件は次のメソッドの WHERE と同じにする。
// 片方を変えたらもう片方も変える:
//   - follow_request: followRequestRepository.CountReceived (follow_request.go)
//   - chat_message: chatRepository.CountUnread (chat.go)
//   - antenna_note_unread / channel_note_unread: HasAnyByUser (unread.go)
//   - note_unread: noteUnreadRepository.HasAnySpecified (unread.go)
//
// 件数は「1 件以上か」しか使わないので count ではなく EXISTS で見る。
//
// 名前付きの bind (@user) は使わない。gorm は名前の終わりを空白や ) でしか
// 判定しないので、ARRAY[@user] の ] を名前の一部として読んでしまう。`@>` が
// 含まれる時点で gorm は名前付きとして解釈するが、? は順に bind される。
const meUnreadFlagsSQL = `SELECT
	EXISTS(SELECT 1 FROM "follow_request" WHERE "followeeId" = ?),
	EXISTS(SELECT 1 FROM "chat_message" WHERE "toUserId" = ? AND NOT ("reads" @> ARRAY[?]::varchar[])),
	EXISTS(SELECT 1 FROM "antenna_note_unread" WHERE "userId" = ?),
	EXISTS(SELECT 1 FROM "channel_note_unread" WHERE "userId" = ?),
	EXISTS(SELECT 1 FROM "note_unread" WHERE "userId" = ? AND "isSpecified" = true)`

func (r *meUnreadFlagsRepository) ReadMeUnreadFlags(userID string) (MeUnreadFlags, error) {
	var f MeUnreadFlags
	if !storable(userID) {
		return f, nil
	}
	err := r.db.Raw(meUnreadFlagsSQL, userID, userID, userID, userID, userID, userID).Row().Scan(
		&f.HasPendingReceivedFollowRequest,
		&f.HasUnreadChatMessages,
		&f.HasUnreadAntenna,
		&f.HasUnreadChannel,
		&f.HasUnreadSpecifiedNotes,
	)
	if err != nil {
		return MeUnreadFlags{}, err
	}
	return f, nil
}
