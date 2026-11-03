package federation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// 連合の招待が受けられたら (Join)、ローカルの招待した人に知らせる (#3946)。

type recordingMatched struct {
	inviters []string
	games    []string
}

func (r *recordingMatched) ReversiMatched(inviterID string, game *model.ReversiGame) {
	r.inviters = append(r.inviters, inviterID)
	r.games = append(r.games, game.ID)
}

func joinBody(sessionID string) []byte {
	return []byte(`{
		"type": "Join",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "` + sessionID + `"}
		}
	}`)
}

func TestReversiInbox_Join_TellsTheLocalInviter(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	game := seedFederatedGame(t, b, "sess-matched-1", false)
	rec := &recordingMatched{}
	b.reversiSvc.SetMatchedNotifier(rec)
	b.reversiSvc.MarkInviterWaiting(context.Background(), game.ID)

	require.NoError(t, b.processor.Process(joinBody("sess-matched-1")))
	assert.Equal(t, []string{"bob"}, rec.inviters)
	assert.Equal(t, []string{game.ID}, rec.games)
	assert.True(t, b.reversiSvc.InvitationAcceptedRecently(context.Background(), game.ID))

	// 届き直した Join では知らせ直さない
	require.NoError(t, b.processor.Process(joinBody("sess-matched-1")))
	assert.Len(t, rec.inviters, 1)
}

// 受けたと言えるのは招待された本人だけ。別の人の Join では知らせない。
func TestReversiInbox_Join_FromSomeoneElseIsIgnored(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	game := seedFederatedGame(t, b, "sess-matched-2", false)
	game.User2ID = "carol"
	require.NoError(t, b.gameRepo.Update(game))
	rec := &recordingMatched{}
	b.reversiSvc.SetMatchedNotifier(rec)
	b.reversiSvc.MarkInviterWaiting(context.Background(), game.ID)

	require.NoError(t, b.processor.Process(joinBody("sess-matched-2")))
	assert.Empty(t, rec.inviters)
	assert.False(t, b.reversiSvc.InvitationAcceptedRecently(context.Background(), game.ID))
}

// 始まった対局への Join では知らせない。
func TestReversiInbox_Join_StartedGameIsIgnored(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	game := seedFederatedGame(t, b, "sess-matched-3", true)
	rec := &recordingMatched{}
	b.reversiSvc.SetMatchedNotifier(rec)
	b.reversiSvc.MarkInviterWaiting(context.Background(), game.ID)

	require.NoError(t, b.processor.Process(joinBody("sess-matched-3")))
	assert.Empty(t, rec.inviters)
}

// 取り消された後に届いた Join (対局が無い) は受け流す。DB の障害はエラーにして、
// ジョブの再試行に任せる。
func TestReversiInbox_Join_GameGone(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	game := seedFederatedGame(t, b, "sess-matched-4", false)
	require.NoError(t, b.gameRepo.Delete(game.ID))
	require.NoError(t, b.processor.Process(joinBody("sess-matched-4")))

	seedFederatedGame(t, b, "sess-matched-5", false)
	b.gameRepo.findErr = errors.New("db down")
	assert.Error(t, b.processor.Process(joinBody("sess-matched-5")))
}

func inviteBody(sessionID string) []byte {
	return []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "` + sessionID + `"}
		}
	}`)
}

// 受けてから 3 分を過ぎた対局 (受けた後に招待した人が去って残ったもの) に、改めて
// 届いた招待を付け替えない (#3949)。付け替えると、受けた印が残ったまま招待一覧から
// 外れ、受けようとすると逆向きの招待になる。受けてから 3 分の間は使い回す
// (CherryPick は招待を 5 秒ごとに送り直すので、行を増やさない)。
func TestReversiInbox_ReinviteAfterAnAcceptedGame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ago     time.Duration
		newGame bool
	}{
		{"accepted long ago", time.Hour, true},
		{"accepted just now", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newReversiProcessor(t)
			registerLocalBob(t, b.userRepo)
			require.NoError(t, b.processor.Process(inviteBody("sess-old")))
			old := b.gameRepo.findGameBySession(t, b.fedCache, "sess-old")
			require.NotNil(t, old)
			require.NoError(t, fedTestRedis.Client.Set(context.Background(), "reversi:game:accepted:"+old.ID, time.Now().Add(-tc.ago).UnixMilli(), time.Hour).Err())

			require.NoError(t, b.processor.Process(inviteBody("sess-new")))
			got := b.gameRepo.findGameBySession(t, b.fedCache, "sess-new")
			require.NotNil(t, got)
			if tc.newGame {
				assert.NotEqual(t, old.ID, got.ID, "新しい対局にする")
				assert.Equal(t, "bob", got.User2ID)
			} else {
				assert.Equal(t, old.ID, got.ID, "使い回す")
			}
		})
	}
}
