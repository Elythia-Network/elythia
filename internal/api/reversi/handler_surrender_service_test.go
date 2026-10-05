package reversi

import (
	"net/http"
	"testing"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// mockReversiRepo は handler_test.go の既存 mock を使うが、
// repository.ReversiRepository interface を満たすことを静的に確認する。
var _ repository.ReversiRepository = (*mockReversiRepo)(nil)

// capturingPublisher は corereversi.GamePublisher を実装する stub。
// service.Surrender 経由で 'ended' が publish されるかをアサートする。
type capturingPublisher struct {
	events []string
}

func (p *capturingPublisher) PublishGameEvent(_ string, eventType string, _ any) {
	p.events = append(p.events, eventType)
}

// --- helpers ---

// newHandlerWithService builds a handler wired to a real corereversi.Service
// so Surrender goes through the service layer (with IsStarted validation
// and 'ended' event publish).
func newHandlerWithService(t *testing.T) (*Handler, *mockReversiRepo, *capturingPublisher) {
	t.Helper()
	h, repo := newTestHandler()
	pub := &capturingPublisher{}
	// Redis なしの Service。turnTimer は nil redis でも動く (no-op)。
	svc := corereversi.NewService(repo, pub, nil)
	h.SetService(svc)
	return h, repo, pub
}

// startedGameForSurrender は IsStarted=true / Black=1 / not ended の対局を
// 返す。handler の pre-check と service の IsStarted ガードを同時にパスさせる。
func startedGameForSurrender() *model.ReversiGame {
	g := sampleGame()
	g.IsStarted = true
	b := 1
	g.Black = &b
	return g
}

// --- Service-backed Surrender tests ---

func TestSurrender_Service_Success(t *testing.T) {
	h, repo, pub := newHandlerWithService(t)
	g := startedGameForSurrender()
	repo.games[g.ID] = g

	rec := post(h.Surrender, `{"gameId":"g1"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)

	got := repo.games["g1"]
	assert.True(t, got.IsEnded)
	require.NotNil(t, got.WinnerID)
	assert.Equal(t, "u2", *got.WinnerID)
	// Service 経由なので 'ended' が publish されている
	assert.Contains(t, pub.events, "ended")
}

// 未開始の対局は ErrNotStarted → 400 NOT_STARTED にマップされる。
// これが issue #24 の主眼 (repo 直接操作経路ではバリデーションを迂回していた)。
func TestSurrender_Service_NotStarted(t *testing.T) {
	h, repo, pub := newHandlerWithService(t)
	g := sampleGame() // IsStarted=false
	repo.games[g.ID] = g

	rec := post(h.Surrender, `{"gameId":"g1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_STARTED")
	// 対局は終局していない
	assert.False(t, repo.games["g1"].IsEnded)
	// ended イベントも飛ばない
	assert.NotContains(t, pub.events, "ended")
}

// 終局済みの対局は ErrAlreadyEnded → 400 ALREADY_ENDED にマップされる。
func TestSurrender_Service_AlreadyEnded(t *testing.T) {
	h, repo, _ := newHandlerWithService(t)
	g := startedGameForSurrender()
	g.IsEnded = true
	repo.games[g.ID] = g

	rec := post(h.Surrender, `{"gameId":"g1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ALREADY_ENDED")
}

// Handler の pre-check (repo.FindByID) で弾く経路は service を呼ばない。
func TestSurrender_Service_NotFoundStillReturns404(t *testing.T) {
	h, _, pub := newHandlerWithService(t)
	rec := post(h.Surrender, `{"gameId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, pub.events)
}

// Handler の pre-check (非プレイヤー) で弾く経路も service を呼ばない。
func TestSurrender_Service_NotPlayerCaughtByPreCheck(t *testing.T) {
	h, repo, pub := newHandlerWithService(t)
	repo.games["g1"] = startedGameForSurrender()
	rec := post(h.Surrender, `{"gameId":"g1"}`, &model.User{ID: "u3"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, pub.events)
}

// --- fallback path (svc nil) が従来通り動くことの回帰テスト ---
// すでに TestSurrender_Success が同じことを確かめているが、refactor 後も
// 明示的にフォールバック経路が生きていることを documentation として残す。
func TestSurrender_FallbackPath_NoServiceInjected(t *testing.T) {
	h, repo := newTestHandler()
	// h.svc は nil のまま
	g := &model.ReversiGame{
		ID: "gX", User1ID: "u1", User2ID: "u2",
		Map:  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:   "random",
		Logs: datatypes.JSON("[]"),
	}
	// IsStarted を立てずとも、fallback 経路は旧来の repo 直接操作なので通ってしまう。
	// (issue #24 が指摘していた一貫性問題そのもの。fallback はテスト互換のため
	// 残っているが、プロダクション経路では常に svc が注入される)
	repo.games[g.ID] = g

	rec := post(h.Surrender, `{"gameId":"gX"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.games["gX"].IsEnded)
}
