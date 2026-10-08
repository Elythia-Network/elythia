// Package pluginsecrets serves the control-panel endpoints for server plugins'
// secret values (#3470).
//
// 各プラグインの名前空間の予約パスに生える。
//
//	POST /api/plugin/<name>/_secrets         一覧 (値は返さない)
//	POST /api/plugin/<name>/_secrets/set     保存
//	POST /api/plugin/<name>/_secrets/delete  削除
//
// **権限の確認は配線側 (internal/server) の middleware が行う。** 管理者で、
// かつブラウザでログインした token (users.token) で呼ばれたものだけがここに
// 届く。ここでは確認しない — 二重に書くと、片方を外しても気付けない。
//
// # 値を返さない
//
// どの応答にも値そのものは入れない。「設定済みか」「今の鍵で読めるか」と、
// 十分に長い値の末尾 4 文字だけを返す。管理画面を覗かれただけで漏れる形にしない。
package pluginsecrets

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/plugin"
)

// Error codes. プラグインのルートと同じ `{"error": {"message", "code"}}` の形で返す。
//
// 本家の apierr (code + id) には載せない。プラグインの名前空間にある Elythia
// 独自の口なので、本家の `INVALID_PARAM` と名前を分けて取り違えを防ぐ。
const (
	CodeUnavailable        = "SECRETS_UNAVAILABLE"
	CodeUnknownSecret      = "UNKNOWN_SECRET"
	CodeInvalidSecretParam = "INVALID_SECRET_PARAM"
)

// Handler serves one plugin's secret endpoints.
type Handler struct {
	svc      *pluginsecret.Service
	plugin   string
	declared []plugin.SecretSpec
}

// New builds the handler for one plugin.
func New(svc *pluginsecret.Service, pluginName string, declared []plugin.SecretSpec) *Handler {
	return &Handler{svc: svc, plugin: pluginName, declared: declared}
}

// SecretInfo is one row of the list response. **値は持たない。**
type SecretInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Declared reports whether the plugin declares this secret in
	// Definition.Secrets. 宣言されていない値 (プラグインが実行時に自分で
	// 置いたもの) は、管理画面からは消せるが入れられない。
	Declared   bool `json:"declared"`
	Configured bool `json:"configured"`
	// Readable is false when the stored value does not decrypt with the
	// current key (the key was replaced or lost).
	Readable bool `json:"readable"`
	// Hint is the last 4 characters of a value at least 20 characters long.
	Hint      *string `json:"hint"`
	UpdatedAt *string `json:"updatedAt"`
}

// ListResponse is the body of POST /api/plugin/<name>/_secrets.
type ListResponse struct {
	// Available is false when pluginSecretKey is not configured. そのときは
	// 保存も削除もできない。
	Available bool         `json:"available"`
	Secrets   []SecretInfo `json:"secrets"`
}

// List returns the declared and stored secrets without their values.
func (h *Handler) List(c echo.Context) error {
	statuses, err := h.svc.Statuses(c.Request().Context(), h.plugin)
	if err != nil {
		slog.Error("plugin secrets: 一覧を読めません", "plugin", h.plugin, "err", err)
		return internalError(c)
	}
	stored := make(map[string]pluginsecret.Status, len(statuses))
	for _, st := range statuses {
		stored[st.Name] = st
	}

	out := make([]SecretInfo, 0, len(h.declared)+len(statuses))
	seen := make(map[string]struct{}, len(h.declared))
	// 宣言順に並べる。プラグインの作者が意図した順で入力欄を出すため。
	for _, sp := range h.declared {
		seen[sp.Name] = struct{}{}
		out = append(out, info(sp.Name, sp.Description, true, stored[sp.Name]))
	}
	var extra []SecretInfo
	for _, st := range statuses {
		if _, ok := seen[st.Name]; ok {
			continue
		}
		extra = append(extra, info(st.Name, "", false, st))
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Name < extra[j].Name })
	out = append(out, extra...)

	return c.JSON(http.StatusOK, ListResponse{Available: h.svc.Available(), Secrets: out})
}

func info(name, desc string, declared bool, st pluginsecret.Status) SecretInfo {
	si := SecretInfo{
		Name:        name,
		Description: desc,
		Declared:    declared,
		Configured:  st.Configured,
		Readable:    st.Readable,
	}
	if st.Hint != "" {
		h := st.Hint
		si.Hint = &h
	}
	if st.Configured {
		u := st.UpdatedAt.UTC().Format(time.RFC3339Nano)
		si.UpdatedAt = &u
	}
	return si
}

type setRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Set stores a declared secret. 204 を返し、値も末尾も返さない。
func (h *Handler) Set(c echo.Context) error {
	var req setRequest
	if err := bind(c, &req); err != nil {
		return codedError(c, http.StatusBadRequest, "リクエストを読めません", CodeInvalidSecretParam)
	}
	if !h.isDeclared(req.Name) {
		// **宣言した名前だけを入れられる。** 管理画面から任意の名前を作れると、
		// プラグインが読まない値が溜まるうえ、プラグインが実行時に自分で
		// 置く値 (トークンなど) を管理者が上書きできてしまう。
		return codedError(c, http.StatusBadRequest, "このプラグインはその名前の値を宣言していません", CodeUnknownSecret)
	}
	// **前後の空白は落とす。** 管理画面に貼り付けた API キーに改行や空白が
	// 付いたまま保存されると、外部サービスの認証で初めて失敗し、値が見えない
	// ので原因に辿り着けない。落とすのはこの入口だけで、プラグイン自身の Set は
	// 渡されたバイト列をそのまま置く。空白だけの値は空として断る (service が弾く)。
	value := strings.TrimSpace(req.Value)
	err := h.svc.Set(c.Request().Context(), h.plugin, req.Name, value)
	if err != nil {
		return h.writeError(c, err)
	}
	slog.Info("plugin secret updated", "plugin", h.plugin, "name", req.Name, "by", userID(c))
	return c.NoContent(http.StatusNoContent)
}

type deleteRequest struct {
	Name string `json:"name"`
}

// Delete removes a secret, declared or not.
func (h *Handler) Delete(c echo.Context) error {
	var req deleteRequest
	if err := bind(c, &req); err != nil {
		return codedError(c, http.StatusBadRequest, "リクエストを読めません", CodeInvalidSecretParam)
	}
	if err := h.svc.Delete(c.Request().Context(), h.plugin, req.Name); err != nil {
		return h.writeError(c, err)
	}
	slog.Info("plugin secret deleted", "plugin", h.plugin, "name", req.Name, "by", userID(c))
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) isDeclared(name string) bool {
	for _, sp := range h.declared {
		if sp.Name == name {
			return true
		}
	}
	return false
}

func (h *Handler) writeError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, plugin.ErrSecretsUnavailable):
		return codedError(c, http.StatusBadRequest,
			"秘密の値を使えません。設定ファイルに pluginSecretKey を設定してください", CodeUnavailable)
	// 管理画面にそのまま出すので、文言は sentinel のものを使う (包んだエラーの
	// 前置きや内部の事情を利用者に見せない)。
	case errors.Is(err, pluginsecret.ErrInvalidName):
		return codedError(c, http.StatusBadRequest, pluginsecret.ErrInvalidName.Error(), CodeInvalidSecretParam)
	case errors.Is(err, pluginsecret.ErrInvalidValue):
		return codedError(c, http.StatusBadRequest, pluginsecret.ErrInvalidValue.Error(), CodeInvalidSecretParam)
	}
	slog.Error("plugin secrets: 保存できません", "plugin", h.plugin, "err", err)
	return internalError(c)
}

// bind decodes the JSON body. 未知のキーは許す — 本体の frontend は token を
// body の `i` に載せて送るため。
func bind(c echo.Context, v any) error {
	err := json.NewDecoder(c.Request().Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func userID(c echo.Context) string {
	if u := middleware.GetUser(c); u != nil {
		return u.ID
	}
	return ""
}

func codedError(c echo.Context, status int, message, code string) error {
	return c.JSON(status, map[string]any{"error": map[string]any{"message": message, "code": code}})
}

func internalError(c echo.Context) error {
	return c.JSON(http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"message": "Internal error."},
	})
}
