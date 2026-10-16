package repository

import (
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// BenchmarkFindManyByIDsWithUser measures the timeline note-hydration hot path:
// fetching a page of note IDs with all preloaded relations (User / Renote /
// Renote.User / Renote.Poll / Reply / Reply.User / Reply.Poll / Poll). GORM
// issues a separate query per preload, so this benchmark surfaces the
// round-trip cost that the timeline read path pays for every page. Run with:
//
//	go test ./internal/repository -bench BenchmarkFindManyByIDsWithUser -benchmem -run x
func BenchmarkFindManyByIDsWithUser(b *testing.B) {
	repo := NewNoteRepository(testDB)

	const users = 50
	const notes = 200
	const pageSize = 40

	// 前回 run が seed 途中で中断して b.Cleanup を逃した場合の残骸を消してから
	// seed する (= bu_%/bn_% の PK 衝突で再実行不能になるのを防ぐ、idempotent start)。
	cleanup := func() {
		testDB.Exec(`DELETE FROM "note" WHERE id LIKE 'bn_%'`)
		testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'bu_%'`)
	}
	cleanup()
	b.Cleanup(cleanup)

	userIDs := make([]string, users)
	for i := 0; i < users; i++ {
		uid := fmt.Sprintf("bu_%d", i)
		userIDs[i] = uid
		u := &model.User{ID: uid, Username: uid, UsernameLower: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		if err := testDB.Create(u).Error; err != nil {
			b.Fatalf("seed user: %v", err)
		}
	}

	noteIDs := make([]string, notes)
	text := "benchmark note body with some text content"
	for i := 0; i < notes; i++ {
		nid := fmt.Sprintf("bn_%d", i)
		noteIDs[i] = nid
		n := &model.Note{
			ID:         nid,
			UserID:     userIDs[i%users],
			Text:       &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte(`{"👍":3,"❤":1}`)),
			FileIDs:    model.StringArray{},
		}
		// 約 1/4 を renote / reply にして preload (Renote / Reply 系) を踏ませる。
		if i >= 8 && i%4 == 0 {
			target := noteIDs[i-8]
			n.RenoteID = &target
		} else if i >= 8 && i%4 == 1 {
			target := noteIDs[i-8]
			n.ReplyID = &target
		}
		if err := testDB.Create(n).Error; err != nil {
			b.Fatalf("seed note: %v", err)
		}
	}

	page := noteIDs[:pageSize]

	// Batched: 現行 FindManyByIDsWithUser (#1368 で batch-IN 4 query 化)。
	b.Run("Batched", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			got, err := repo.FindManyByIDsWithUser(page)
			if err != nil {
				b.Fatalf("FindManyByIDsWithUser: %v", err)
			}
			if len(got) != pageSize {
				b.Fatalf("got %d notes, want %d", len(got), pageSize)
			}
		}
	})

	// Preloads: 旧経路 (preloadNoteRelations の 8 preload = ~9 query)。batch との
	// 比較で往復削減の効果を測る。
	b.Run("Preloads", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var notes []*model.Note
			if err := preloadNoteRelations(testDB).Where("id IN ?", page).Find(&notes).Error; err != nil {
				b.Fatalf("preload find: %v", err)
			}
			if len(notes) != pageSize {
				b.Fatalf("got %d notes, want %d", len(notes), pageSize)
			}
		}
	})

	// NoPreload: 素の `id IN (...)` 1 query (relation 無し)。理論下限。
	b.Run("NoPreload", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var notes []*model.Note
			if err := testDB.Where("id IN ?", page).Find(&notes).Error; err != nil {
				b.Fatalf("bare find: %v", err)
			}
			if len(notes) != pageSize {
				b.Fatalf("got %d notes, want %d", len(notes), pageSize)
			}
		}
	})
}

// BenchmarkFindByIDWithRelations compares the single-note read the write paths
// use (reaction, notification packing) between the hydrate (#3611) and the
// former Preload chain, on a pure renote of a quote (the deepest embed) and on
// a plain note. Run with:
//
//	go test ./internal/repository -bench BenchmarkFindByIDWithRelations -benchmem -run x
func BenchmarkFindByIDWithRelations(b *testing.B) {
	repo := NewNoteRepository(testDB)
	cleanup := func() {
		testDB.Exec(`DELETE FROM "poll" WHERE "noteId" LIKE 'bf3611_%'`)
		testDB.Exec(`DELETE FROM "note" WHERE id LIKE 'bf3611_%'`)
		testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'bf3611u_%'`)
	}
	cleanup()
	b.Cleanup(cleanup)
	for i := 0; i < 3; i++ {
		uid := fmt.Sprintf("bf3611u_%d", i)
		if err := testDB.Create(&model.User{ID: uid, Username: uid, UsernameLower: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error; err != nil {
			b.Fatalf("seed user: %v", err)
		}
	}
	text := "body"
	mk := func(id, uid string, renote *string, poll bool) {
		n := &model.Note{ID: id, UserID: uid, Text: &text, Visibility: model.NoteVisibilityPublic, RenoteID: renote, HasPoll: poll, Reactions: datatypes.JSON([]byte(`{}`)), FileIDs: model.StringArray{}}
		if renote != nil && id == "bf3611_boost" {
			n.Text = nil
		}
		if err := testDB.Create(n).Error; err != nil {
			b.Fatalf("seed note: %v", err)
		}
		if poll {
			if err := testDB.Create(&model.Poll{NoteID: id, Choices: model.StringArray{"a"}, Votes: model.Int64Array{0}, NoteVisibility: model.NoteVisibilityPublic, UserID: uid}).Error; err != nil {
				b.Fatalf("seed poll: %v", err)
			}
		}
	}
	quoted, quote := "bf3611_quoted", "bf3611_quote"
	mk(quoted, "bf3611u_0", nil, true)
	mk(quote, "bf3611u_1", &quoted, false)
	mk("bf3611_boost", "bf3611u_2", &quote, false)
	mk("bf3611_plain", "bf3611u_0", nil, false)

	for _, id := range []string{"bf3611_boost", "bf3611_plain"} {
		b.Run(id+"/Hydrate", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := repo.FindByIDWithRelations(id); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(id+"/Preload", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var n model.Note
				if err := preloadNoteRelations(testDB).First(&n, "id = ?", id).Error; err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
