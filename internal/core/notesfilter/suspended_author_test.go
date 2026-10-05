package notesfilter

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestHasSuspendedAuthor(t *testing.T) {
	suspended := &model.User{ID: "x", IsSuspended: true}
	active := &model.User{ID: "y"}

	tests := []struct {
		name   string
		note   *model.Note
		author *model.User
		want   bool
	}{
		{name: "nil note", note: nil, author: suspended, want: false},
		{name: "active author", note: &model.Note{ID: "n"}, author: active, want: false},
		{name: "suspended author", note: &model.Note{ID: "n"}, author: suspended, want: true},
		{
			name:   "suspended author on the note itself",
			note:   &model.Note{ID: "n", User: suspended},
			author: nil,
			want:   true,
		},
		{
			name:   "suspended renote target",
			note:   &model.Note{ID: "n", Renote: &model.Note{ID: "r", User: suspended}},
			author: active,
			want:   true,
		},
		{
			name:   "suspended reply target",
			note:   &model.Note{ID: "n", Reply: &model.Note{ID: "r", User: suspended}},
			author: active,
			want:   true,
		},
		{
			name:   "active renote target",
			note:   &model.Note{ID: "n", Renote: &model.Note{ID: "r", User: active}},
			author: active,
			want:   false,
		},
		{
			// relation が未取得なら判定材料が無いので通す。SQL 側の
			// `"renoteUserId" IS NULL OR NOT EXISTS (...)` と揃える。
			name:   "renote target not preloaded",
			note:   &model.Note{ID: "n", RenoteID: strPtr("r"), RenoteUserID: strPtr("x")},
			author: active,
			want:   false,
		},
		{
			// 埋め込みはあるが User relation が無い場合も判定しない。
			name:   "renote target without user relation",
			note:   &model.Note{ID: "n", Renote: &model.Note{ID: "r", UserID: "x"}},
			author: active,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HasSuspendedAuthor(tt.note, tt.author))
		})
	}
}
