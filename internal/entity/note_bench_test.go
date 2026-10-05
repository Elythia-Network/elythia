package entity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// BenchmarkPackNotes measures the pure CPU/alloc cost of packing a timeline
// page of notes into the response shape (no DB). Lookups are nil (local notes,
// no remote instance/emoji resolution) so this isolates the per-note struct
// building / serialization-prep cost paid on every timeline read. Run with:
//
//	go test ./internal/entity -bench BenchmarkPackNotes -benchmem -run x
func BenchmarkPackNotes(b *testing.B) {
	idGen, _ := id.NewGenerator("aidx")
	const pageSize = 40
	text := "benchmark note body with some text content and a #hashtag"

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	notes := make([]*model.Note, pageSize)
	for i := 0; i < pageSize; i++ {
		uid := fmt.Sprintf("u%d", i%10)
		notes[i] = &model.Note{
			ID:         idGen.Generate(base.Add(time.Duration(i) * time.Second)),
			UserID:     uid,
			Text:       &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte(`{"👍":5,"❤":2,"🎉":1}`)),
			FileIDs:    model.StringArray{"f1", "f2"},
			User:       &model.User{ID: uid, Username: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := PackNotes(context.Background(), notes, idGen, nil, nil, nil)
		if len(out) != pageSize {
			b.Fatalf("packed %d, want %d", len(out), pageSize)
		}
	}
}
