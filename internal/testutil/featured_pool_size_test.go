package testutil

import (
	"testing"

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
