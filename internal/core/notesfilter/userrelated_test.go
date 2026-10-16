package notesfilter_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// TestIsUserRelated pins the port of upstream misc/is-user-related.ts.
func TestIsUserRelated(t *testing.T) {
	set := map[string]struct{}{"m": {}}
	tests := []struct {
		name         string
		note         *model.Note
		ignoreAuthor bool
		want         bool
	}{
		{"nil note", nil, false, false},
		{"author in set", &model.Note{UserID: "m"}, false, true},
		{"author in set is ignored with ignoreAuthor", &model.Note{UserID: "m"}, true, false},
		{"unrelated note", &model.Note{UserID: "a"}, false, false},
		{"reply target author in set", &model.Note{UserID: "a", ReplyUserID: strp("m")}, true, true},
		{"renote target author in set", &model.Note{UserID: "a", RenoteUserID: strp("m")}, true, true},
		{"reply target from embed when column is nil", &model.Note{UserID: "a", Reply: &model.Note{UserID: "m"}}, true, true},
		{"renote target from embed when column is nil", &model.Note{UserID: "a", Renote: &model.Note{UserID: "m"}}, true, true},
		{"column wins over embed", &model.Note{UserID: "a", RenoteUserID: strp("x"), Renote: &model.Note{UserID: "m"}}, true, false},
		{"self reply of a set member is not matched through the target", &model.Note{UserID: "m", ReplyUserID: strp("m")}, true, false},
		{"self renote of a set member is not matched through the target", &model.Note{UserID: "m", RenoteUserID: strp("m")}, true, false},
		{"set member renoting another set-free user", &model.Note{UserID: "m", RenoteUserID: strp("a")}, true, false},
		{"depth 2 is not inspected", &model.Note{UserID: "a", RenoteUserID: strp("b"), Renote: &model.Note{UserID: "b", ReplyUserID: strp("m")}}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, notesfilter.IsUserRelated(tt.note, set, tt.ignoreAuthor))
		})
	}
	assert.False(t, notesfilter.IsUserRelated(&model.Note{UserID: "m"}, nil, false), "empty set never matches")
}

func TestDropUserRelated(t *testing.T) {
	notes := []*model.Note{
		{ID: "own", UserID: "m"},
		{ID: "renote", UserID: "a", RenoteUserID: strp("m")},
		nil,
		{ID: "plain", UserID: "a"},
	}
	set := map[string]struct{}{"m": {}}

	ids := func(ns []*model.Note) []string {
		out := []string{}
		for _, n := range ns {
			out = append(out, n.ID)
		}
		return out
	}
	assert.Equal(t, []string{"own", "plain"}, ids(notesfilter.DropUserRelated(notes, set, true)))
	assert.Equal(t, []string{"plain"}, ids(notesfilter.DropUserRelated(notes, set, false)))
	// 空の集合は入力をそのまま返す (nil も含めて)。
	assert.Len(t, notesfilter.DropUserRelated(notes, nil, false), 4)
	assert.Empty(t, notesfilter.DropUserRelated(nil, set, false))
}
