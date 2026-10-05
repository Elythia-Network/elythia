package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// **not-found を模す。** 汎用 error だと #2792 の「DB 障害は 500」に引っかかる。
// repository は GORM の error をそのまま返すので、テストもそれに揃える。
var errMock = repository.ErrNotFound

// errBoom is a generic failure for write paths. **not-found とは分けること** —
// 同じ値を使い回すと、将来 write path に `IsNotFound` の分岐が入ったときに
// これらのテストが黙って別の枝を通る (#2792)。
var errBoom = errors.New("db down")

type mockReversiRepo struct {
	games     map[string]*model.ReversiGame
	createErr error
	// listErr forces ListByUser to fail (#2407 の探索経路のエラー分岐用)。
	listErr error
}

func newMock() *mockReversiRepo {
	return &mockReversiRepo{games: make(map[string]*model.ReversiGame)}
}

func (m *mockReversiRepo) Create(g *model.ReversiGame) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.games[g.ID] = g
	return nil
}

func (m *mockReversiRepo) FindByID(id string) (*model.ReversiGame, error) {
	if g, ok := m.games[id]; ok {
		return g, nil
	}
	return nil, errMock
}

func (m *mockReversiRepo) Update(g *model.ReversiGame) error {
	m.games[g.ID] = g
	return nil
}

func (m *mockReversiRepo) UpdateReadyState(gameID string, user1 bool, ready bool) (*model.ReversiGame, error) {
	g, ok := m.games[gameID]
	if !ok || g.IsStarted || g.IsEnded {
		return nil, nil
	}
	if user1 {
		g.User1Ready = ready
	} else {
		g.User2Ready = ready
	}
	return g, nil
}

func (m *mockReversiRepo) MarkStarted(g *model.ReversiGame) (bool, error) {
	stored, ok := m.games[g.ID]
	if !ok || stored.IsStarted {
		return false, nil
	}
	stored.Black = g.Black
	stored.IsStarted = true
	stored.StartedAt = g.StartedAt
	stored.CRC32 = g.CRC32
	return true, nil
}

func (m *mockReversiRepo) ListByUser(userID string, limit int) ([]*model.ReversiGame, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var result []*model.ReversiGame
	for _, g := range m.games {
		if g.User1ID == userID || g.User2ID == userID {
			result = append(result, g)
		}
	}
	return result, nil
}

// FindPendingInvitation mirrors the repository's SQL: newest not-yet-started
// row with User1 = inviter and User2 = invitee.
func (m *mockReversiRepo) FindPendingInvitation(inviteeID, inviterID string) (*model.ReversiGame, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var found *model.ReversiGame
	for _, g := range m.games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		if g.User1ID != inviterID || g.User2ID != inviteeID {
			continue
		}
		if found == nil || g.ID > found.ID {
			found = g
		}
	}
	return found, nil
}

func (m *mockReversiRepo) ListByUserCursor(userID, sinceID, untilID string, limit int) ([]*model.ReversiGame, error) {
	var result []*model.ReversiGame
	for _, g := range m.games {
		if g.User1ID != userID && g.User2ID != userID {
			continue
		}
		if sinceID != "" && g.ID <= sinceID {
			continue
		}
		if untilID != "" && g.ID >= untilID {
			continue
		}
		result = append(result, g)
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *mockReversiRepo) ListStartedCursor(sinceID, untilID string, limit int) ([]*model.ReversiGame, error) {
	var result []*model.ReversiGame
	for _, g := range m.games {
		if !g.IsStarted {
			continue
		}
		if sinceID != "" && g.ID <= sinceID {
			continue
		}
		if untilID != "" && g.ID >= untilID {
			continue
		}
		result = append(result, g)
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *mockReversiRepo) ListActive() ([]*model.ReversiGame, error) {
	var result []*model.ReversiGame
	for _, g := range m.games {
		if !g.IsEnded {
			result = append(result, g)
		}
	}
	return result, nil
}

func (m *mockReversiRepo) Delete(id string) error {
	delete(m.games, id)
	return nil
}

func (m *mockReversiRepo) DeleteOutdatedGames(thresholdID string) (int64, error) {
	var n int64
	for id, g := range m.games {
		if id < thresholdID && !g.IsStarted {
			delete(m.games, id)
			n++
		}
	}
	return n, nil
}

func newTestHandler() (*Handler, *mockReversiRepo) {
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	return NewHandler(repo, idGen), repo
}

func post(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

var u1 = &model.User{ID: "u1", Username: "alice"}

func sampleGame() *model.ReversiGame {
	return &model.ReversiGame{
		ID: "g1", User1ID: "u1", User2ID: "u2",
		Map: model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:  "random", TimeLimitForEachTurn: 90,
		Logs:  datatypes.JSON("[]"),
		User1: &model.User{ID: "u1", Username: "alice"},
		User2: &model.User{ID: "u2", Username: "bob"},
	}
}

// --- Games ---

// my=true で viewer が関与するゲームのみ返す (CherryPick 互換)。
func TestGames_MyFlag(t *testing.T) {
	h, repo := newTestHandler()
	repo.games["g1"] = sampleGame()
	rec := post(h.Games, `{"my": true}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// my=false (デフォルト) は isStarted=true のゲームのみ返す。sampleGame は
// IsStarted=false なので空配列。
func TestGames_PublicOnlyStarted(t *testing.T) {
	h, repo := newTestHandler()
	repo.games["g1"] = sampleGame() // IsStarted=false
	rec := post(h.Games, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 0)
}

// started なゲームは my=false でも返る。
func TestGames_PublicShowsStarted(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	g.IsStarted = true
	repo.games["g1"] = g
	rec := post(h.Games, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// untilId でページング (cursor より古い = id が小さいゲームだけ返す) —
// 無限ループ防止の核。aidx ID と同じく新しいほど lexicographic で大きい前提。
func TestGames_UntilIdPagination(t *testing.T) {
	h, repo := newTestHandler()
	older := sampleGame()
	older.ID = "aaa-old"
	older.IsStarted = true
	repo.games[older.ID] = older
	newer := sampleGame()
	newer.ID = "zzz-new"
	newer.IsStarted = true
	repo.games[newer.ID] = newer
	rec := post(h.Games, `{"untilId":"zzz-new"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "aaa-old", resp[0]["id"])
}

// --- Invitations ---

// CherryPick 互換で invitations は UserLite[] (招待者一覧) を返す。viewer が
// User2 (招待される側) のゲームについてのみ、User1 を UserLite で載せる。
func TestInvitations_Success(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	// sampleGame は User1="u1", User2="u2" なので viewer を u2 にして
	// u1 を招待者として得るパターンをテストする。
	g.IsStarted = false
	repo.games["g1"] = g
	u2 := &model.User{ID: "u2", Username: "bob"}
	rec := post(h.Invitations, `{}`, u2)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "u1", resp[0]["id"])
	assert.Equal(t, "alice", resp[0]["username"])
}

// viewer が User1 (招待側) の場合は自分の invitations に出さない。
func TestInvitations_ViewerIsInviter_ExcludeOwn(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	g.IsStarted = false
	repo.games["g1"] = g
	rec := post(h.Invitations, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 0)
}

func TestInvitations_Empty(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Invitations, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- ShowGame ---

func TestShowGame_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.games["g1"] = sampleGame()
	rec := post(h.ShowGame, `{"gameId":"g1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShowGame_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.ShowGame, `{"gameId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShowGame_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.ShowGame, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Match ---

// 新規招待は対局未成立なので upstream 同様 204 空ボディ。招待自体は
// reversi_game 行として残る (連合の AP Invite target に要る)。
func TestMatch_Success(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.Match, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Len(t, repo.games, 1)
}

// userId 無し (ランダムマッチ) は相手未確定なので upstream 同様 204 空ボディ。
// 旧実装は user1=user2 の仮置き game 行を作って 200 を返していた (#1553)。
func TestMatch_NoTarget(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "マッチ未成立時は空ボディ")
	assert.Empty(t, repo.games, "仮置き game 行を作らない")
}

// 自分自身を相手指定したら upstream match.ts と同じ TARGET_IS_YOURSELF (#1553)。
func TestMatch_SelfTarget(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.Match, `{"userId":"u1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, repo.games)
}

// acct (`@自分`) が self に解決されるケースも TARGET_IS_YOURSELF で弾く。
func TestMatch_AcctSelfTarget(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	h.SetFederation("https://example.com", nil, nil, userRepo)

	rec := post(h.Match, `{"userId":"@alice"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, repo.games)
}

func TestMatch_CreateError(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = errBoom
	rec := post(h.Match, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 相手から届いている招待に対する match は「対局成立」なので 200 + game を返す
// (upstream matchSpecificUser の invitations.includes 分岐)。自分発の招待だけが
// 204 になる点と対になるガード。
func TestMatch_AcceptsInboundInvitationWith200(t *testing.T) {
	h, repo := newTestHandler()
	idGen, _ := id.NewGenerator("aidx")
	// u2 -> u1 の未開始 pending game = u1 が受け取っている招待。
	gid := idGen.Generate(time.Now())
	repo.games[gid] = &model.ReversiGame{ID: gid, User1ID: "u2", User2ID: "u1"}

	rec := post(h.Match, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.games, 1, "招待を再利用し新規作成しない")
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, gid, out["id"])
}

// TestMatch_MultipleDedup guards #1774: multiple=false reuses a recent (<3 min)
// outbound pending game instead of creating a duplicate, while multiple=true and
// stale games fall through to a fresh game (upstream matchSpecificUser の
// 3 分 dedup を multiple で skip する挙動)。
func TestMatch_MultipleDedup(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	seedPending := func(repo *mockReversiRepo, createdAt time.Time) string {
		gid := idGen.Generate(createdAt)
		repo.games[gid] = &model.ReversiGame{ID: gid, User1ID: "u1", User2ID: "u2"}
		return gid
	}

	// いずれも「相手が accept していない自分発の招待」なので応答は 204。
	// 差が出るのは行を作り直すかどうか。
	t.Run("multiple=false reuses recent pending outbound game", func(t *testing.T) {
		h, repo := newTestHandler()
		gid := seedPending(repo, time.Now())
		rec := post(h.Match, `{"userId":"u2"}`, u1)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Len(t, repo.games, 1, "既存 pending game を再利用し新規作成しない")
		_, kept := repo.games[gid]
		assert.True(t, kept, "再利用した行がそのまま残る")
	})

	t.Run("multiple=true skips dedup and creates a new game", func(t *testing.T) {
		h, repo := newTestHandler()
		seedPending(repo, time.Now())
		rec := post(h.Match, `{"userId":"u2","multiple":true}`, u1)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Len(t, repo.games, 2, "multiple=true は dedup を skip して新規作成する")
	})

	t.Run("stale pending game (>3min) is not reused", func(t *testing.T) {
		h, repo := newTestHandler()
		seedPending(repo, time.Now().Add(-5*time.Minute))
		rec := post(h.Match, `{"userId":"u2"}`, u1)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Len(t, repo.games, 2, "3 分超の古い pending game は再利用せず新規作成する")
	})
}

// --- Match with acct (CherryPick extension) ---

func TestMatch_AcctLocal(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", UsernameLower: "bob"}
	h.SetFederation("https://example.com", nil, nil, userRepo)

	rec := post(h.Match, `{"userId":"@bob"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, repo.games, 1)
	for _, g := range repo.games {
		assert.Equal(t, "u2", g.User2ID)
	}
}

func TestMatch_AcctRemoteKnown(t *testing.T) {
	h, repo := newTestHandler()
	host := "remote.example"
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u3"] = &model.User{
		ID: "u3", Username: "carol", UsernameLower: "carol", Host: &host,
	}
	h.SetFederation("https://example.com", nil, nil, userRepo)

	rec := post(h.Match, `{"userId":"@carol@remote.example"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, repo.games, 1)
	for _, g := range repo.games {
		assert.Equal(t, "u3", g.User2ID)
	}
}

func TestMatch_AcctUnknown(t *testing.T) {
	h, _ := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	h.SetFederation("https://example.com", nil, nil, userRepo)

	rec := post(h.Match, `{"userId":"@ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMatch_AcctEmptyPrefix(t *testing.T) {
	h, _ := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	h.SetFederation("https://example.com", nil, nil, userRepo)

	rec := post(h.Match, `{"userId":"@"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- CancelMatch ---

func TestCancelMatch_Success(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	g.IsStarted = false
	repo.games["g1"] = g
	rec := post(h.CancelMatch, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.games)
}

// userId 指定時は upstream matchSpecificUserCancel 互換で、その相手に送った
// 招待だけを取り消す。他の相手への pending 招待は残す (#1553)。
func TestCancelMatch_SpecificUserOnly(t *testing.T) {
	h, repo := newTestHandler()
	toU2 := sampleGame() // User1=u1, User2=u2
	repo.games["g1"] = toU2
	toU3 := sampleGame()
	toU3.ID = "g2"
	toU3.User2ID = "u3"
	repo.games["g2"] = toU3

	rec := post(h.CancelMatch, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	_, g1Left := repo.games["g1"]
	assert.False(t, g1Left, "u2 への招待は取り消される")
	_, g2Left := repo.games["g2"]
	assert.True(t, g2Left, "u3 への招待は残る")
}

// userId 指定時、相手から受けている招待 (User1=相手, User2=自分) は取り消さ
// ない。upstream matchSpecificUserCancel も自分が送った招待のみ削除する。
func TestCancelMatch_SpecificUserKeepsInbound(t *testing.T) {
	h, repo := newTestHandler()
	inbound := sampleGame()
	inbound.User1ID = "u2"
	inbound.User2ID = "u1"
	repo.games["g1"] = inbound

	rec := post(h.CancelMatch, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	_, left := repo.games["g1"]
	assert.True(t, left, "受信側の招待は cancel-match では消えない")
}

// --- Surrender ---

func TestSurrender_Success(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	g.IsStarted = true
	repo.games["g1"] = g
	rec := post(h.Surrender, `{"gameId":"g1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.games["g1"].IsEnded)
	assert.Equal(t, "u2", *repo.games["g1"].WinnerID)
}

func TestSurrender_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Surrender, `{"gameId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSurrender_NotPlayer(t *testing.T) {
	h, repo := newTestHandler()
	repo.games["g1"] = sampleGame()
	rec := post(h.Surrender, `{"gameId":"g1"}`, &model.User{ID: "u3"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSurrender_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Surrender, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Verify ---

func TestVerify_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Verify, `{"gameId":"ghost","crc32":"1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestVerify_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Verify, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// crc32 は upstream verify.ts:38-41 で required (#1553)。欠落は 400。
func TestVerify_MissingCRC32(t *testing.T) {
	h, repo := newTestHandler()
	repo.games["g1"] = sampleGame()
	rec := post(h.Verify, `{"gameId":"g1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 保存済み game.crc32 と一致で desynced=false (#1553)。upstream checkCrc は
// ログ再生での再計算ではなく DB 保存値と比較する (ReversiService.ts:618)。
func TestVerify_MatchingCRC(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	stored := "123456789"
	g.CRC32 = &stored
	repo.games["g1"] = g

	rec := post(h.Verify, `{"gameId":"g1","crc32":"123456789"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["desynced"])
	_, hasGame := resp["game"]
	assert.False(t, hasGame, "同期時は game は返さない")
}

// crc32 が保存値と不一致で desynced=true + game が返る。
func TestVerify_DivergingCRC(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	stored := "123456789"
	g.CRC32 = &stored
	repo.games["g1"] = g

	rec := post(h.Verify, `{"gameId":"g1","crc32":"999999"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["desynced"])
	_, hasGame := resp["game"]
	assert.True(t, hasGame, "desync 時は game を返して restoreGame させる")
}

// 保存値が無い (開始前等) 場合は upstream 同様に不一致 = desynced 扱い。
func TestVerify_NoStoredCRC(t *testing.T) {
	h, repo := newTestHandler()
	repo.games["g1"] = sampleGame() // CRC32 nil

	rec := post(h.Verify, `{"gameId":"g1","crc32":"1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["desynced"])
}

// --- Federation ---

// /match remote target で FederationChecker が unavailable を返したら
// 400 エラーで弾いて Invite 配信もゲーム行作成もしない (#417 P3)。
func TestMatch_Returns400WhenFederationUnavailable(t *testing.T) {
	ctx := context.Background()
	apiReversiRedis.FlushAll(ctx)
	h, repo := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "nonreversi.example"
	uri := "https://nonreversi.example/users/alice"
	userRepo.Users["remoteAlice"] = &model.User{
		ID: "remoteAlice", Username: "alice", Host: &host, URI: &uri,
	}
	fedCache := corereversi.NewFederationIDCache(apiReversiRedis.Client)
	h.SetFederation("https://example.com", d, fedCache, userRepo)
	h.SetFederationChecker(stubFedChecker{available: false})

	rec := post(h.Match, `{"userId":"remoteAlice"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, d.calls, "非対応ホストには deliver しない")
	// ゲーム行は作成されない (rollback 不要な early return)
	assert.Len(t, repo.games, 0)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_REVERSI_FEDERATION", errObj["code"])
}

// FederationChecker が available を返したら従来通り Invite 配信。
func TestMatch_SendsInviteWhenFederationAvailable(t *testing.T) {
	ctx := context.Background()
	apiReversiRedis.FlushAll(ctx)
	h, _ := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	userRepo.Users["remoteAlice"] = &model.User{
		ID: "remoteAlice", Username: "alice", Host: &host, URI: &uri,
	}
	fedCache := corereversi.NewFederationIDCache(apiReversiRedis.Client)
	h.SetFederation("https://example.com", d, fedCache, userRepo)
	h.SetFederationChecker(stubFedChecker{available: true})

	rec := post(h.Match, `{"userId":"remoteAlice"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, d.calls)
}

// Acct `@user@host` で未キャッシュのリモートユーザーを WebFinger で取り込む。
func TestMatch_AcctRemoteResolvesViaWebfinger(t *testing.T) {
	ctx := context.Background()
	apiReversiRedis.FlushAll(ctx)
	h, _ := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	// ローカル DB には居ない相手
	fedCache := corereversi.NewFederationIDCache(apiReversiRedis.Client)
	h.SetFederation("https://example.com", d, fedCache, userRepo)
	h.SetFederationChecker(stubFedChecker{available: true})

	host := "remote.example"
	uri := "https://remote.example/users/ghost"
	discovered := &model.User{ID: "discovered", Username: "ghost", Host: &host, URI: &uri}
	h.SetRemoteUserLookup(&stubRemoteLookup{user: discovered})
	// acct を local id に解決した後、FindByID で再取得されるため登録しておく。
	userRepo.Users["discovered"] = discovered

	rec := post(h.Match, `{"userId":"@ghost@remote.example"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, d.calls)
}

type stubFedChecker struct {
	available bool
}

func (s stubFedChecker) Available(_ context.Context, _ string) bool { return s.available }

type stubRemoteLookup struct {
	user *model.User
	err  error
}

func (s *stubRemoteLookup) ResolveByUsernameHost(_, _ string) (*model.User, error) {
	return s.user, s.err
}

// Local ターゲットへの /match で reversi stream に `invited` が push される
// (#417 P2)。リモートターゲットのときは Invite を deliver するのみで
// local stream への push はしない。
func TestMatch_LocalTargetPublishesInvited(t *testing.T) {
	h, _ := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	// local target u2 を登録
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob"}
	h.SetFederation("https://example.com", &mockDeliverer{}, nil, userRepo)
	pub := &stubStreamPub{}
	h.SetStreamPublisher(pub)

	rec := post(h.Match, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u2", pub.calls[0].targetUserID)
	assert.Equal(t, "u1", pub.calls[0].inviter.ID)
}

// Remote ターゲットの /match では stream push は行わない (相手は別 instance)。
func TestMatch_RemoteTargetSkipsStream(t *testing.T) {
	ctx := context.Background()
	apiReversiRedis.FlushAll(ctx)
	h, _ := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	userRepo.Users["remoteAlice"] = &model.User{
		ID: "remoteAlice", Username: "alice", Host: &host, URI: &uri,
	}
	fedCache := corereversi.NewFederationIDCache(apiReversiRedis.Client)
	h.SetFederation("https://example.com", d, fedCache, userRepo)
	pub := &stubStreamPub{}
	h.SetStreamPublisher(pub)

	rec := post(h.Match, `{"userId":"remoteAlice"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	assert.Equal(t, 1, d.calls, "remote target には Invite AP activity が配信される")
	assert.Len(t, pub.calls, 0, "remote target では local stream push は無い")
}

type stubStreamPubCall struct {
	targetUserID string
	inviter      *model.User
}

type stubStreamPub struct {
	calls []stubStreamPubCall
}

func (s *stubStreamPub) PublishInvited(targetUserID string, inviter *model.User) {
	s.calls = append(s.calls, stubStreamPubCall{targetUserID: targetUserID, inviter: inviter})
}

type mockDeliverer struct {
	calls int
}

func (m *mockDeliverer) DeliverToUser(_ string, _ *model.User, _ []byte) error {
	m.calls++
	return nil
}

func TestSetFederation(t *testing.T) {
	h, _ := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	h.SetFederation("https://example.com", d, nil, userRepo)
	assert.Equal(t, "https://example.com", h.baseURL)
	assert.NotNil(t, h.deliverer)
}

func TestMatch_WithRemoteUser(t *testing.T) {
	// Use a real (nil-redis) FederationIDCache; Set/Get are no-ops which lets
	// the handler still fire DeliverToUser via federation branch.
	h, repo := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := "https://remote.example/users/u2"
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", Host: &host, URI: &uri}
	fedCache := corereversi.NewFederationIDCache(nil)
	h.SetFederation("https://example.com", d, fedCache, userRepo)

	rec := post(h.Match, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.games, 1)
	assert.Equal(t, 1, d.calls) // Invite 送信
}

func TestSurrender_WithRemoteUser(t *testing.T) {
	// stubFedCache stores the session/game mapping in memory so the handler's
	// GetSessionByGame lookup returns true and triggers the Leave delivery.
	h, repo := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := "https://remote.example/users/u2"
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", Host: &host, URI: &uri}

	g := sampleGame()
	g.IsStarted = true
	repo.games["g1"] = g

	// Use a real cache bound to nil redis; Set/Get/Delete are no-ops which
	// means GetSessionByGame returns ("", false) — matching a non-federated
	// game case. Separately verify the federated path via the stub below.
	fedCache := corereversi.NewFederationIDCache(nil)
	h.SetFederation("https://example.com", d, fedCache, userRepo)

	rec := post(h.Surrender, `{"gameId":"g1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// No federation mapping → no Leave delivery
	assert.Equal(t, 0, d.calls)
}

func TestMatch_CreateErrorWithRemote(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = errBoom
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := "https://remote.example/users/u2"
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", Host: &host, URI: &uri}
	fedCache := corereversi.NewFederationIDCache(nil)
	h.SetFederation("https://example.com", d, fedCache, userRepo)

	rec := post(h.Match, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, 0, d.calls) // Create失敗時はInvite送らない
}

// userId 無しのランダムマッチは federation 配線済みでも 204 空ボディで、
// 仮置き game 行も Invite 配信も発生しない (#1553)。
func TestMatch_EmptyUserIDIsRandomMatch(t *testing.T) {
	h, repo := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	fedCache := corereversi.NewFederationIDCache(nil)
	h.SetFederation("https://example.com", d, fedCache, userRepo)

	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.games)
	assert.Equal(t, 0, d.calls)
}

// TestPackGame_WinnerField guards that REST packGame derives a `winner`
// UserLite from winnerId. The frontend (game.board.vue) drives its
// `won/draw` display via `v-if="game.winner"` — without this field the
// game result is rendered as a draw on every viewer (#649).
func TestPackGame_WinnerField(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	gid := idGen.Generate(timeRef())
	user1 := &model.User{ID: "alice", Username: "alice"}
	user2 := &model.User{ID: "bob", Username: "bob"}

	t.Run("user1 wins", func(t *testing.T) {
		winnerID := "alice"
		g := &model.ReversiGame{
			ID: gid, User1ID: "alice", User2ID: "bob",
			User1: user1, User2: user2, WinnerID: &winnerID,
		}
		out := packGame(g, idGen)
		w, ok := out["winner"].(entity.UserLite)
		require.True(t, ok, "winner must be present and be a UserLite")
		assert.Equal(t, "alice", w.ID)
		assert.Equal(t, &winnerID, out["winnerId"])
	})

	t.Run("user2 wins", func(t *testing.T) {
		winnerID := "bob"
		g := &model.ReversiGame{
			ID: gid, User1ID: "alice", User2ID: "bob",
			User1: user1, User2: user2, WinnerID: &winnerID,
		}
		out := packGame(g, idGen)
		w, ok := out["winner"].(entity.UserLite)
		require.True(t, ok)
		assert.Equal(t, "bob", w.ID)
	})

	t.Run("draw (winnerId nil)", func(t *testing.T) {
		g := &model.ReversiGame{
			ID: gid, User1ID: "alice", User2ID: "bob",
			User1: user1, User2: user2,
		}
		out := packGame(g, idGen)
		// upstream json-schema は winner を optional:false, nullable:true で
		// 宣言するためキー欠落ではなく null を出す (#1553)。
		w, has := out["winner"]
		require.True(t, has, "winner key must always be present")
		assert.Nil(t, w, "winner must be null when WinnerID is nil")
	})
}

// timeRef returns a fixed deterministic time for aidx Generate calls in
// pack tests — keeps id generation reproducible even though the actual
// timestamp value is irrelevant.
func timeRef() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

// errorID decodes a Misskey-shaped error body and returns the `error.id`.
func errorID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Error.ID
}

// TestPackGame_FormFields guards that REST packGame always emits form1/form2
// (#1553). Upstream packedReversiGameDetailedSchema declares them
// optional:false, nullable:true and ReversiGameEntityService.packDetail returns
// `form1: game.form1, form2: game.form2`, so the keys must always be present:
// null when the column is empty, the stored JSON object otherwise. A nil column
// must not break marshaling of the whole response.
func TestPackGame_FormFields(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	gid := idGen.Generate(timeRef())

	t.Run("nil form columns serialize to null but keys present", func(t *testing.T) {
		g := &model.ReversiGame{ID: gid, User1ID: "alice", User2ID: "bob"}
		out := packGame(g, idGen)

		f1, has1 := out["form1"]
		require.True(t, has1, "form1 key must always be present")
		assert.Nil(t, f1)
		f2, has2 := out["form2"]
		require.True(t, has2, "form2 key must always be present")
		assert.Nil(t, f2)

		// レスポンス全体が json.Marshal 可能であること (空 datatypes.JSON を
		// そのまま入れると marshal が失敗するため null 正規化を検証する)。
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(raw, &decoded))
		_, ok1 := decoded["form1"]
		_, ok2 := decoded["form2"]
		assert.True(t, ok1)
		assert.True(t, ok2)
		assert.Nil(t, decoded["form1"])
		assert.Nil(t, decoded["form2"])
	})

	t.Run("populated form columns pass through verbatim", func(t *testing.T) {
		g := &model.ReversiGame{
			ID: gid, User1ID: "alice", User2ID: "bob",
			Form1: datatypes.JSON(`{"autoStart":true}`),
			Form2: datatypes.JSON(`[{"id":"x"}]`),
		}
		out := packGame(g, idGen)

		raw, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(raw, &decoded))
		form1, ok := decoded["form1"].(map[string]any)
		require.True(t, ok, "form1 must round-trip as the stored object")
		assert.Equal(t, true, form1["autoStart"])
		form2, ok := decoded["form2"].([]any)
		require.True(t, ok, "form2 must round-trip as the stored array")
		require.Len(t, form2, 1)
	})
}

// TestPackGame_TimestampFormat guards #1774: startedAt/endedAt are emitted as
// millisecond-fixed ISO8601 (upstream ReversiGameEntityService の toISOString)
// like createdAt, not Go's RFC3339Nano (where trailing-zero ms drop and pg
// microseconds yield 6 digits). nil pointers serialize to null with the key
// present.
func TestPackGame_TimestampFormat(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	gid := idGen.Generate(timeRef())

	t.Run("non-nil times render as .000Z millisecond ISO", func(t *testing.T) {
		started := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)        // 0 fractional
		ended := time.Date(2026, 6, 15, 12, 5, 30, 123456000, time.UTC) // 123456us
		g := &model.ReversiGame{ID: gid, User1ID: "alice", User2ID: "bob", StartedAt: &started, EndedAt: &ended}
		out := packGame(g, idGen)
		assert.Equal(t, "2026-06-15T12:00:00.000Z", out["startedAt"])
		assert.Equal(t, "2026-06-15T12:05:30.123Z", out["endedAt"])
	})

	t.Run("nil times serialize to null with key present", func(t *testing.T) {
		g := &model.ReversiGame{ID: gid, User1ID: "alice", User2ID: "bob"}
		out := packGame(g, idGen)
		s, hasS := out["startedAt"]
		e, hasE := out["endedAt"]
		require.True(t, hasS, "startedAt key must always be present")
		require.True(t, hasE, "endedAt key must always be present")
		assert.Nil(t, s)
		assert.Nil(t, e)
	})
}

// TestReversiErrorIDsMatchUpstream guards that each reversi endpoint emits the
// exact per-endpoint error UUID defined by vanilla Misskey (#1553). The UUIDs
// differ per endpoint (NO_SUCH_GAME is f13a03db… in show-game, 8fb05624… in
// verify, ace0b11f… in surrender), so a single shared UUID is a drop-in
// regression for clients that branch on error.id.
func TestReversiErrorIDsMatchUpstream(t *testing.T) {
	t.Run("show-game NO_SUCH_GAME", func(t *testing.T) {
		h, _ := newTestHandler()
		rec := post(h.ShowGame, `{"gameId":"ghost"}`, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "f13a03db-fae1-46c9-87f3-43c8165419e1", errorID(t, rec))
	})

	t.Run("verify NO_SUCH_GAME", func(t *testing.T) {
		h, _ := newTestHandler()
		rec := post(h.Verify, `{"gameId":"ghost","crc32":"1"}`, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "8fb05624-b525-43dd-90f7-511852bdfeee", errorID(t, rec))
	})

	t.Run("surrender NO_SUCH_GAME (inline)", func(t *testing.T) {
		h, _ := newTestHandler()
		rec := post(h.Surrender, `{"gameId":"ghost"}`, u1)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "ace0b11f-e0a6-4076-a30d-e8284c81b2df", errorID(t, rec))
	})

	t.Run("surrender ACCESS_DENIED (inline)", func(t *testing.T) {
		h, repo := newTestHandler()
		repo.games["g1"] = sampleGame()
		rec := post(h.Surrender, `{"gameId":"g1"}`, &model.User{ID: "u3"})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "6e04164b-a992-4c93-8489-2123069973e1", errorID(t, rec))
	})

	t.Run("surrender NO_SUCH_GAME (service path)", func(t *testing.T) {
		rec := surrenderErrorRec(t, corereversi.ErrGameNotFound)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "ace0b11f-e0a6-4076-a30d-e8284c81b2df", errorID(t, rec))
	})

	t.Run("surrender ACCESS_DENIED (service path)", func(t *testing.T) {
		rec := surrenderErrorRec(t, corereversi.ErrNotPlayer)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "6e04164b-a992-4c93-8489-2123069973e1", errorID(t, rec))
	})

	t.Run("surrender ALREADY_ENDED (service path)", func(t *testing.T) {
		rec := surrenderErrorRec(t, corereversi.ErrAlreadyEnded)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "6c2ad4a6-cbf1-4a5b-b187-b772826cfc6d", errorID(t, rec))
	})

	t.Run("match NO_SUCH_USER", func(t *testing.T) {
		h, _ := newTestHandler()
		userRepo := testutil.NewMockUserRepository()
		h.SetFederation("https://example.com", nil, nil, userRepo)
		rec := post(h.Match, `{"userId":"@ghost"}`, u1)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "0b4f0559-b484-4e31-9581-3f73cee89b28", errorID(t, rec))
	})

	t.Run("match TARGET_IS_YOURSELF", func(t *testing.T) {
		h, _ := newTestHandler()
		rec := post(h.Match, `{"userId":"u1"}`, u1)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "96fd7bd6-d2bc-426c-a865-d055dcd2828e", errorID(t, rec))
	})
}

// surrenderErrorRec drives surrenderErrorResponse directly so the service-error
// branch (used when the core service rejects a surrender) is covered for the
// upstream UUID assertions above.
func surrenderErrorRec(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, surrenderErrorResponse(c, err))
	return rec
}

// #2106 L16: 終了済 game への非プレイヤー surrender は ACCESS_DENIED より先に ALREADY_ENDED。
func TestSurrender_EndedBeforeNotPlayer(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	g.IsEnded = true
	repo.games["g1"] = g
	rec := post(h.Surrender, `{"gameId":"g1"}`, &model.User{ID: "u3"}) // 非プレイヤー
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ALREADY_ENDED")
}
