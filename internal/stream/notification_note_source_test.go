package stream

import (
	"encoding/json"
	"errors"
	"testing"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingNotifNoteRepo counts the reads PackWithNoteSource must not make.
type countingNotifNoteRepo struct {
	note  *model.Note
	reads int
}

func (r *countingNotifNoteRepo) FindByIDWithRelations(string) (*model.Note, error) {
	r.reads++
	return r.note, nil
}

// PackWithNoteSource packs the same body as Pack, taking the note from the
// source instead of reading it (#3611), and keeps the visibility gate.
func TestNotificationPublisher_PackWithNoteSource_MatchesPack(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	text := "hello"
	for _, tc := range []struct {
		name     string
		vis      model.NoteVisibility
		wantNote bool
	}{
		{"public note is embedded", model.NoteVisibilityPublic, true},
		{"followers note is gated for a non-follower", model.NoteVisibilityFollowers, false},
	} {
		note := &model.Note{ID: "note1", UserID: "bob", Visibility: tc.vis, Text: &text, User: &model.User{ID: "bob", Username: "bob"}}
		repo := &countingNotifNoteRepo{note: note}
		np := NewNotificationPublisher(nil)
		np.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "bob", Username: "bob"}}, repo, idGen)
		n := &corenotification.Notification{ID: "x", Type: corenotification.TypeMention, NotifierID: "bob", NoteID: "note1"}

		want, err := json.Marshal(np.Pack("alice", n))
		require.NoError(t, err)
		require.Equal(t, 1, repo.reads)

		got, err := json.Marshal(np.PackWithNoteSource("alice", n, corenotification.LoadedNoteSource(note)))
		require.NoError(t, err)
		assert.Equal(t, 1, repo.reads, "%s: the source replaces the read", tc.name)
		assert.JSONEq(t, string(want), string(got), tc.name)
		var body map[string]any
		require.NoError(t, json.Unmarshal(got, &body))
		assert.Equal(t, tc.wantNote, body["note"] != nil, tc.name)
	}
}

func TestNotificationPublisher_PackWithNoteSource_Fallbacks(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	note := &model.Note{ID: "note1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	n := &corenotification.Notification{ID: "x", Type: corenotification.TypeMention, NotifierID: "bob", NoteID: "note1"}

	np := NewNotificationPublisher(nil)
	assert.Nil(t, np.PackWithNoteSource("alice", nil, nil))

	// note repository 未配線なら、source があっても note を埋めない (従来と同じ)。
	np.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "bob"}}, nil, idGen)
	body, ok := np.PackWithNoteSource("alice", n, corenotification.LoadedNoteSource(note)).(map[string]any)
	require.True(t, ok)
	assert.Nil(t, body["note"])

	// source が nil なら repository から読む。
	repo := &countingNotifNoteRepo{note: note}
	np.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "bob"}}, repo, idGen)
	body, ok = np.PackWithNoteSource("alice", n, nil).(map[string]any)
	require.True(t, ok)
	assert.NotNil(t, body["note"])
	assert.Equal(t, 1, repo.reads)

	// 読み込みの失敗は note を落とす。
	failing := corenotification.NewNoteSource(func() (*model.Note, error) { return nil, errors.New("boom") })
	body, ok = np.PackWithNoteSource("alice", n, failing).(map[string]any)
	require.True(t, ok)
	assert.Nil(t, body["note"])
	assert.Equal(t, 1, repo.reads)
}
