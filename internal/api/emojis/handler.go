package emojis

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Handler handles emoji-related API endpoints.
type Handler struct {
	emojiRepo repository.EmojiRepository
	// exportQueue は export-custom-emojis 用 (#2791 で router.go の inline
	// closure から移設)。
	exportQueue ExportEnqueuer
	// encoded は /api/emojis の符号化済みの応答 (#3593)。
	encoded atomic.Pointer[encodedEmojis]
}

// NewHandler creates a new emojis Handler.
func NewHandler(emojiRepo repository.EmojiRepository) *Handler {
	return &Handler{emojiRepo: emojiRepo}
}

// Emojis returns local custom emojis.
// POST /api/emojis
//
// The response body is encoded (and gzip-compressed) once per emoji list and
// reused until the repository hands out a different list. The server's gzip
// middleware skips this route (selfEncodedRoutes in internal/server), so the
// handler negotiates Content-Encoding itself.
func (h *Handler) Emojis(c echo.Context) error {
	emojis, err := h.emojiRepo.ListLocal()
	if err != nil {
		// エラー時は空配列で返却 (best-effort)。本家はエラーになりキャッシュされない
		// ので、空の一覧を public にしない (#3593)。
		middleware.SkipPublicCache(c)
		return c.JSON(http.StatusOK, map[string]any{"emojis": []any{}})
	}
	body := h.emojisBody(emojis)
	res := c.Response()
	res.Header().Add(echo.HeaderVary, echo.HeaderAcceptEncoding)
	res.Header().Set(echo.HeaderContentType, "application/json; charset=utf-8")
	payload := body.plain
	if body.gzip != nil && middleware.AcceptsEncoding(c.Request().Header.Get(echo.HeaderAcceptEncoding), "gzip") {
		res.Header().Set(echo.HeaderContentEncoding, "gzip")
		payload = body.gzip
	}
	res.Header().Set(echo.HeaderContentLength, strconv.Itoa(len(payload)))
	res.WriteHeader(http.StatusOK)
	_, err = res.Write(payload)
	return err
}

// emojisMinGzipLength mirrors the server's gzip middleware MinLength: smaller
// bodies are sent uncompressed.
const emojisMinGzipLength = 1024

// encodedEmojis is the /api/emojis response for one emoji list.
type encodedEmojis struct {
	// src は元にした一覧。CachedEmojiRepository.ListLocal は無効化 (書き込み・
	// pub/sub・TTL) まで同じ slice を返し、無効化の後は作り直した別の slice を
	// 返す。slice の同一性を見れば、絵文字の cache を捨てる契機とこの cache を
	// 捨てる契機が揃う。src を握っているので、古い配列の番地が別の一覧に
	// 再利用されることはない。
	src   []*model.Emoji
	plain []byte
	// gzip は plain が emojisMinGzipLength 以上のときだけ持つ。
	gzip []byte
}

// sameList reports whether a and b are the same slice (same backing array
// and length). Two empty lists encode identically, so they also match.
func sameList(a, b []*model.Emoji) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0]
}

// emojisBody returns the encoded response for list, reusing the previous
// encoding when list is the same slice.
func (h *Handler) emojisBody(list []*model.Emoji) *encodedEmojis {
	if cur := h.encoded.Load(); cur != nil && sameList(cur.src, list) {
		return cur
	}
	next := encodeEmojis(list)
	// 同時に作り直した要求どうしは、どちらが残っても同じ一覧から作った内容。
	h.encoded.Store(next)
	return next
}

// encodeEmojis builds the response for emojis. It cannot fail: the body holds
// only strings, string slices and booleans, and both encoders write to memory.
func encodeEmojis(emojis []*model.Emoji) *encodedEmojis {
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
	// c.JSON (internal/server の fastJSONSerializer) と同じ json.Encoder で書き、
	// 末尾の改行まで含めて従来と同じ本文にする。
	// 文字列・文字列の slice・真偽値だけなので encode は失敗しない。書き込み先も
	// メモリなので、gzip の書き込みと Close も失敗しない (level は定数)。
	var plain bytes.Buffer
	_ = json.NewEncoder(&plain).Encode(map[string]any{"emojis": result})
	out := &encodedEmojis{src: emojis, plain: plain.Bytes()}
	if plain.Len() >= emojisMinGzipLength {
		var gz bytes.Buffer
		// 一覧が変わるまで使い回すので、1 回だけ最大の圧縮率で縮める。
		zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		_, _ = zw.Write(out.plain)
		_ = zw.Close()
		out.gzip = gz.Bytes()
	}
	return out
}

// Emoji returns a single custom emoji by name.
// POST /api/emoji
func (h *Handler) Emoji(c echo.Context) error {
	var req struct {
		Name *string `json:"name" query:"name"`
	}
	// 空の name は本家の paramDef を通る。本家は findOneOrFail で 500 になるが、
	// 存在しない絵文字と同じ NO_SUCH_EMOJI にする (#3774)。欠けだけを拒否する。
	if err := c.Bind(&req); err != nil || req.Name == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	e, err := h.emojiRepo.FindByNameAndHost(*req.Name, nil)
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
