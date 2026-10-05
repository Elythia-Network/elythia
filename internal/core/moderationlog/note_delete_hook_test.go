package moderationlog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteDeleteHook_WritesDeleteNoteLog(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)
	hook := NewNoteDeleteHook(svc)

	host := "remote.example"
	hook.OnModeratorNoteDeleted("mod1",
		&model.Note{ID: "n1", UserID: "author1", UserHost: &host},
		&model.User{ID: "author1", Username: "alice"})

	require.Eventually(t, func() bool { return len(repo.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)
	logs := repo.snapshot()
	assert.Equal(t, "mod1", logs[0].UserID)
	assert.Equal(t, "deleteNote", logs[0].Type)

	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "n1", info["noteId"])
	assert.Equal(t, "author1", info["noteUserId"])
	assert.Equal(t, "remote.example", info["noteUserHost"])
	assert.Equal(t, "alice", info["noteUserUsername"])
}

func TestNoteDeleteHook_NilGuards(t *testing.T) {
	// nil svc receiver -> noop, no panic.
	(&NoteDeleteHook{}).OnModeratorNoteDeleted("mod1", &model.Note{ID: "n1"}, nil)

	repo := &spyRepo{}
	svc := newSvc(t, repo)
	// nil note -> noop.
	NewNoteDeleteHook(svc).OnModeratorNoteDeleted("mod1", nil, nil)
	// local note (UserHost nil) + nil author -> logs with noteUserHost null and no username.
	NewNoteDeleteHook(svc).OnModeratorNoteDeleted("mod1", &model.Note{ID: "n2", UserID: "a2"}, nil)

	require.Eventually(t, func() bool { return len(repo.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)
	var info map[string]any
	require.NoError(t, json.Unmarshal(repo.snapshot()[0].Info, &info))
	assert.Equal(t, "n2", info["noteId"])
	_, hasUsername := info["noteUserUsername"]
	assert.False(t, hasUsername, "nil author omits noteUserUsername")
}
