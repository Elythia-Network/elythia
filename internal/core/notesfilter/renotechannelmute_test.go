package notesfilter

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestApplyRenoteChannelMute(t *testing.T) {
	mutedCh := "ch_muted"
	otherCh := "ch_other"
	notes := []*model.Note{
		{ID: "n1"},                            // 非renote
		{ID: "n2", RenoteChannelID: &otherCh}, // 別 channel の renote (保持)
		{ID: "n3", RenoteChannelID: &mutedCh}, // muted channel の renote (除外)
	}
	out := ApplyRenoteChannelMute(notes, []string{mutedCh})
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"n1", "n2"}, ids)
}

func TestApplyRenoteChannelMute_EmptyMutedIsNoop(t *testing.T) {
	ch := "ch1"
	notes := []*model.Note{{ID: "n1", RenoteChannelID: &ch}}
	assert.Len(t, ApplyRenoteChannelMute(notes, nil), 1)
}
