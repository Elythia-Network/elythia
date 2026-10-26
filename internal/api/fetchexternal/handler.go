// Package fetchexternal implements POST /api/fetch-external-resources, an
// integrity-checked fetch of an external JSON resource ({type, data}). The
// caller supplies the resource URL and the expected sha512 of its `data`
// field; the server fetches the URL (via an SSRF-safe client), verifies the
// hash, and returns the resource. Used by the frontend to load external
// themes / plugins / assets whose content hash is already known.
//
// Mirrors Misskey TS endpoints/fetch-external-resources.ts.
package fetchexternal

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/labstack/echo/v4"
)

// FetchTimeout matches the upstream `timeout: 5000` ms.
const FetchTimeout = 5 * time.Second

// maxBodyBytes caps the external response size to avoid memory abuse. External
// theme/plugin JSON is small; 5 MiB is generous.
const maxBodyBytes = 5 << 20

// Handler serves fetch-external-resources via an outbound HTTP client.
type Handler struct {
	httpClient *http.Client
	userAgent  string
}

// New builds a Handler bound to the given (SSRF-safe) outbound HTTP client and
// User-Agent.
func New(httpClient *http.Client, userAgent string) *Handler {
	return &Handler{httpClient: httpClient, userAgent: userAgent}
}

// Fetch handles POST /api/fetch-external-resources.
func (h *Handler) Fetch(c echo.Context) error {
	var raw struct {
		URL  *string `json:"url"`
		Hash *string `json:"hash"`
	}
	if err := c.Bind(&raw); err != nil || raw.URL == nil || raw.Hash == nil {
		return apierr.JSONInvalidParam(c)
	}
	req := struct{ URL, Hash string }{*raw.URL, *raw.Hash}
	// 本家の paramDef は url・hash を required の string とするだけで、空文字も
	// 通す。空の hash は取得した本文のハッシュと一致しないので、本家と同じく
	// EXT_RESOURCE_HASH_DIDNT_MATCH になる (#3845)。
	//
	// 絶対の http(s) の URL でないものは、取得の前に 400 にする。本家は相対の URL や
	// http(s) 以外の scheme では fetch が例外を投げて 500 になるが、呼び出し側の
	// 誤りなので 500 にはしない。data: の URL は本家では取得できるが、Elythia は
	// 外部のリソースとして扱わず拒否する (以前から)。
	if u, err := url.Parse(req.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return apierr.JSONInvalidParam(c)
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), FetchTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return h.invalidSchema(c)
	}
	httpReq.Header.Set("User-Agent", h.userAgent)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		// fetch 失敗 (DNS / SSRF block / timeout 等) も schema 取得不能として
		// invalidSchema 扱いにする (upstream の getJson 失敗経路相当)。
		return h.invalidSchema(c)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return h.invalidSchema(c)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return h.invalidSchema(c)
	}
	// `data` を string として decode できなければ invalid schema。
	var res struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return h.invalidSchema(c)
	}

	// sha512(data) を CRLF 正規化してから計算する (upstream と同じ)。
	normalized := strings.ReplaceAll(res.Data, "\r\n", "\n")
	sum := sha512.Sum512([]byte(normalized))
	if hex.EncodeToString(sum[:]) != req.Hash {
		return c.JSON(http.StatusBadRequest, apierr.Error("EXT_RESOURCE_HASH_DIDNT_MATCH", "Hash did not match.", "693ba8ba-b486-40df-a174-72f8279b56a4"))
	}

	return c.JSON(http.StatusOK, map[string]any{"type": res.Type, "data": res.Data})
}

func (h *Handler) invalidSchema(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("EXT_RESOURCE_RETURNED_INVALID_SCHEMA", "External resource returned invalid schema.", "bb774091-7a15-4a70-9dc5-6ac8cf125856"))
}
