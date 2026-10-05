package search

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestNoteIndexHook_ForwardsCreatedDeleted(t *testing.T) {
	provider := &recordingProvider{}
	svc := NewService(provider)
	hook := NewNoteIndexHook(svc)

	hook.OnNoteCreated(&model.Note{ID: "n1"})
	hook.OnNoteDeleted(&model.Note{ID: "n1"})

	assert.Equal(t, 1, provider.indexCalls)
	assert.Equal(t, 1, provider.unindexCalls)
}

func TestNoteIndexHook_NilSafety(t *testing.T) {
	// nil receiver / nil service / nil note のいずれも panic を起こさず
	// 何もしないことを確認する。
	var hook *NoteIndexHook
	hook.OnNoteCreated(&model.Note{ID: "x"})
	hook.OnNoteDeleted(&model.Note{ID: "x"})

	hook = NewNoteIndexHook(nil)
	hook.OnNoteCreated(&model.Note{ID: "x"})
	hook.OnNoteDeleted(&model.Note{ID: "x"})

	hook = NewNoteIndexHook(NewService(&recordingProvider{}))
	hook.OnNoteCreated(nil)
	hook.OnNoteDeleted(nil)
}
