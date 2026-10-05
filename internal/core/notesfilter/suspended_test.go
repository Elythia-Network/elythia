package notesfilter

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestApplySuspended(t *testing.T) {
	notes := []*model.Note{
		{ID: "n1", User: &model.User{ID: "u1", IsSuspended: false}},
		{ID: "n2", User: &model.User{ID: "u2", IsSuspended: true}},
		{ID: "n3", User: nil}, // User 未preload は除外しない (評価不能)
	}
	out := ApplySuspended(notes)
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"n1", "n3"}, ids, "suspended 著者の note のみ除外、User nil は保持")
}

func TestApplySuspended_Empty(t *testing.T) {
	assert.Empty(t, ApplySuspended(nil))
}
