package maintenance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/misc/redisglob"
)

// ChatConversation is one direct-message conversation seen from the reader:
// the messages SenderID sent to ReaderID.
type ChatConversation struct {
	ReaderID string
	SenderID string
}

// ChatReadState is what the source server recorded about a conversation.
type ChatReadState int

const (
	// ChatReadStateRead means the source server had nothing unread in the
	// conversation, so its messages are marked read.
	ChatReadStateRead ChatReadState = iota
	// ChatReadStateUnread means the source server still had the conversation
	// unread. Its messages are left as they are.
	ChatReadStateUnread
	// ChatReadStateBroken means a record of the conversation could not be
	// interpreted (a key of the wrong type). Its messages are left as they are.
	ChatReadStateBroken
)

// String names the state in the report.
func (s ChatReadState) String() string {
	switch s {
	case ChatReadStateRead:
		return "read"
	case ChatReadStateUnread:
		return "unread"
	default:
		return "broken"
	}
}

// ChatReadRecord is what the source server recorded about one conversation.
type ChatReadRecord struct {
	State ChatReadState
	// MarkerID is the value of the marker (the id of the latest message the
	// source server delivered in the conversation), "" when there is none.
	MarkerID string
}

// ChatReadStateSource tells, per conversation, whether the source server
// still had it unread.
type ChatReadStateSource interface {
	States(ctx context.Context, convs []ChatConversation) (map[ChatConversation]ChatReadRecord, error)
}

// AssumeReadSource treats every conversation as read. It is for a migration
// whose Redis records are gone.
type AssumeReadSource struct{}

// States reports every conversation as read.
func (AssumeReadSource) States(_ context.Context, convs []ChatConversation) (map[ChatConversation]ChatReadRecord, error) {
	out := make(map[ChatConversation]ChatReadRecord, len(convs))
	for _, c := range convs {
		out[c] = ChatReadRecord{State: ChatReadStateRead}
	}
	return out, nil
}

// ErrNoChatReadRecords is returned by RedisChatReadStateSource when Redis
// holds no chat read record of upstream at all under the prefix.
var ErrNoChatReadRecords = errors.New("redis holds no chat read records under the prefix")

// RedisChatReadStateSource reads the unread records that Misskey and
// CherryPick keep in Redis.
//
// 本家 (2026.10.0) と CherryPick の ChatService は、DM を受け取ると
// `newUserChatMessageExists:<reader>:<sender>` (値は最新の未読のメッセージの id) を
// 置き、`newChatMessagesExists:<reader>` (set) に `user:<sender>` を足す。会話を
// 読むと両方を消す (readUserChatMessage)。`chat_message.reads` は作るときの空の
// 配列のまま一度も書かない。キーには ioredis の keyPrefix (`<redis.prefix>:`、
// 未設定なら URL の host) が付く。
type RedisChatReadStateSource struct {
	Client redis.Cmdable
	// Prefix is prepended to every key, as config.RedisOptions.KeyPrefix
	// returns it ("<prefix>:").
	Prefix string

	checked bool
	cache   map[ChatConversation]ChatReadRecord
}

func (s *RedisChatReadStateSource) markerKey(c ChatConversation) string {
	return s.Prefix + "newUserChatMessageExists:" + c.ReaderID + ":" + c.SenderID
}

func (s *RedisChatReadStateSource) setKey(readerID string) string {
	return s.Prefix + "newChatMessagesExists:" + readerID
}

// hasAnyRecord reports whether Redis holds at least one key of either shape.
func (s *RedisChatReadStateSource) hasAnyRecord(ctx context.Context) (bool, error) {
	for _, pattern := range []string{"newUserChatMessageExists:*", "newChatMessagesExists:*"} {
		var cursor uint64
		for {
			keys, next, err := s.Client.Scan(ctx, cursor, redisglob.Escape(s.Prefix)+pattern, 1000).Result()
			if err != nil {
				return false, err
			}
			if len(keys) > 0 {
				return true, nil
			}
			if next == 0 {
				break
			}
			cursor = next
		}
	}
	return false, nil
}

// States looks the conversations up in Redis.
//
// **未読とみなすのは、2 つの記録がどちらも未読を示す会話だけにする。** 本家の
// 「全チャットを既読にする」(readAllChatMessages) は set だけを消して marker を
// 残すので、marker だけを見ると、利用者が既読にした会話を未読に戻してしまう。
// 本家でバッジを決めるのは set (hasUnreadMessages) で、Elythia の /api/i に当たる。
//
// **読めない記録は既読にしない。** 型の違うキーがあれば、その会話は触らずに
// broken として報告する。接続の失敗はエラーで返して止める (黙って全部既読に
// 倒すと、取り戻せない)。
//
// **Redis に記録が 1 つも無ければ ErrNoChatReadRecords で止める。** prefix の
// 取り違えや、記録が消えた Redis を指していると、全会話が「記録が無い = 既読」に
// なる。それでよい場合 (全部読まれていた、記録が残っていない) は
// AssumeReadSource を使わせる。
//
// marker の値 (本家が最後に届けたメッセージの id) も返す。本家は新着のたびに
// marker を上書きするので、それより (幅を越えて) 新しいメッセージは本家が
// 受けていない (= -before が移行より遅い) ことの証拠になる。
func (s *RedisChatReadStateSource) States(ctx context.Context, convs []ChatConversation) (map[ChatConversation]ChatReadRecord, error) {
	if !s.checked {
		ok, err := s.hasAnyRecord(ctx)
		if err != nil {
			return nil, fmt.Errorf("scan redis: %w", err)
		}
		if !ok {
			return nil, ErrNoChatReadRecords
		}
		s.checked = true
	}
	if s.cache == nil {
		s.cache = make(map[ChatConversation]ChatReadRecord)
	}

	// 呼び出し側 (BackfillChatReadsBatch) は会話を一意にしてから渡す。
	var missing []ChatConversation
	for _, c := range convs {
		if _, ok := s.cache[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		pipe := s.Client.Pipeline()
		markers := make([]*redis.StringCmd, len(missing))
		members := make([]*redis.BoolCmd, len(missing))
		for i, c := range missing {
			markers[i] = pipe.Get(ctx, s.markerKey(c))
			members[i] = pipe.SIsMember(ctx, s.setKey(c.ReaderID), "user:"+c.SenderID)
		}
		// Exec は redis.Nil (marker が無い) もエラーとして返すので、判定は
		// コマンドごとに行う。
		_, _ = pipe.Exec(ctx)
		for i, c := range missing {
			state, err := chatReadState(markers[i].Err(), members[i])
			if err != nil {
				return nil, fmt.Errorf("read redis for reader %s sender %s: %w", c.ReaderID, c.SenderID, err)
			}
			rec := ChatReadRecord{State: state}
			if state != ChatReadStateBroken && markers[i].Err() == nil {
				rec.MarkerID = markers[i].Val()
			}
			s.cache[c] = rec
		}
	}

	out := make(map[ChatConversation]ChatReadRecord, len(convs))
	for _, c := range convs {
		out[c] = s.cache[c]
	}
	return out, nil
}

// chatReadState combines the results of the marker GET and the set
// SISMEMBER. It returns an error only when Redis could not be reached.
func chatReadState(markerErr error, member *redis.BoolCmd) (ChatReadState, error) {
	markerPresent := markerErr == nil
	switch {
	case markerErr == nil, errors.Is(markerErr, redis.Nil):
	case isRedisReply(markerErr):
		return ChatReadStateBroken, nil
	default:
		return 0, markerErr
	}
	inSet, err := member.Result()
	if err != nil {
		if isRedisReply(err) {
			return ChatReadStateBroken, nil
		}
		return 0, err
	}
	if markerPresent && inSet {
		return ChatReadStateUnread, nil
	}
	return ChatReadStateRead, nil
}

// isRedisReply reports whether err is an error reply of the server (for
// example WRONGTYPE), as opposed to a failure to talk to it.
func isRedisReply(err error) bool {
	var re redis.Error
	return errors.As(err, &re) && !errors.Is(err, redis.Nil)
}

// ChatReadsBackfillOptions configures BackfillChatReadsBatch.
type ChatReadsBackfillOptions struct {
	// Before is when the server was migrated. Only messages whose id was
	// generated before it are considered.
	Before time.Time
	// ParseTime extracts the creation time from a message id (the id
	// generator of the configuration).
	ParseTime func(id string) (time.Time, error)
	// Source tells whether each conversation is still unread.
	Source ChatReadStateSource
	// DryRun counts what would change without writing.
	DryRun bool
	// TooLateMargin is how much newer than the marker of the source server a
	// message must be to count as evidence that Before is too late.
	TooLateMargin time.Duration
}

// ChatReadsConversationResult counts the messages of one conversation in a
// batch.
type ChatReadsConversationResult struct {
	Conversation ChatConversation
	State        ChatReadState
	// Marked is the number of messages marked read (in a dry-run: that would
	// be).
	Marked int
	// Kept is the number of messages left unread because the conversation is
	// unread or broken.
	Kept int
}

// ChatReadsBackfillResult reports the outcome of one keyset batch.
type ChatReadsBackfillResult struct {
	// Scanned is the number of direct messages to local users that the
	// recipient had not read, inspected this batch.
	Scanned int
	// LastID is the greatest message id seen; "" when no rows remained.
	LastID string
	// AfterCutoff counts messages created at or after Before (left alone).
	AfterCutoff int
	// Unparsable counts messages whose id time could not be read (left alone).
	Unparsable int
	// Conversations lists the conversations of the messages created before
	// Before, ordered by reader and sender.
	Conversations []ChatReadsConversationResult
	// Newest is the creation time of the newest message created before
	// Before; zero when there was none.
	Newest time.Time
	// TooLate lists the conversations holding messages more than TooLateMargin
	// newer than the marker of the source server. When it is not empty
	// nothing was written.
	TooLate []ChatReadsTooLate
}

// ChatReadsTooLate is a conversation whose messages show that Before is later
// than the migration: the source server never saw them.
type ChatReadsTooLate struct {
	Conversation ChatConversation
	MarkerID     string
	// Newer is the number of messages more than the margin newer than
	// MarkerID and older than Before.
	Newer int
	// MarkerAt is the time read from MarkerID; NewestAt is that of the newest
	// of those messages. Operators compare them to tell a late -before from a
	// false alarm.
	MarkerAt time.Time
	NewestAt time.Time
}

// ErrChatReadsCutoffTooLate is returned when a batch finds messages created
// after the source server stopped but before Before.
var ErrChatReadsCutoffTooLate = errors.New("-before is later than the migration: direct messages newer than the records of the source server are older than it")

// chatReadsCandidateSQL lists the direct messages to local users whose
// recipient is not in `reads`.
//
// ルーム (`toRoomId`) は対象にしない。ルームのメッセージは `toUserId` が NULL
// なので、user との JOIN で外れる。/api/i の hasUnreadChatMessages はルームを
// 数えない (`"toUserId" = ?` だけ) ので、埋めても未読の表示は変わらない。宛先が
// リモートの利用者のメッセージも対象にしない。本家はリモートの宛先に未読の記録を
// 作らず、Elythia も既読を書かない。
const chatReadsCandidateSQL = `
SELECT m.id, m."fromUserId" AS from_user_id, m."toUserId" AS to_user_id
FROM "chat_message" m
JOIN "user" u ON u.id = m."toUserId"
WHERE m.id > ?
  AND u.host IS NULL
  AND NOT (m."reads" @> ARRAY[m."toUserId"])
ORDER BY m.id ASC
LIMIT ?`

// chatReadsMarkSQL adds the recipient to `reads` of the given messages.
//
// 条件を書く文の側でも見直すので、2 回走らせても受け手を二重に足さない。
// 走らせている間に Elythia が既読にした行も同じく外れる。
const chatReadsMarkSQL = `
UPDATE "chat_message"
SET "reads" = array_append("reads", "toUserId")
WHERE id IN ?
  AND NOT ("reads" @> ARRAY["toUserId"])`

// BackfillChatReadsBatch carries the read state of direct messages from the
// Redis records of Misskey / CherryPick into `chat_message.reads` for one
// keyset batch (#3653).
//
// 本家と CherryPick は既読を Redis にだけ持ち、`reads` を書かない。Elythia は
// 既読を `reads` で判定するので、移行した DB では受け取った DM がすべて未読になる。
// 移行した時点 (Before) より前に作られた DM について、移行元が未読の記録を
// 持っていない会話のメッセージを既読にする。Before より後の DM は Elythia が
// `reads` を書いているので触らない。
//
// 冪等。`LastID` を次回の fromID に渡せば再開できる。
func BackfillChatReadsBatch(ctx context.Context, db *gorm.DB, fromID string, batchSize int, opts ChatReadsBackfillOptions) (ChatReadsBackfillResult, error) {
	batchSize = clampBatchSize(batchSize)

	type candidate struct {
		ID         string
		FromUserID string
		ToUserID   string
	}
	var rows []candidate
	if err := db.WithContext(ctx).Raw(chatReadsCandidateSQL, fromID, batchSize).Scan(&rows).Error; err != nil {
		return ChatReadsBackfillResult{}, err
	}
	res := ChatReadsBackfillResult{Scanned: len(rows)}
	if len(rows) == 0 {
		return res, nil
	}
	res.LastID = rows[len(rows)-1].ID

	byConv := make(map[ChatConversation][]string)
	var convs []ChatConversation
	for _, r := range rows {
		t, err := opts.ParseTime(r.ID)
		if err != nil {
			res.Unparsable++
			continue
		}
		if !t.Before(opts.Before) {
			res.AfterCutoff++
			continue
		}
		if t.After(res.Newest) {
			res.Newest = t
		}
		c := ChatConversation{ReaderID: r.ToUserID, SenderID: r.FromUserID}
		if _, ok := byConv[c]; !ok {
			convs = append(convs, c)
		}
		byConv[c] = append(byConv[c], r.ID)
	}
	if len(convs) == 0 {
		return res, nil
	}

	states, err := opts.Source.States(ctx, convs)
	if err != nil {
		return ChatReadsBackfillResult{}, err
	}

	var markIDs []string
	slices.SortFunc(convs, func(a, b ChatConversation) int {
		if c := strings.Compare(a.ReaderID, b.ReaderID); c != 0 {
			return c
		}
		return strings.Compare(a.SenderID, b.SenderID)
	})
	for _, c := range convs {
		ids := byConv[c]
		rec := states[c]
		if rec.MarkerID != "" {
			late := ChatReadsTooLate{Conversation: c, MarkerID: rec.MarkerID}
			for _, msgID := range ids {
				if !chatMessageNewer(opts.ParseTime, msgID, rec.MarkerID, opts.TooLateMargin) {
					continue
				}
				late.Newer++
				// chatMessageNewer が true なら、どちらの id も読める。
				if t, _ := opts.ParseTime(msgID); t.After(late.NewestAt) {
					late.NewestAt = t
				}
			}
			if late.Newer > 0 {
				late.MarkerAt, _ = opts.ParseTime(rec.MarkerID)
				res.TooLate = append(res.TooLate, late)
			}
		}
		cr := ChatReadsConversationResult{Conversation: c, State: rec.State}
		if cr.State == ChatReadStateRead {
			cr.Marked = len(ids)
			markIDs = append(markIDs, ids...)
		} else {
			cr.Kept = len(ids)
		}
		res.Conversations = append(res.Conversations, cr)
	}

	// **遅すぎる -before の証拠があれば書かない。** Elythia で受けた未読を、
	// 本家の記録が無い (= 既読) として既読にしてしまうため。reads から受け手を
	// 外す手段は無いので、書いた後では戻せない。
	if len(res.TooLate) > 0 {
		if opts.DryRun {
			return res, nil
		}
		return res, ErrChatReadsCutoffTooLate
	}
	if !opts.DryRun && len(markIDs) > 0 {
		if err := db.WithContext(ctx).Exec(chatReadsMarkSQL, markIDs).Error; err != nil {
			return ChatReadsBackfillResult{}, err
		}
	}
	return res, nil
}

// DefaultChatReadsTooLateMargin is the default of
// ChatReadsBackfillOptions.TooLateMargin.
//
// 本家は id を作ってから insert を await し、その後で marker を SET する。同じ会話に
// ほぼ同時に 2 通来ると (別の端末、bot、CherryPick の inbox の並行処理)、marker が
// 古い方の id になりうる。止めた瞬間に insert の後・SET の前だった DM も同じ形になる。
// また objectid は秒、meid / ulid は ms の後ろが乱数、cluster で動かした aidx は
// 個体 ID が先に来るので、同じ時刻の中の id の並びは作った順にならない。どちらも
// 数秒の中で起きることなので、それを十分に越える 5 分を幅にする。本当に -before が
// 遅いときは、移行の後に受けた DM は移行の時点の marker より普通は何分も新しい。
const DefaultChatReadsTooLateMargin = 5 * time.Minute

// chatMessageNewer reports whether msgID was created more than margin after
// markerID, the latest message the source server delivered. Only the times
// read from the ids are compared, never the order of the ids. An id whose
// time cannot be read is not counted.
func chatMessageNewer(parse func(string) (time.Time, error), msgID, markerID string, margin time.Duration) bool {
	tm, err := parse(msgID)
	if err != nil {
		return false
	}
	tk, err := parse(markerID)
	if err != nil {
		return false
	}
	return tm.After(tk.Add(margin))
}
