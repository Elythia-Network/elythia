package bubblegame

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Handler handles bubble-game/* endpoints.
type Handler struct {
	repo  repository.BubbleGameRepository
	idGen id.Generator
}

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
		Score       int     `json:"score"`
		Seed        string  `json:"seed"`
		Logs        [][]any `json:"logs"`
		GameMode    string  `json:"gameMode"`
		GameVersion int     `json:"gameVersion"`
	}
	if err := c.Bind(&req); err != nil || req.Seed == "" || req.GameMode == "" {
		return apierr.JSONInvalidParam(c)
	}
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
	if !colfit.Fits(req.GameMode, bubbleGameModeMaxRunes) {
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
		GameVersion: req.GameVersion,
		GameMode:    req.GameMode,
		Score:       req.Score,
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
// `query` タグでしか拾わないため、fetch-rss と同様に QueryParam を先に読み、
// 無ければ JSON body に fallback する (#1774)。
func (h *Handler) Ranking(c echo.Context) error {
	gameMode := c.QueryParam("gameMode")
	if gameMode == "" {
		var req struct {
			GameMode string `json:"gameMode"`
		}
		// Bind 失敗は無視 (空 gameMode のまま下の guard で 400 を返す)。
		_ = c.Bind(&req)
		gameMode = req.GameMode
	}
	if gameMode == "" {
		return apierr.JSONInvalidParam(c)
	}

	records, err := h.repo.Ranking(gameMode, 10)
	if err != nil {
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
			entry["user"] = entity.PackUserLite(r.User)
		}
		result[i] = entry
	}
	// upstream ranking.ts は cacheSec:60 → 未認証 GET に Cache-Control: public,
	// max-age=60 を付ける (ApiCallService。POST/認証済みには付けない、#2027)。
	// token は raw token (HasRawToken) で見るので無効 token GET でも cache を付けない (#2049)。
	if c.Request().Method == http.MethodGet && middleware.GetUser(c) == nil && !middleware.HasRawToken(c) {
		c.Response().Header().Set("Cache-Control", "public, max-age=60")
	}
	return c.JSON(http.StatusOK, result)
}
