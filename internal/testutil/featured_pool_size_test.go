package testutil

import (
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// TestFeaturedPoolSizeMatchesRepo は #1491 review B 指摘の drift 検出。
// mockFeaturedNotesPerUserPoolSize と repository.FeaturedNotesPerUserPoolSize
// は users/featured-notes の selection 段 pool cap として同値でなければ
// ならない。production code から repository を import すると repository の
// test build を介して import cycle になるため (mock_chat_test.go と同じ
// pattern)、_test.go で runtime に等価性を確認する。
//
// 一方を変更してもう一方を見落とした PR がここで落ちる。CI が走る
// internal/testutil パッケージのテストとして実行されるので、CI で必ず検出。
func TestFeaturedPoolSizeMatchesRepo(t *testing.T) {
	if mockFeaturedNotesPerUserPoolSize != repository.FeaturedNotesPerUserPoolSize {
		t.Fatalf(
			"pool size drift: mockFeaturedNotesPerUserPoolSize=%d != repository.FeaturedNotesPerUserPoolSize=%d. "+
				"両者は users/featured-notes の selection 段 pool cap として同値でなければならない。"+
				"一方だけ変更すると handler 層の TestFeaturedNotes_* が mock 経由で pass する一方、"+
				"実 SQL の挙動と乖離する (#1491 review)。",
			mockFeaturedNotesPerUserPoolSize,
			repository.FeaturedNotesPerUserPoolSize,
		)
	}
}

// TestFeaturedCandidateWindowMatchesRepo checks the mock's candidate window
// against the real repository's (#3612). 理由は TestFeaturedPoolSizeMatchesRepo と同じ。
func TestFeaturedCandidateWindowMatchesRepo(t *testing.T) {
	if mockFeaturedNotesPerUserCandidateWindow != repository.FeaturedNotesPerUserCandidateWindow {
		t.Fatalf(
			"candidate window drift: mockFeaturedNotesPerUserCandidateWindow=%d != repository.FeaturedNotesPerUserCandidateWindow=%d",
			mockFeaturedNotesPerUserCandidateWindow,
			repository.FeaturedNotesPerUserCandidateWindow,
		)
	}
}

// TestMockListFeaturedByUserCandidateWindow checks that the mock, like the real
// repository, only ranks the newest visible notes (#3612).
func TestMockListFeaturedByUserCandidateWindow(t *testing.T) {
	m := NewMockNoteRepository()
	const oldID = "win0000"
	m.Notes[oldID] = &model.Note{ID: oldID, UserID: "u", Visibility: "public", RenoteCount: 100}
	for i := 1; i <= mockFeaturedNotesPerUserCandidateWindow; i++ {
		id := fmt.Sprintf("win%04d", i)
		m.Notes[id] = &model.Note{ID: id, UserID: "u", Visibility: "public"}
	}

	got, err := m.ListFeaturedByUser("u", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != mockFeaturedNotesPerUserPoolSize {
		t.Fatalf("len = %d, want %d", len(got), mockFeaturedNotesPerUserPoolSize)
	}
	for _, n := range got {
		if n.ID == oldID {
			t.Fatalf("a note older than the candidate window was selected")
		}
	}
}
