package maintenance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

var chatReadsGen, _ = id.NewGenerator("aidx")

// chatReadsCutoff is the migration time of the tests.
var chatReadsCutoff = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

const chatReadsPrefix = "example.tld:"

func seedChatReadsUser(t *testing.T, userID string, host *string) {
	t.Helper()
	u := &model.User{
		ID: userID, Username: userID, UsernameLower: userID, Host: host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	if host == nil {
		token := "tok_" + userID
		u.Token = &token
	}
	require.NoError(t, testDB.Create(u).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, userID) })
}

// seedDM stores a direct message created at `at` whose reads are `reads`.
func seedDM(t *testing.T, at time.Time, from, to string, reads ...string) string {
	t.Helper()
	msgID := chatReadsGen.Generate(at)
	seedDMWithID(t, msgID, from, to, reads...)
	return msgID
}

func seedDMWithID(t *testing.T, msgID, from, to string, reads ...string) {
	t.Helper()
	text := "secret body " + msgID
	if reads == nil {
		reads = []string{}
	}
	require.NoError(t, testDB.Create(&model.ChatMessage{
		ID: msgID, FromUserID: from, ToUserID: &to, Text: &text, Reads: model.StringArray(reads),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msgID) })
}

func messageReads(t *testing.T, msgID string) []string {
	t.Helper()
	var m model.ChatMessage
	require.NoError(t, testDB.Where("id = ?", msgID).First(&m).Error)
	return []string(m.Reads)
}

func newChatReadsRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

// markUnread writes the records that upstream ChatService.createMessageToUser
// leaves for an unread conversation.
func markUnread(t *testing.T, mr *miniredis.Miniredis, reader, sender, lastID string) {
	t.Helper()
	require.NoError(t, mr.Set(chatReadsPrefix+"newUserChatMessageExists:"+reader+":"+sender, lastID))
	_, err := mr.SetAdd(chatReadsPrefix+"newChatMessagesExists:"+reader, "user:"+sender)
	require.NoError(t, err)
}

func runChatReads(t *testing.T, src ChatReadStateSource, dryRun bool) (ChatReadsBackfillResult, []ChatReadsBackfillResult) {
	t.Helper()
	opts := ChatReadsBackfillOptions{Before: chatReadsCutoff, ParseTime: chatReadsGen.ParseTime, Source: src, DryRun: dryRun}
	var total ChatReadsBackfillResult
	var batches []ChatReadsBackfillResult
	cursor := ""
	for {
		// 2 件ずつにして、会話がバッチをまたいでも同じ判定になることを見る。
		res, err := BackfillChatReadsBatch(context.Background(), testDB, cursor, 2, opts)
		require.NoError(t, err)
		if res.Scanned == 0 {
			break
		}
		batches = append(batches, res)
		cursor = res.LastID
		total.Scanned += res.Scanned
		total.AfterCutoff += res.AfterCutoff
		total.Unparsable += res.Unparsable
		total.Conversations = append(total.Conversations, res.Conversations...)
		total.TooLate = append(total.TooLate, res.TooLate...)
		if res.Newest.After(total.Newest) {
			total.Newest = res.Newest
		}
	}
	return total, batches
}

func sumByConversation(results []ChatReadsConversationResult) map[ChatConversation]ChatReadsConversationResult {
	out := map[ChatConversation]ChatReadsConversationResult{}
	for _, r := range results {
		cur := out[r.Conversation]
		cur.Conversation = r.Conversation
		cur.State = r.State
		cur.Marked += r.Marked
		cur.Kept += r.Kept
		out[r.Conversation] = cur
	}
	return out
}

func clearChatTables(t *testing.T) {
	t.Helper()
	require.NoError(t, testDB.Exec(`DELETE FROM "chat_message"`).Error)
	require.NoError(t, testDB.Exec(`DELETE FROM "chat_room"`).Error)
	require.NoError(t, testDB.Exec(`DELETE FROM "user"`).Error)
}

func TestBackfillChatReadsBatch_FollowsRedisRecords(t *testing.T) {
	clearChatTables(t)
	remote := "remote.example"
	for _, u := range []string{"cr_reader", "cr_a", "cr_b", "cr_c", "cr_d", "cr_e"} {
		seedChatReadsUser(t, u, nil)
	}
	seedChatReadsUser(t, "cr_remote", &remote)

	before := chatReadsCutoff.Add(-24 * time.Hour)
	after := chatReadsCutoff.Add(time.Hour)

	// a: 記録が無い会話 (読まれた) → 既読にする。
	a1 := seedDM(t, before, "cr_a", "cr_reader")
	a2 := seedDM(t, before.Add(time.Minute), "cr_a", "cr_reader")
	// a の移行の後のメッセージは Elythia が reads を持つので触らない。
	aAfter := seedDM(t, after, "cr_a", "cr_reader")
	// b: marker と set の両方が未読 → 触らない。
	b1 := seedDM(t, before.Add(2*time.Minute), "cr_b", "cr_reader")
	// c: marker だけが残っている (本家の「全チャットを既読にする」の後) → 既読にする。
	c1 := seedDM(t, before.Add(3*time.Minute), "cr_c", "cr_reader")
	// d: marker のキーが別の型 (読めない記録) → 触らない。
	d1 := seedDM(t, before.Add(4*time.Minute), "cr_d", "cr_reader")
	// 宛先がリモートの利用者のメッセージは対象外。
	toRemote := seedDM(t, before.Add(5*time.Minute), "cr_reader", "cr_remote")
	// 既に受け手が reads にある行は数えない。
	alreadyRead := seedDM(t, before.Add(6*time.Minute), "cr_e", "cr_reader", "cr_reader")
	// e から reader への会話の残りは記録が無いので既読になる。
	e1 := seedDM(t, before.Add(7*time.Minute), "cr_e", "cr_reader")

	// ルームのメッセージは /api/i が数えないので対象外。
	room := &model.ChatRoom{ID: "cr_room", Name: "room", OwnerID: "cr_a"}
	require.NoError(t, testDB.Create(room).Error)
	roomMsgID := chatReadsGen.Generate(before.Add(8 * time.Minute))
	roomText := "room body"
	require.NoError(t, testDB.Create(&model.ChatMessage{
		ID: roomMsgID, FromUserID: "cr_a", ToRoomID: &room.ID, Text: &roomText, Reads: model.StringArray{},
	}).Error)

	mr, client := newChatReadsRedis(t)
	markUnread(t, mr, "cr_reader", "cr_b", b1)
	require.NoError(t, mr.Set(chatReadsPrefix+"newUserChatMessageExists:cr_reader:cr_c", c1))
	_, err := mr.SetAdd(chatReadsPrefix+"newUserChatMessageExists:cr_reader:cr_d", "x")
	require.NoError(t, err)
	// prefix の付かないキーは別のサーバーの記録なので見ない。
	require.NoError(t, mr.Set("newUserChatMessageExists:cr_reader:cr_a", a2))

	src := &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix}
	total, _ := runChatReads(t, src, false)

	assert.Equal(t, []string{"cr_reader"}, messageReads(t, a1))
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, a2))
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, c1))
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, e1))
	assert.Empty(t, messageReads(t, aAfter), "a message after the cutoff must stay unread")
	assert.Empty(t, messageReads(t, b1), "a conversation still unread in Redis must stay unread")
	assert.Empty(t, messageReads(t, d1), "a conversation whose record is broken must stay unread")
	assert.Empty(t, messageReads(t, toRemote), "a message to a remote user must not be touched")
	assert.Empty(t, messageReads(t, roomMsgID), "a room message must not be touched")
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, alreadyRead))

	assert.Equal(t, 7, total.Scanned, "a1, a2, aAfter, b1, c1, d1 and e1: DMs to a local user without the reader in reads")
	assert.Equal(t, 1, total.AfterCutoff)
	assert.Zero(t, total.Unparsable)
	byConv := sumByConversation(total.Conversations)
	assert.Equal(t, ChatReadsConversationResult{Conversation: ChatConversation{"cr_reader", "cr_a"}, State: ChatReadStateRead, Marked: 2}, byConv[ChatConversation{"cr_reader", "cr_a"}])
	assert.Equal(t, ChatReadsConversationResult{Conversation: ChatConversation{"cr_reader", "cr_b"}, State: ChatReadStateUnread, Kept: 1}, byConv[ChatConversation{"cr_reader", "cr_b"}])
	assert.Equal(t, ChatReadStateRead, byConv[ChatConversation{"cr_reader", "cr_c"}].State)
	assert.Equal(t, ChatReadsConversationResult{Conversation: ChatConversation{"cr_reader", "cr_d"}, State: ChatReadStateBroken, Kept: 1}, byConv[ChatConversation{"cr_reader", "cr_d"}])
	assert.Len(t, byConv, 5)

	// 2 回目: 受け手を二重に足さず、未読の会話も変わらない。
	again, _ := runChatReads(t, &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix}, false)
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, a1))
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, e1))
	assert.Empty(t, messageReads(t, b1))
	assert.Equal(t, 3, again.Scanned, "only aAfter, b1 and d1 are left")
	for _, c := range again.Conversations {
		assert.Zero(t, c.Marked, "%v", c.Conversation)
	}
}

// 本家の「全チャットを既読にする」は set ごと消すので、set のキーが無いときも
// marker だけで未読にしない。set に入っているが marker が無い会話も既読にする。
func TestChatReadState(t *testing.T) {
	ctx := context.Background()
	_, client := newChatReadsRedis(t)
	require.NoError(t, client.SAdd(ctx, "s", "user:x").Err())
	require.NoError(t, client.Set(ctx, "str", "v", 0).Err())

	cases := []struct {
		name      string
		markerErr error
		member    *redis.BoolCmd
		want      ChatReadState
	}{
		{name: "marker and member", markerErr: nil, member: client.SIsMember(ctx, "s", "user:x"), want: ChatReadStateUnread},
		{name: "marker only", markerErr: nil, member: client.SIsMember(ctx, "s", "user:y"), want: ChatReadStateRead},
		{name: "member only", markerErr: redis.Nil, member: client.SIsMember(ctx, "s", "user:x"), want: ChatReadStateRead},
		{name: "neither", markerErr: redis.Nil, member: client.SIsMember(ctx, "none", "user:x"), want: ChatReadStateRead},
		{name: "set of the wrong type", markerErr: nil, member: client.SIsMember(ctx, "str", "user:x"), want: ChatReadStateBroken},
		{name: "marker of the wrong type", markerErr: client.Get(ctx, "s").Err(), member: client.SIsMember(ctx, "s", "user:x"), want: ChatReadStateBroken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chatReadState(tc.markerErr, tc.member)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBackfillChatReadsBatch_DryRunWritesNothing(t *testing.T) {
	clearChatTables(t)
	seedChatReadsUser(t, "cr_reader", nil)
	seedChatReadsUser(t, "cr_a", nil)
	m := seedDM(t, chatReadsCutoff.Add(-time.Hour), "cr_a", "cr_reader")

	total, _ := runChatReads(t, AssumeReadSource{}, true)
	assert.Empty(t, messageReads(t, m))
	assert.Equal(t, 1, sumByConversation(total.Conversations)[ChatConversation{"cr_reader", "cr_a"}].Marked)
}

func TestBackfillChatReadsBatch_AssumeReadMarksEverythingBeforeTheCutoff(t *testing.T) {
	clearChatTables(t)
	seedChatReadsUser(t, "cr_reader", nil)
	seedChatReadsUser(t, "cr_a", nil)
	beforeID := seedDM(t, chatReadsCutoff.Add(-time.Hour), "cr_a", "cr_reader")
	// cutoff ちょうどに作られたものは移行の後とみなす。
	atCutoff := seedDM(t, chatReadsCutoff, "cr_a", "cr_reader")
	// id の時刻が読めない行は触らずに数える。
	seedDMWithID(t, "~~~~~~~~bad", "cr_a", "cr_reader")

	total, _ := runChatReads(t, AssumeReadSource{}, false)
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, beforeID))
	assert.Empty(t, messageReads(t, atCutoff))
	assert.Empty(t, messageReads(t, "~~~~~~~~bad"))
	assert.Equal(t, 1, total.AfterCutoff)
	assert.Equal(t, 1, total.Unparsable)
}

func TestRedisChatReadStateSource_NoRecords(t *testing.T) {
	clearChatTables(t)
	seedChatReadsUser(t, "cr_reader", nil)
	seedChatReadsUser(t, "cr_a", nil)
	m := seedDM(t, chatReadsCutoff.Add(-time.Hour), "cr_a", "cr_reader")

	mr, client := newChatReadsRedis(t)
	// prefix の違うサーバーの記録は数えない。
	require.NoError(t, mr.Set("other.tld:newUserChatMessageExists:cr_reader:cr_a", "x"))

	opts := ChatReadsBackfillOptions{
		Before: chatReadsCutoff, ParseTime: chatReadsGen.ParseTime,
		Source: &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix},
	}
	_, err := BackfillChatReadsBatch(context.Background(), testDB, "", 10, opts)
	require.ErrorIs(t, err, ErrNoChatReadRecords)
	assert.Empty(t, messageReads(t, m), "no record must not silently mark everything read")
}

func TestRedisChatReadStateSource_SetOnlyCountsAsRecord(t *testing.T) {
	_, client := newChatReadsRedis(t)
	ctx := context.Background()
	// prefix に glob の記号があっても、その文字どおりに探す。
	prefix := "a[b]*:"
	require.NoError(t, client.SAdd(ctx, prefix+"newChatMessagesExists:r", "room:x").Err())
	require.NoError(t, client.Set(ctx, "aXb*:newUserChatMessageExists:r:s", "x", 0).Err())

	src := &RedisChatReadStateSource{Client: client, Prefix: prefix}
	states, err := src.States(ctx, []ChatConversation{{"r", "s"}})
	require.NoError(t, err)
	assert.Equal(t, ChatReadRecord{State: ChatReadStateRead}, states[ChatConversation{"r", "s"}])

	src = &RedisChatReadStateSource{Client: client, Prefix: "a?b*:"}
	_, err = src.States(ctx, []ChatConversation{{"r", "s"}})
	require.ErrorIs(t, err, ErrNoChatReadRecords, "a glob in the prefix must not match other prefixes")
}

func TestRedisChatReadStateSource_CachesPerConversation(t *testing.T) {
	mr, client := newChatReadsRedis(t)
	markUnread(t, mr, "r", "s", "x")
	src := &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix}
	ctx := context.Background()
	states, err := src.States(ctx, []ChatConversation{{"r", "s"}, {"r", "s"}, {"r", "t"}})
	require.NoError(t, err)
	assert.Equal(t, ChatReadRecord{State: ChatReadStateUnread, MarkerID: "x"}, states[ChatConversation{"r", "s"}])
	assert.Equal(t, ChatReadRecord{State: ChatReadStateRead}, states[ChatConversation{"r", "t"}])

	// 一度決めた会話は、後のバッチで Redis を読み直さない (同じ会話の判定が
	// バッチごとに変わらない)。
	testutil.MiniredisDown(mr)
	states, err = src.States(ctx, []ChatConversation{{"r", "s"}})
	require.NoError(t, err)
	assert.Equal(t, ChatReadRecord{State: ChatReadStateUnread, MarkerID: "x"}, states[ChatConversation{"r", "s"}])
}

func TestBackfillChatReadsBatch_RedisDownWritesNothing(t *testing.T) {
	clearChatTables(t)
	seedChatReadsUser(t, "cr_reader", nil)
	seedChatReadsUser(t, "cr_a", nil)
	seedChatReadsUser(t, "cr_b", nil)
	m := seedDM(t, chatReadsCutoff.Add(-time.Hour), "cr_a", "cr_reader")

	for _, tc := range []struct {
		name  string
		setup func(mr *miniredis.Miniredis, src *RedisChatReadStateSource)
	}{
		{name: "down before the scan", setup: func(mr *miniredis.Miniredis, _ *RedisChatReadStateSource) {
			testutil.MiniredisDown(mr)
		}},
		{name: "down after the scan", setup: func(mr *miniredis.Miniredis, src *RedisChatReadStateSource) {
			src.checked = true
			testutil.MiniredisDown(mr)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, client := newChatReadsRedis(t)
			markUnread(t, mr, "cr_reader", "cr_b", "x")
			src := &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix}
			tc.setup(mr, src)
			opts := ChatReadsBackfillOptions{Before: chatReadsCutoff, ParseTime: chatReadsGen.ParseTime, Source: src}
			_, err := BackfillChatReadsBatch(context.Background(), testDB, "", 10, opts)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrNoChatReadRecords)
			assert.Empty(t, messageReads(t, m))
		})
	}
}

func TestBackfillChatReadsBatch_DBError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BackfillChatReadsBatch(ctx, testDB, "", 10, ChatReadsBackfillOptions{Source: AssumeReadSource{}})
	require.Error(t, err)
}

func TestChatReadStateString(t *testing.T) {
	assert.Equal(t, "read", ChatReadStateRead.String())
	assert.Equal(t, "unread", ChatReadStateUnread.String())
	assert.Equal(t, "broken", ChatReadStateBroken.String())
}

// markingSource marks the messages read while the batch is deciding, as
// Elythia does when the recipient opens the conversation during the run.
type markingSource struct{}

func (markingSource) States(_ context.Context, convs []ChatConversation) (map[ChatConversation]ChatReadRecord, error) {
	out := map[ChatConversation]ChatReadRecord{}
	for _, c := range convs {
		if err := testDB.Exec(`UPDATE "chat_message" SET "reads" = array_append("reads", ?) WHERE "toUserId" = ?`, c.ReaderID, c.ReaderID).Error; err != nil {
			return nil, err
		}
		out[c] = ChatReadRecord{State: ChatReadStateRead}
	}
	return out, nil
}

func TestBackfillChatReadsBatch_DoesNotAddTheReaderTwice(t *testing.T) {
	clearChatTables(t)
	seedChatReadsUser(t, "cr_reader", nil)
	seedChatReadsUser(t, "cr_a", nil)
	m := seedDM(t, chatReadsCutoff.Add(-time.Hour), "cr_a", "cr_reader")

	runChatReads(t, markingSource{}, false)
	assert.Equal(t, []string{"cr_reader"}, messageReads(t, m))
}

// 本家は新着のたびに marker を最新の id で上書きするので、marker より新しく
// -before より前の DM は、本家が受けていない (Elythia で受けた) もの。それがあれば
// -before が遅すぎるので、書かずに止める (#3653)。
func TestBackfillChatReadsBatch_DetectsALateCutoff(t *testing.T) {
	clearChatTables(t)
	for _, u := range []string{"cr_reader", "cr_a", "cr_b", "cr_c"} {
		seedChatReadsUser(t, u, nil)
	}
	// 本家の時代のメッセージ。b は未読、c は「全チャットを既読にする」の後。
	b1 := seedDM(t, chatReadsCutoff.Add(-48*time.Hour), "cr_b", "cr_reader")
	c1 := seedDM(t, chatReadsCutoff.Add(-47*time.Hour), "cr_c", "cr_reader")
	// Elythia で受けた未読 (渡された -before が移行より遅い)。
	a1 := seedDM(t, chatReadsCutoff.Add(-3*time.Hour), "cr_a", "cr_reader")
	b2 := seedDM(t, chatReadsCutoff.Add(-2*time.Hour), "cr_b", "cr_reader")
	// 証拠が 2 通ある会話では、newest-at は新しい方 (運営者が移行の時刻と見比べる)。
	b3 := seedDM(t, chatReadsCutoff.Add(-90*time.Minute), "cr_b", "cr_reader")
	newest := chatReadsCutoff.Add(-time.Hour)
	c2 := seedDM(t, newest, "cr_c", "cr_reader")

	mr, client := newChatReadsRedis(t)
	markUnread(t, mr, "cr_reader", "cr_b", b1)
	require.NoError(t, mr.Set(chatReadsPrefix+"newUserChatMessageExists:cr_reader:cr_c", c1))

	// -before より後の DM は、最も新しい DM の日時にも入れない。
	seedDM(t, chatReadsCutoff.Add(time.Hour), "cr_a", "cr_reader")

	opts := ChatReadsBackfillOptions{
		Before: chatReadsCutoff, ParseTime: chatReadsGen.ParseTime,
		Source:        &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix},
		TooLateMargin: DefaultChatReadsTooLateMargin,
	}
	res, err := BackfillChatReadsBatch(context.Background(), testDB, "", 100, opts)
	require.ErrorIs(t, err, ErrChatReadsCutoffTooLate)
	ts := func(msgID string) time.Time {
		at, err := chatReadsGen.ParseTime(msgID)
		require.NoError(t, err)
		return at
	}
	assert.Equal(t, []ChatReadsTooLate{
		{Conversation: ChatConversation{"cr_reader", "cr_b"}, MarkerID: b1, Newer: 2, MarkerAt: ts(b1), NewestAt: ts(b3)},
		{Conversation: ChatConversation{"cr_reader", "cr_c"}, MarkerID: c1, Newer: 1, MarkerAt: ts(c1), NewestAt: ts(c2)},
	}, res.TooLate)
	for _, m := range []string{a1, b1, b2, b3, c1, c2} {
		assert.Empty(t, messageReads(t, m), "nothing must be written when -before is too late")
	}

	assert.True(t, res.Newest.Equal(newest.Truncate(time.Millisecond)), "%v", res.Newest)

	// dry-run では止めずに証拠と、対象のうち最も新しい DM の日時を返す。
	opts.DryRun = true
	total, _ := runChatReads(t, opts.Source, true)
	// バッチ (ここでは 2 件ずつ) をまたぐ会話は、バッチごとに 1 行になる。
	lateConvs := map[ChatConversation]bool{}
	for _, l := range total.TooLate {
		lateConvs[l.Conversation] = true
	}
	assert.Len(t, lateConvs, 2)
	assert.True(t, total.Newest.Equal(newest.Truncate(time.Millisecond)), "%v", total.Newest)
	for _, m := range []string{a1, b2, c2} {
		assert.Empty(t, messageReads(t, m))
	}
}

// 時刻だけで比べ、marker の時刻から幅を越えて新しい DM だけを証拠にする。同じ時刻の
// 中の id の並びは作った順にならず (objectid の秒、meid / ulid の乱数、cluster の
// aidx)、本家の書き込みの順の前後で marker が古い方の id になることもある。
func TestChatMessageNewer(t *testing.T) {
	margin := DefaultChatReadsTooLateMargin
	aidx := chatReadsGen.ParseTime
	at := chatReadsCutoff
	marker := chatReadsGen.Generate(at)

	// 幅より後なら証拠。
	assert.True(t, chatMessageNewer(aidx, chatReadsGen.Generate(at.Add(margin+time.Millisecond)), marker, margin))
	assert.True(t, chatMessageNewer(aidx, chatReadsGen.Generate(at.Add(time.Hour)), marker, margin))
	// 幅ちょうど、幅の中、同じ ms、marker より前は証拠にしない。
	assert.False(t, chatMessageNewer(aidx, chatReadsGen.Generate(at.Add(margin)), marker, margin))
	assert.False(t, chatMessageNewer(aidx, chatReadsGen.Generate(at.Add(20*time.Millisecond)), marker, margin), "marker may point at the older of two messages")
	assert.False(t, chatMessageNewer(aidx, marker[:8]+"zzzzzzzz", marker[:8]+"00000000", margin))
	assert.False(t, chatMessageNewer(aidx, chatReadsGen.Generate(at.Add(-time.Second)), marker, margin))
	// 幅が 0 でも、同じ時刻の中は id の並びで比べない。
	assert.False(t, chatMessageNewer(aidx, marker[:8]+"zzzzzzzz", marker[:8]+"00000000", 0))

	// objectid は秒なので、同じ秒の 2 通は乱数部に関係なく同じ時刻。
	oid, err := id.NewGenerator("objectid")
	require.NoError(t, err)
	sec := fmt.Sprintf("%08x", at.Unix())
	assert.False(t, chatMessageNewer(oid.ParseTime, sec+"ffffffffffffffff", sec+"0000000000000000", margin))
	assert.False(t, chatMessageNewer(oid.ParseTime, sec+"ffffffffffffffff", sec+"0000000000000000", 0))
	later := fmt.Sprintf("%08x", at.Add(margin+time.Second).Unix())
	assert.True(t, chatMessageNewer(oid.ParseTime, later+"0000000000000000", sec+"ffffffffffffffff", margin))

	// 読めない id は数えない。
	assert.False(t, chatMessageNewer(aidx, "~~~~~~~~bad", marker, margin))
	assert.False(t, chatMessageNewer(aidx, marker, "~~~~~~~~bad", margin))
}

// 本家の書き込みの順の前後 (marker が古い方の id を指す) や、同じ時刻の 2 通では
// 止めない。
func TestBackfillChatReadsBatch_NoFalseTooLate(t *testing.T) {
	clearChatTables(t)
	for _, u := range []string{"cr_reader", "cr_b", "cr_c"} {
		seedChatReadsUser(t, u, nil)
	}
	at := chatReadsCutoff.Add(-24 * time.Hour)
	// b: 20ms 後の 2 通目が先に SET され、marker が 1 通目を指している。
	b1 := seedDM(t, at, "cr_b", "cr_reader")
	b2 := seedDM(t, at.Add(20*time.Millisecond), "cr_b", "cr_reader")
	// c: 同じ ms の 2 通。marker は id の並びで小さい方。
	c1 := at.Add(time.Hour)
	cHigh := chatReadsGen.Generate(c1)[:8] + "zzzzzzzz"
	cLow := chatReadsGen.Generate(c1)[:8] + "00000000"
	seedDMWithID(t, cHigh, "cr_c", "cr_reader")
	seedDMWithID(t, cLow, "cr_c", "cr_reader")

	mr, client := newChatReadsRedis(t)
	markUnread(t, mr, "cr_reader", "cr_b", b1)
	markUnread(t, mr, "cr_reader", "cr_c", cLow)

	opts := ChatReadsBackfillOptions{
		Before: chatReadsCutoff, ParseTime: chatReadsGen.ParseTime,
		Source:        &RedisChatReadStateSource{Client: client, Prefix: chatReadsPrefix},
		TooLateMargin: DefaultChatReadsTooLateMargin,
	}
	res, err := BackfillChatReadsBatch(context.Background(), testDB, "", 100, opts)
	require.NoError(t, err)
	assert.Empty(t, res.TooLate)
	for _, m := range []string{b1, b2, cHigh, cLow} {
		assert.Empty(t, messageReads(t, m), "unread conversations stay unread")
	}
}

// pagedScan returns SCAN results in fixed pages, as a large Redis does: a page
// can be empty while the cursor goes on. The cursor is the index of the page.
type pagedScan struct {
	redis.Cmdable
	pages [][]string
	calls int
}

func (p *pagedScan) Scan(_ context.Context, cursor uint64, _ string, _ int64) *redis.ScanCmd {
	p.calls++
	next := cursor + 1
	if next >= uint64(len(p.pages)) {
		next = 0
	}
	return redis.NewScanCmdResult(p.pages[cursor], next, nil)
}

func TestRedisChatReadStateSource_ScanFollowsTheCursor(t *testing.T) {
	ctx := context.Background()
	// 最初のページが空でも、カーソルが 0 に戻るまで読む。
	found := &pagedScan{pages: [][]string{{}, {}, {chatReadsPrefix + "newChatMessagesExists:r"}}}
	ok, err := (&RedisChatReadStateSource{Client: found, Prefix: chatReadsPrefix}).hasAnyRecord(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 3, found.calls)

	// どちらの形も、全部のページが空なら記録は無い。
	empty := &pagedScan{pages: [][]string{{}, {}}}
	ok, err = (&RedisChatReadStateSource{Client: empty, Prefix: chatReadsPrefix}).hasAnyRecord(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 4, empty.calls)
}
