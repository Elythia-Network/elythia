package bubblegame

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Handler handles bubble-game/* endpoints.
type Handler struct {
	repo  repository.BubbleGameRepository
	idGen id.Generator
	// entityPacker は利用者を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3794)。組み立てで渡す。
	entityPacker *entity.Packer
}

// SetEntityPacker wires the Packer this Handler packs entities with (#3794).
func (h *Handler) SetEntityPacker(p *entity.Packer) { h.entityPacker = p }

// HasEntityPacker reports whether SetEntityPacker received a Packer that
// rewrites media URLs. 無いと avatarUrl などを生のまま返し、閲覧者の IP が
// リモートへ漏れる。
func (h *Handler) HasEntityPacker() bool { return h.entityPacker.HasMediaURL() }

// NewHandler creates a new bubble-game handler.
func NewHandler(repo repository.BubbleGameRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// seedMaxAge is how old a seed may be when its score is registered (mk-go,
// #3192). upstream は 5 時間。frontend の途中保存の期限 (drop-and-fusion-save.ts の
// SAVE_MAX_AGE_MS) と揃えること。
const seedMaxAge = 7 * 24 * time.Hour

// bubbleGameModeMaxRunes は `bubble_game_record.gameMode` の列幅。
const bubbleGameModeMaxRunes = 128

// Register handles POST /api/bubble-game/register.
func (h *Handler) Register(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Score       jsnum.Int `json:"score"`
		Seed        string    `json:"seed"`
		Logs        [][]any   `json:"logs"`
		GameMode    *string   `json:"gameMode"`
		GameVersion jsnum.Int `json:"gameVersion"`
	}
	// 本家の paramDef は gameMode に minLength を持たないので、空文字も登録する (#3768)。
	// 欠けは本家の paramDef が先に 400 にする。
	if err := c.Bind(&req); err != nil || req.Seed == "" || req.GameMode == nil {
		return apierr.JSONInvalidParam(c)
	}
	gameMode := *req.GameMode
	// upstream register.ts paramDef は score:{minimum:0}。負値を ajv 同様 400 で弾く (#2027)。
	if req.Score < 0 {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/properties/score/minimum", "must be >= 0"))
	}

	// シード検証: seedはUnixタイムスタンプ文字列
	seedMs, err := strconv.ParseInt(req.Seed, 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_SEED", "Provided seed is invalid.", "eb627bc7-574b-4a52-a860-3c3eae772b88"))
	}
	seedDate := time.UnixMilli(seedMs)
	now := time.Now()

	// 未来のシードは不正
	if seedDate.After(now) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_SEED", "Provided seed is invalid.", "eb627bc7-574b-4a52-a860-3c3eae772b88"))
	}
	// 古すぎるシードは不正。**upstream は 5 時間**だが、mk-go は途中から再開できる
	// (#3192) ので延ばす。再開したゲームは元のシードを使い続けるので、5 時間だと
	// 夜に中断して翌朝終えたゲームが登録できず、しかも frontend は登録の失敗を
	// 表示しない。frontend はこれより古い保存を再開させない。
	if seedDate.Before(now.Add(-seedMaxAge)) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_SEED", "Provided seed is invalid.", "eb627bc7-574b-4a52-a860-3c3eae772b88"))
	}

	// **gameMode は varchar(128)、logs は jsonb (#3037)。** PostgreSQL は
	// 列に入らない値を SQLSTATE 22021 / 22P05 でクエリごと落とすので、引く前に
	// 弾かないと**任意の認証ユーザーが 500 を起こせる**。`seed` は上で
	// `ParseInt` を通っているので数字だけ (列幅 1024 に収まる)。
	if !colfit.Fits(gameMode, bubbleGameModeMaxRunes) {
		return apierr.JSONInvalidParam(c)
	}
	logsJSON, _ := json.Marshal(req.Logs)
	if !colfit.JSONStorable(logsJSON) {
		return apierr.JSONInvalidParam(c)
	}
	record := &model.BubbleGameRecord{
		ID:          h.idGen.Generate(now),
		UserID:      user.ID,
		SeededAt:    seedDate,
		Seed:        req.Seed,
		GameVersion: int(req.GameVersion),
		GameMode:    gameMode,
		Score:       int(req.Score),
		Logs:        logsJSON,
		IsVerified:  false,
	}
	if err := h.repo.Create(record); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.NoContent(http.StatusNoContent)
}

// Ranking handles GET/POST /api/bubble-game/ranking. upstream ranking.ts は
// allowGet:true で、frontend drop-and-fusion.vue は misskeyApiGet (GET +
// query string) で gameMode を投げる。Echo の Bind は GET だと query を
// `query` タグでしか拾わないため、GET では QueryParam で読む (#1774)。
func (h *Handler) Ranking(c echo.Context) error {
	// 本家は GET なら query、POST なら body だけを読む。POST の query を読むと、
	// 本家の paramDef が見た body とは別の gameMode で引くことになる (#3768)。
	// 空文字は本家と同じく、その名前のモードとして引く。
	var gameMode string
	if c.Request().Method == http.MethodGet {
		if !c.QueryParams().Has("gameMode") {
			return apierr.JSONInvalidParam(c)
		}
		gameMode = c.QueryParam("gameMode")
	} else {
		var req struct {
			GameMode *string `json:"gameMode"`
		}
		if err := c.Bind(&req); err != nil || req.GameMode == nil {
			return apierr.JSONInvalidParam(c)
		}
		gameMode = *req.GameMode
	}

	records, err := h.repo.Ranking(gameMode, 10)
	if err != nil {
		// 本家はエラーになりキャッシュされない。空の一覧を public にしない (#3593)。
		middleware.SkipPublicCache(c)
		return c.JSON(http.StatusOK, []any{})
	}

	result := make([]map[string]any, len(records))
	for i, r := range records {
		entry := map[string]any{
			"id":    r.ID,
			"score": r.Score,
		}
		// upstream ranking.ts は user を ref:'UserLite' で返す。ad-hoc な
		// 4 フィールドマップだと avatarUrl 等の必須フィールドが欠けて
		// frontend のランキング表示が壊れるため packer を経由する (#1553)。
		if r.User != nil {
			entry["user"] = h.entityPacker.PackUserLite(r.User)
		}
		result[i] = entry
	}
	// upstream ranking.ts は cacheSec:60 → 未認証 GET に Cache-Control: public,
	// max-age=60 を付ける (ApiCallService。POST/認証済みには付けない、#2027)。
	// /api では middleware.APICacheControl が同じ表 (APICacheSec) から同じ値を付けるので
	// 二重になる。この handler 単体のテストが見ているので残す (#3593)。
	// token は raw token (HasRawToken) で見るので無効 token GET でも cache を付けない (#2049)。
	if c.Request().Method == http.MethodGet && middleware.GetUser(c) == nil && !middleware.HasRawToken(c) {
		c.Response().Header().Set("Cache-Control", "public, max-age=60")
	}
	return c.JSON(http.StatusOK, result)
}
