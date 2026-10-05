package emojis

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// Handler handles emoji-related API endpoints.
type Handler struct {
	emojiRepo repository.EmojiRepository
	// exportQueue は export-custom-emojis 用 (#2791 で router.go の inline
	// closure から移設)。
	exportQueue ExportEnqueuer
}

// NewHandler creates a new emojis Handler.
func NewHandler(emojiRepo repository.EmojiRepository) *Handler {
	return &Handler{emojiRepo: emojiRepo}
}

// Emojis returns local custom emojis.
// POST /api/emojis
func (h *Handler) Emojis(c echo.Context) error {
	emojis, err := h.emojiRepo.ListLocal()
	if err != nil {
		// エラー時は空配列で返却 (best-effort)
		return c.JSON(http.StatusOK, map[string]any{"emojis": []any{}})
	}

	result := make([]map[string]any, 0, len(emojis))
	for _, e := range emojis {
		// url は upstream packSimple と同じく publicUrl || originalUrl で、
		// publicUrl 空 (remote emoji 等) のとき originalUrl に fallback する
		// (#1556)。EmojiSimple.url は required non-null。
		url := e.PublicURL
		if url == "" {
			url = e.OriginalURL
		}
		item := map[string]any{
			"name":     e.Name,
			"category": e.Category,
			"aliases":  e.Aliases,
			"url":      url,
		}
		// upstream packSimple は localOnly / isSensitive を `? true : undefined`
		// で出すため、false のとき key 自体を省く (json-schema 上 optional)。
		// 常に false を載せると shape が upstream とずれる (#1781)。
		if e.LocalOnly {
			item["localOnly"] = true
		}
		if e.IsSensitive {
			item["isSensitive"] = true
		}
		// roleIdsThatCanBeUsedThisEmojiAsReaction は length>0 のときだけ emit
		// (upstream packSimple、#1556)。reaction-role gating UI が使う。
		if len(e.RoleIDsThatCanBeUsedThisEmojiAsReaction) > 0 {
			item["roleIdsThatCanBeUsedThisEmojiAsReaction"] = []string(e.RoleIDsThatCanBeUsedThisEmojiAsReaction)
		}
		result = append(result, item)
	}

	return c.JSON(http.StatusOK, map[string]any{"emojis": result})
}

// Emoji returns a single custom emoji by name.
// POST /api/emoji
func (h *Handler) Emoji(c echo.Context) error {
	var req struct {
		Name string `json:"name" query:"name"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	e, err := h.emojiRepo.FindByNameAndHost(req.Name, nil)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "14141e4b-dea8-41f0-9ba1-1721a6b5b92c"))
	}
	// upstream は EmojiDetailed を返す。ad-hoc map では license /
	// roleIdsThatCanBeUsedThisEmojiAsReaction が欠落し、url も publicUrl 固定で
	// originalUrl fallback が無かった。共通 packer に揃える。
	return c.JSON(http.StatusOK, entity.PackEmojiDetailed(e))
}
