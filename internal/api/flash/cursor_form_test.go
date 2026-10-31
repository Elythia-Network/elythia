package flash

import (
	"net/http"
	"testing"
	"time"

	coreflash "github.com/elythia-network/elythia/internal/core/flash"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cursorRecordingRepo records the cursors ListByUser was called with.
type cursorRecordingRepo struct {
	*testutil.MockFlashRepository
	cursors [][2]string
}

func (r *cursorRecordingRepo) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	r.cursors = append(r.cursors, [2]string{sinceID, untilID})
	return r.MockFlashRepository.ListByUser(userID, sinceID, untilID, limit, offset)
}

// cursorRecordingLikeRepo records the cursors ListByUserSearch was called with.
type cursorRecordingLikeRepo struct {
	*testutil.MockFlashLikeRepository
	cursors [][2]string
}

func (r *cursorRecordingLikeRepo) ListByUserSearch(userID, search, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error) {
	r.cursors = append(r.cursors, [2]string{sinceID, untilID})
	return r.MockFlashLikeRepository.ListByUserSearch(userID, search, sinceID, untilID, limit, offset)
}

// flash/my と flash/my-likes は、本家の makePaginationQuery と同じく ID があれば
// 日付を捨てる。本家に無い i/flashs / i/flashs/likes は、以前の向きごとの組み合わせを
// 残す (#3369)。
func TestCursorForm_MyAndAliases(t *testing.T) {
	const untilDate = int64(1779744000000)
	datePrefix := id.AidxCutoffPrefix(time.UnixMilli(untilDate))
	body := `{"sinceId":"a1","untilDate":1779744000000}`

	repo := &cursorRecordingRepo{MockFlashRepository: testutil.NewMockFlashRepository()}
	likeRepo := &cursorRecordingLikeRepo{MockFlashLikeRepository: testutil.NewMockFlashLikeRepository()}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h := NewHandler(coreflash.NewService(repo, likeRepo, idGen), nil, nil, nil)

	tests := []struct {
		name    string
		call    func(*Handler) func(echo.Context) error
		cursors *[][2]string
		want    [2]string
	}{
		{name: "flash/my drops the date", call: func(h *Handler) func(echo.Context) error { return h.My }, cursors: &repo.cursors, want: [2]string{"a1", ""}},
		{name: "i/flashs keeps the date", call: func(h *Handler) func(echo.Context) error { return h.IFlashs }, cursors: &repo.cursors, want: [2]string{"a1", datePrefix}},
		{name: "flash/my-likes drops the date", call: func(h *Handler) func(echo.Context) error { return h.MyLikes }, cursors: &likeRepo.cursors, want: [2]string{"a1", ""}},
		{name: "i/flashs/likes keeps the date", call: func(h *Handler) func(echo.Context) error { return h.IFlashsLikes }, cursors: &likeRepo.cursors, want: [2]string{"a1", datePrefix}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			*tt.cursors = nil
			c, rec := newReq(t, body)
			setUser(c, "alice")
			require.NoError(t, tt.call(h)(c))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Len(t, *tt.cursors, 1)
			assert.Equal(t, tt.want, (*tt.cursors)[0])
		})
	}
}
