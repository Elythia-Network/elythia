package repository

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyUserHistorySQL / legacyRoomHistorySQL / legacyHistorySQL are the
// DISTINCT ON queries that chat/history used before #3990. The tests below
// pin the GROUP BY + max(id) rewrite to return exactly the same ids.
const legacyUserHistorySQL = `
	SELECT id FROM (
		SELECT DISTINCT ON (conversation_key) *
		FROM (
			SELECT *, LEAST("fromUserId","toUserId") || '-' || GREATEST("fromUserId","toUserId") AS conversation_key
			FROM "chat_message"
			WHERE ("fromUserId" = ? OR "toUserId" = ?) AND "toRoomId" IS NULL
		) sub
		ORDER BY conversation_key, id DESC
	) conversations
	WHERE (? = '' OR id < ?)
	ORDER BY id DESC
	LIMIT ?`

const legacyRoomHistorySQL = `
	SELECT id FROM (
		SELECT DISTINCT ON ("toRoomId") cm.*
		FROM "chat_message" cm
		WHERE cm."toRoomId" IS NOT NULL
		AND (
			EXISTS (SELECT 1 FROM "chat_room_membership" m WHERE m."roomId" = cm."toRoomId" AND m."userId" = ?)
			OR EXISTS (SELECT 1 FROM "chat_room" cr WHERE cr."id" = cm."toRoomId" AND cr."ownerId" = ?)
		)
		ORDER BY "toRoomId", cm.id DESC
	) conversations
	WHERE (? = '' OR id < ?)
	ORDER BY id DESC
	LIMIT ?`

const legacyHistorySQL = `
	SELECT id FROM (
		SELECT DISTINCT ON (conversation_key) *
		FROM (
			SELECT *, LEAST("fromUserId","toUserId") || '-' || GREATEST("fromUserId","toUserId") AS conversation_key
			FROM "chat_message"
			WHERE ("fromUserId" = ? OR "toUserId" = ?) AND "toRoomId" IS NULL
			UNION ALL
			SELECT cm.*, 'room:' || cm."toRoomId" AS conversation_key
			FROM "chat_message" cm
			WHERE cm."toRoomId" IS NOT NULL
			AND (
				EXISTS (SELECT 1 FROM "chat_room_membership" m WHERE m."roomId" = cm."toRoomId" AND m."userId" = ?)
				OR EXISTS (SELECT 1 FROM "chat_room" cr WHERE cr."id" = cm."toRoomId" AND cr."ownerId" = ?)
			)
		) sub
		ORDER BY conversation_key, id DESC
	) conversations
	ORDER BY id DESC
	LIMIT ?`

func legacyIDs(t *testing.T, q string, args ...any) []string {
	t.Helper()
	var ids []string
	require.NoError(t, testDB.Raw(q, args...).Pluck("id", &ids).Error)
	if ids == nil {
		ids = []string{}
	}
	return ids
}

func msgIDs(msgs []*model.ChatMessage) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestChatRepository_HistoryMatchesLegacy pins #3990: the GROUP BY + max(id)
// history queries must pick the same latest message per conversation, in the
// same order, as the DISTINCT ON queries they replaced. The fixture covers
// self-DMs (from = to), sender-only rows (toUserId NULL, toRoomId NULL),
// blocked / muted peers (neither query filters them), owned / joined / left
// rooms, every untilId and a deleted peer.
func TestChatRepository_HistoryMatchesLegacy(t *testing.T) {
	repo := NewChatRepository(testDB)
	rng := rand.New(rand.NewPCG(3990, 1))

	var users []string
	for i := range 6 {
		u := insertTestUser(t, fmt.Sprintf("u_cheq%d", i), fmt.Sprintf("cheq%d", i))
		users = append(users, u.ID)
	}
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "chat_message" WHERE id LIKE 'cmeq%'`)
		testDB.Exec(`DELETE FROM "chat_room" WHERE id LIKE 'creq%'`)
		for _, id := range users {
			cleanupUser(t, id)
		}
	})

	// rooms: A は u0 が所有し u1 が参加、B は u2 が所有し u0 / u3 が参加、
	// C は u4 が所有し u0 が参加したが退出した。
	rooms := []string{"creqA", "creqB", "creqC"}
	for i, owner := range []string{users[0], users[2], users[4]} {
		require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: rooms[i], Name: rooms[i], OwnerID: owner}))
	}
	for i, mb := range []struct{ user, room string }{
		{users[1], rooms[0]}, {users[0], rooms[1]}, {users[3], rooms[1]}, {users[0], rooms[2]},
	} {
		require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: fmt.Sprintf("cmbeq%d", i), UserID: mb.user, RoomID: mb.room}))
	}
	require.NoError(t, repo.DeleteMembership(users[0], rooms[2]))
	require.NoError(t, testDB.Exec(`INSERT INTO "blocking" (id, "blockerId", "blockeeId") VALUES ('blkeq1', ?, ?)`, users[0], users[1]).Error)
	require.NoError(t, testDB.Exec(`INSERT INTO "muting" (id, "muterId", "muteeId") VALUES ('muteq1', ?, ?)`, users[0], users[2]).Error)

	// id は挿入順と無関係に振る (会話の最新が最後に入れた行とは限らない)。
	const n = 180
	order := rng.Perm(n)
	text := "eq"
	var allIDs []string
	for i := range n {
		id := fmt.Sprintf("cmeq%04d", order[i]*7)
		allIDs = append(allIDs, id)
		m := &model.ChatMessage{ID: id, Text: &text, Reactions: model.StringArray{}, Emojis: model.StringArray{}}
		m.FromUserID = users[rng.IntN(len(users))]
		switch k := rng.IntN(10); {
		case k < 5:
			to := users[rng.IntN(len(users))]
			m.ToUserID = &to
		case k == 5:
			to := m.FromUserID // 自分宛て
			m.ToUserID = &to
		case k == 6:
			// 宛先の無い行 (toUserId も toRoomId も NULL)
		default:
			room := rooms[rng.IntN(len(rooms))]
			m.ToRoomID = &room
		}
		reads := model.StringArray{}
		for _, u := range users {
			if rng.IntN(3) == 0 {
				reads = append(reads, u)
			}
		}
		m.Reads = reads
		require.NoError(t, repo.CreateMessage(m))
	}
	// untilId: 空、範囲の外、メッセージの id (4 件に 1 件) と、その直後の id。
	// 全件にすると比較が 20 秒を超えるので間引く。
	untils := []string{"", "0", "zzzz", "cmeq", "cmeq0000"}
	for i, id := range allIDs {
		if i%4 == 0 {
			untils = append(untils, id, id+"0")
		}
	}

	compare := func(t *testing.T, present []string) {
		t.Helper()
		var nonEmpty int
		for _, me := range present {
			for _, limit := range []int{1, 2, 3, 100} {
				got, err := repo.ListHistory(me, limit)
				require.NoError(t, err)
				want := legacyIDs(t, legacyHistorySQL, me, me, me, me, limit)
				assert.Equal(t, want, msgIDs(got), "ListHistory me=%s limit=%d", me, limit)
			}
			for _, until := range untils {
				limits := []int{2}
				if until == "" {
					limits = []int{1, 2, 3, 100}
				}
				for _, limit := range limits {
					got, err := repo.ListUserHistory(me, limit, until)
					require.NoError(t, err)
					want := legacyIDs(t, legacyUserHistorySQL, me, me, until, until, limit)
					assert.Equal(t, want, msgIDs(got), "ListUserHistory me=%s limit=%d until=%q", me, limit, until)

					got, err = repo.ListRoomHistory(me, limit, until)
					require.NoError(t, err)
					wantRoom := legacyIDs(t, legacyRoomHistorySQL, me, me, until, until, limit)
					assert.Equal(t, wantRoom, msgIDs(got), "ListRoomHistory me=%s limit=%d until=%q", me, limit, until)
					if len(want) > 0 && len(wantRoom) > 0 {
						nonEmpty++
					}
				}
			}
		}
		// 両方が空のまま一致しても何も確かめていないので、中身のある比較を要求する。
		require.Greater(t, nonEmpty, 20)
	}
	compare(t, users)

	// 相手を削除した後 (CASCADE で DM が消える) も一致する。
	cleanupUser(t, users[5])
	compare(t, users[:5])
}

// TestChatRepository_UnreadBatchMatchesSingle pins #3990: UnreadFromUsers /
// UnreadInRooms must agree with HasUnreadFromUser / HasUnreadInRoom for every
// (reader, peer) and (reader, room), including the reader itself as a peer.
func TestChatRepository_UnreadBatchMatchesSingle(t *testing.T) {
	repo := NewChatRepository(testDB)
	rng := rand.New(rand.NewPCG(3990, 2))

	var users []string
	for i := range 5 {
		u := insertTestUser(t, fmt.Sprintf("u_chur%d", i), fmt.Sprintf("chur%d", i))
		users = append(users, u.ID)
	}
	rooms := []string{"crurA", "crurB", "crurC"}
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "chat_message" WHERE id LIKE 'cmur%'`)
		testDB.Exec(`DELETE FROM "chat_room" WHERE id LIKE 'crur%'`)
		for _, id := range users {
			cleanupUser(t, id)
		}
	})
	for i, r := range rooms {
		require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: r, Name: r, OwnerID: users[i]}))
	}
	text := "ur"
	for i := range 80 {
		m := &model.ChatMessage{ID: fmt.Sprintf("cmur%03d", i), Text: &text, Reactions: model.StringArray{}, Emojis: model.StringArray{}}
		m.FromUserID = users[rng.IntN(len(users))]
		if rng.IntN(3) == 0 {
			room := rooms[rng.IntN(len(rooms))]
			m.ToRoomID = &room
		} else {
			to := users[rng.IntN(len(users))]
			m.ToUserID = &to
		}
		reads := model.StringArray{}
		for _, u := range users {
			if rng.IntN(2) == 0 {
				reads = append(reads, u)
			}
		}
		m.Reads = reads
		require.NoError(t, repo.CreateMessage(m))
	}

	// 自分が送っただけの未読 (相手も自分もまだ読んでいない) は、自分にとって
	// 未読ではない。乱数の行では起きにくいので、専用の部屋と相手で作る。
	ownOnlyPeer := insertTestUser(t, "u_chur_own", "churown")
	t.Cleanup(func() { cleanupUser(t, ownOnlyPeer.ID) })
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "crurOwn", Name: "own", OwnerID: users[0]}))
	ownRoom := "crurOwn"
	require.NoError(t, repo.CreateMessage(&model.ChatMessage{ID: "cmur_own1", FromUserID: users[0], ToRoomID: &ownRoom, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}}))
	require.NoError(t, repo.CreateMessage(&model.ChatMessage{ID: "cmur_own2", FromUserID: users[0], ToUserID: &ownOnlyPeer.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}}))
	ownPeer, err := repo.UnreadFromUsers(users[0], []string{ownOnlyPeer.ID})
	require.NoError(t, err)
	assert.False(t, ownPeer[ownOnlyPeer.ID], "a DM the reader sent is not unread for the reader")
	ownRoomUnread, err := repo.UnreadInRooms(users[0], []string{ownRoom})
	require.NoError(t, err)
	assert.False(t, ownRoomUnread[ownRoom], "the reader's own room message is not unread for the reader")
	// 他人の発言をすべて読んだ部屋は既読。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "crurRead", Name: "read", OwnerID: users[1]}))
	readRoom := "crurRead"
	require.NoError(t, repo.CreateMessage(&model.ChatMessage{ID: "cmur_read1", FromUserID: users[1], ToRoomID: &readRoom, Text: &text,
		Reads: model.StringArray{users[0]}, Reactions: model.StringArray{}, Emojis: model.StringArray{}}))
	readRoomUnread, err := repo.UnreadInRooms(users[0], []string{readRoom})
	require.NoError(t, err)
	assert.False(t, readRoomUnread[readRoom], "a room whose messages the reader has read is not unread")
	peerSide, err := repo.UnreadFromUsers(ownOnlyPeer.ID, []string{users[0]})
	require.NoError(t, err)
	assert.True(t, peerSide[users[0]], "the same DM is unread for its recipient")

	peers := append(append([]string{}, users...), "u_chur_missing")
	roomIDs := append(append([]string{}, rooms...), ownRoom, readRoom, "crur_missing")
	var unreadPeers, readPeers, unreadRooms, readRooms int
	for _, reader := range users {
		got, err := repo.UnreadFromUsers(reader, peers)
		require.NoError(t, err)
		for _, p := range peers {
			want, err := repo.HasUnreadFromUser(reader, p)
			require.NoError(t, err)
			assert.Equal(t, want, got[p], "reader=%s peer=%s", reader, p)
			if want {
				unreadPeers++
			} else {
				readPeers++
			}
		}
		gotRooms, err := repo.UnreadInRooms(reader, roomIDs)
		require.NoError(t, err)
		for _, r := range roomIDs {
			want, err := repo.HasUnreadInRoom(reader, r)
			require.NoError(t, err)
			assert.Equal(t, want, gotRooms[r], "reader=%s room=%s", reader, r)
			if want {
				unreadRooms++
			} else {
				readRooms++
			}
		}
	}
	// 片側だけの比較は「常に未読」「常に既読」の実装でも通るので、両方を要求する。
	require.Positive(t, unreadPeers)
	require.Positive(t, readPeers)
	require.Positive(t, unreadRooms)
	require.Positive(t, readRooms)

	empty, err := repo.UnreadFromUsers(users[0], nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
	emptyRooms, err := repo.UnreadInRooms(users[0], nil)
	require.NoError(t, err)
	assert.Empty(t, emptyRooms)
}
