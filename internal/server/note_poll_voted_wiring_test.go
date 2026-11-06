package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **notes の handler も、router が作る共通の NoteFieldResolver で投票を埋める
// (#4044)。**
//
// 以前は notes の handler が resolver を自前で作り直しており、共通の resolver に
// だけ付けた投票の lookup が抜けていた。そのため notes/show は、投票した本人にも
// 全選択肢を isVoted: false で返した (users/notes などの共通の resolver を使う
// 経路は正しかった)。ここでは両方の経路で、投票した選択肢だけが true になることを
// 確かめる。
func TestNewServer_NotesHandlerResolvesPollVotes(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		authorID = "pvauthor4044aaaa"
		voterID  = "pvvoter4044aaaaa"
		token    = "pvtokenx4044aaaa"
	)
	noteID := testIDGen(t).Generate(time.Now())
	cleanupPluginAccountUsers(t, db, "pv_author", "pv_voter")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "poll_vote" WHERE "noteId" = ?`, noteID)
		db.Exec(`DELETE FROM "poll" WHERE "noteId" = ?`, noteID)
		db.Exec(`DELETE FROM "note" WHERE "id" = ?`, noteID)
	})
	createLocalUser(t, db, authorID, "pv_author", "pvtokenx4044bbbb", "pv-pass", false)
	createLocalUser(t, db, voterID, "pv_voter", token, "pv-pass", false)
	text := "which one?"
	require.NoError(t, db.Create(&model.Note{
		ID: noteID, UserID: authorID, Visibility: model.NoteVisibility("public"), Text: &text, HasPoll: true,
	}).Error)
	require.NoError(t, db.Create(&model.Poll{
		NoteID: noteID, Choices: model.StringArray{"a", "b", "c"}, Votes: model.Int64Array{0, 1, 0},
		NoteVisibility: model.NoteVisibility("public"), UserID: authorID,
	}).Error)
	require.NoError(t, db.Create(&model.PollVote{
		ID: testIDGen(t).Generate(time.Now()), UserID: voterID, NoteID: noteID, Choice: 1,
	}).Error)

	srv := newServerWithConfig(t, func(*config.Config) {})

	type packedNote struct {
		ID   string `json:"id"`
		Poll *struct {
			Choices []struct {
				Text    string `json:"text"`
				IsVoted bool   `json:"isVoted"`
			} `json:"choices"`
		} `json:"poll"`
	}
	assertVoted := func(t *testing.T, n packedNote, body string) {
		t.Helper()
		require.NotNil(t, n.Poll, body)
		require.Len(t, n.Poll.Choices, 3, body)
		for i, c := range n.Poll.Choices {
			assert.Equal(t, i == 1, c.IsVoted, "choice %d (%s): %s", i, c.Text, body)
		}
	}

	t.Run("notes/show", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/notes/show", token, map[string]any{"noteId": noteID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var n packedNote
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &n), rec.Body.String())
		assertVoted(t, n, rec.Body.String())
	})
	t.Run("users/notes", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/users/notes", token, map[string]any{"userId": authorID, "limit": 1})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var ns []packedNote
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ns), rec.Body.String())
		require.Len(t, ns, 1, rec.Body.String())
		assertVoted(t, ns[0], rec.Body.String())
	})
}
