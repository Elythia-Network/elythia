package notesfilter

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func strptr(s string) *string { return &s }

func TestApplyThreadMute_EmptyListIsNoop(t *testing.T) {
	notes := []*model.Note{{ID: "n1"}}
	assert.Len(t, ApplyThreadMute(notes, nil), 1)
}

func TestApplyThreadMute_DropsByRootID(t *testing.T) {
	// note.id がそのまま muted thread root のケース。
	notes := []*model.Note{{ID: "root"}, {ID: "other"}}
	out := ApplyThreadMute(notes, []string{"root"})
	assert.Len(t, out, 1)
	assert.Equal(t, "other", out[0].ID)
}

func TestApplyThreadMute_DropsByThreadID(t *testing.T) {
	// reply note の threadId が muted thread のケース。
	notes := []*model.Note{
		{ID: "reply1", ThreadID: strptr("root")},
		{ID: "reply2", ThreadID: strptr("elsewhere")},
		{ID: "noThread"},
	}
	out := ApplyThreadMute(notes, []string{"root"})
	assert.Len(t, out, 2)
	assert.Equal(t, "reply2", out[0].ID)
	assert.Equal(t, "noThread", out[1].ID)
}

func TestApplyThreadMute_NilNoteSkipped(t *testing.T) {
	notes := []*model.Note{nil, {ID: "n1"}}
	out := ApplyThreadMute(notes, []string{"x"})
	assert.Len(t, out, 1)
	assert.Equal(t, "n1", out[0].ID)
}
