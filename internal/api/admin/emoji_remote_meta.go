package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/emojimeta"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// RemoteEmojiMetaFetcher retrieves emoji metadata from the origin server.
// handler 単体テストで fake を差し込めるよう interface で受ける
// (`EmojiImageFetcher` と同じ形)。
type RemoteEmojiMetaFetcher interface {
	Fetch(ctx context.Context, host, name, softwareName string) (*emojimeta.Meta, error)
}

// SetRemoteEmojiMetaFetcher attaches the fetcher used by
// admin/emoji/fetch-remote-meta. nil のままだと endpoint は「取得できなかった」を
// 返す (未配線環境で 5xx にしない)。
func (h *Handler) SetRemoteEmojiMetaFetcher(f RemoteEmojiMetaFetcher) {
	h.remoteEmojiMetaFetcher = f
}

// EmojiFetchRemoteMeta handles POST /api/admin/emoji/fetch-remote-meta.
//
// **mk-go 独自 endpoint** (#2698)。リモート絵文字をインポートするとき、AP では
// 運ばれないカテゴリ・エイリアス・センシティブを相手の REST API から取ってくる。
// 本番の実測では、リモート絵文字 19,129 件のうちそれらは **1 件も** 連合で
// 入っていない。
//
// **作成はしない。** ここは取得だけで、確認・編集を経てから `admin/emoji/copy` が
// 作る。作成まで一気にやると「値を埋めた状態で確認してから取り込む」ができない。
//
// **失敗を 5xx にしない。** 相手が落ちている / per-name endpoint を持たない、は
// どちらも正常な結果なので、`fetched: false` を返して手入力に倒す。
func (h *Handler) EmojiFetchRemoteMeta(c echo.Context) error {
	// **`emojiId` と `name`+`host` の両方で引ける。** 投稿本文やリアクションの
	// 右クリックから呼ぶとき、frontend が持っているのは `name@host` だけで
	// emoji の id を知らない。id を引くためだけに `admin/emoji/list` を叩かせると
	// 往復が 1 つ増える。
	var req struct {
		EmojiID string `json:"emojiId"`
		Name    string `json:"name"`
		Host    string `json:"host"`
	}
	if err := c.Bind(&req); err != nil || (req.EmojiID == "" && (req.Name == "" || req.Host == "")) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("emojiId or name+host is required."))
	}
	if h.emojiRepo == nil {
		// ここだけは src を引けないので id も返せない (repo 未配線はテスト専用の状態)。
		return c.JSON(http.StatusOK, map[string]any{"fetched": false, "reason": "error"})
	}

	var (
		src *model.Emoji
		err error
	)
	if req.EmojiID != "" {
		src, err = h.emojiRepo.FindByID(req.EmojiID)
	} else {
		src, err = h.emojiRepo.FindByNameAndHost(req.Name, &req.Host)
	}
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "e2785b66-dca3-4087-9cac-b93c541cc425"))
	}
	// ローカル絵文字には取ってくる相手がいない。
	if src.Host == nil || *src.Host == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_REMOTE_EMOJI", "The emoji is not a remote one.", "3d81ad0e-7d1d-4c4a-9b9b-3f1d3d0f6a2e"))
	}
	if h.remoteEmojiMetaFetcher == nil {
		// **id 等は返す。** frontend は id が無いとモーダルを描けず、取り込みも
		// できない。`ferr` 側と同じ形に揃える。
		return c.JSON(http.StatusOK, withStoredLicense(map[string]any{
			"fetched": false, "reason": "error",
			"emojiId": src.ID, "name": src.Name, "host": *src.Host, "originalUrl": src.OriginalURL,
		}, src))
	}

	// software 名は nodeinfo 由来。**未知なら叩かない** — per-name endpoint を
	// 持たない相手 (Mastodon 系) に投げても 404 が返るだけで、こちらの待ち時間と
	// 相手の負荷が無駄になる。
	var software string
	if h.instanceRepo != nil {
		inst, ierr := h.instanceRepo.FindByHost(*src.Host)
		if ierr != nil && !repository.IsNotFound(ierr) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if ierr == nil && inst != nil && inst.SoftwareName != nil {
			software = *inst.SoftwareName
		}
	}

	meta, ferr := h.remoteEmojiMetaFetcher.Fetch(c.Request().Context(), *src.Host, src.Name, software)
	if ferr != nil {
		// 取得できないのは異常ではない。理由を返して UI が説明できるようにする。
		reason := "error"
		switch {
		case errors.Is(ferr, emojimeta.ErrUnsupported):
			reason = "unsupported"
		case errors.Is(ferr, emojimeta.ErrNotFound):
			reason = "notFound"
		default:
			slog.WarnContext(c.Request().Context(), "emoji fetch-remote-meta failed",
				"host", *src.Host, "name", src.Name, "software", software, "err", ferr)
		}
		return c.JSON(http.StatusOK, withStoredLicense(map[string]any{
			"fetched": false, "reason": reason,
			"emojiId": src.ID, "name": src.Name, "host": *src.Host, "originalUrl": src.OriginalURL,
		}, src))
	}

	// **取れなかった項目はキーごと出さない。** 空文字を返すと、frontend が
	// 「相手が空を返した」と解釈して既存値を消してしまう。
	// **モーダルが要る情報も返す。** name+host で呼ばれたとき、frontend は
	// `admin/emoji/copy` に渡す id を知らないので、ここで返しておく。
	out := map[string]any{
		"fetched":     true,
		"emojiId":     src.ID,
		"name":        src.Name,
		"host":        *src.Host,
		"originalUrl": src.OriginalURL,
	}
	if meta.Category != nil {
		out["category"] = *meta.Category
	}
	if len(meta.Aliases) > 0 {
		out["aliases"] = meta.Aliases
	}
	if meta.License != nil {
		out["license"] = *meta.License
	} else {
		withStoredLicense(out, src)
	}
	if meta.IsSensitive != nil {
		out["isSensitive"] = *meta.IsSensitive
	}
	return c.JSON(http.StatusOK, out)
}

// withStoredLicense fills `license` from the stored remote emoji when the
// response does not carry one from the origin's REST API (#3246).
//
// ライセンスは連合 (`_misskey_license`) で既に入っている唯一の項目。frontend は
// 応答の `license` でダイアログを初期化し、編集していなくても `admin/emoji/copy` へ
// 上書きとして送るので、ここで返さないと相手の API から取れなかったとき (失敗・
// 非対応・相手が持っていない) に元のライセンスが空で上書きされる。空の値は
// 「取れなかった項目のキーは出さない」規則に合わせて出さない。
func withStoredLicense(out map[string]any, src *model.Emoji) map[string]any {
	if src.License != nil && *src.License != "" {
		out["license"] = *src.License
	}
	return out
}
