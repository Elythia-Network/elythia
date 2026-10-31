package gallery_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gallery/posts は、本家と同じく makePaginationQuery の形で ID と日付を組み合わせる。
// ID が1つでもあれば日付を捨て、sinceId だけなら昇順で返す (#3369)。
func TestPosts_IDCursorDropsDateCursor(t *testing.T) {
	cleanup()
	defer cleanup()
	at := func(year int) time.Time { return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC) }
	// aidx の ID は先頭 8 文字が時刻なので、日付のカーソルと比べられる。
	postID := func(year int) string { return id.AidxCutoffPrefix(at(year))[:8] + "gp000001" }
	for _, year := range []int{2020, 2021, 2022} {
		seedPost(t, &model.GalleryPost{ID: postID(year), UpdatedAt: time.Now(), Title: "y", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	}
	ms := func(year int) int64 { return at(year).AddDate(0, 6, 0).UnixMilli() }

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "sinceId ignores untilDate and sorts ascending",
			body: fmt.Sprintf(`{"sinceId":%q,"untilDate":%d}`, postID(2020), ms(2020)),
			want: []string{postID(2021), postID(2022)},
		},
		{
			name: "untilId ignores sinceDate",
			body: fmt.Sprintf(`{"untilId":%q,"sinceDate":%d}`, postID(2022), ms(2021)),
			want: []string{postID(2021), postID(2020)},
		},
		{
			name: "dates alone still bound both sides",
			body: fmt.Sprintf(`{"sinceDate":%d,"untilDate":%d}`, ms(2020), ms(2021)),
			want: []string{postID(2021)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPost(newHandler().Posts, tt.body, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp []map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			got := make([]string, 0, len(resp))
			for _, p := range resp {
				got = append(got, p["id"].(string))
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
