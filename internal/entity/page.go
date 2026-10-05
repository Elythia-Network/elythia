package entity

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// PackPage converts a model.Page into the map shape returned by /api/pages/*
// and embedded as UserDetailed.pinnedPage. Returns nil when p is nil so
// callers can assign the result directly to a nilable field.
// When an idGen is supplied, createdAt is derived from the aidx ID and
// rendered in the same "2006-01-02T15:04:05.000Z" format used by other
// entity packers (PackNote / PackDriveFile etc). updatedAt is rendered with
// the same format for consistency.
//
// Use PackPageWithContext when the caller can supply the page owner +
// per-viewer state — list endpoints (/api/i/pages, /api/users/pages,
// /api/pages/featured) and the page detail (/api/pages/show) all need
// `page.user` populated or the frontend MkPagePreview / page.vue templates
// fail to render (`page.user.username` is unconditional).
func PackPage(p *model.Page, idGens ...id.Generator) map[string]any {
	if p == nil {
		return nil
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := map[string]any{
		"id":                  p.ID,
		"updatedAt":           p.UpdatedAt.UTC().Format(tsFormat),
		"title":               p.Title,
		"name":                p.Name,
		"summary":             p.Summary,
		"alignCenter":         p.AlignCenter,
		"hideTitleWhenPinned": p.HideTitleWhenPinned,
		"font":                p.Font,
		"userId":              p.UserID,
		"eyeCatchingImageId":  p.EyeCatchingImageID,
		// golden Page は eyeCatchingImage を present 必須 (DriveFile | null) とする。
		// 画像が無い / lookup miss でも null を出す (omit しない)。実画像は
		// PackPageWithContext が ctx.EyeCatchingImage で上書きする。
		"eyeCatchingImage": nil,
		// content / variables / attachedFiles は misskey_dart の Page.fromJson が
		// 非null List として cast するため、空でも null ではなく [] を返す
		// (#1237)。content は golden Page でも PageBlock[] 必須 (non-null) だが、
		// 当初 #1237 では content だけ rawJSONBytes のまま取りこぼしていた。
		"content":       migratePageContent(p.Content),
		"variables":     jsonArrayOrEmpty(p.Variables),
		"attachedFiles": []any{},
		"script":        p.Script,
		"visibility":    string(p.Visibility),
		"likedCount":    p.LikedCount,
	}
	if len(idGens) > 0 && idGens[0] != nil {
		if t, err := idGens[0].ParseTime(p.ID); err == nil {
			out["createdAt"] = t.UTC().Format(tsFormat)
		}
	}
	return out
}

// PackPageContext carries per-call enrichment data for PackPageWithContext.
// All fields are optional; nil leaves the corresponding output field
// **omitted entirely** (never null) so the frontend cannot crash on
// `page.user.username` etc.
type PackPageContext struct {
	// IDGen drives the aidx → createdAt derivation just like the variadic
	// arg on PackPage.
	IDGen id.Generator
	// Owner is the page.user owner. Required by frontend MkPagePreview
	// (`page.user.username` is read unconditionally); when nil the `user`
	// field is omitted entirely. List callers MUST therefore drop the row
	// when their lookup misses (= upstream packMany semantics), otherwise
	// MkPagePreview crashes on the next `page.user.username` access.
	Owner *model.User
	// EyeCatchingImage is the packed Drive file when Page.EyeCatchingImageID
	// is set (entity.DriveFileEntity or nil). nil means "either no image or the
	// file lookup missed"; both render the same in MkPagePreview (the
	// `v-if="page.eyeCatchingImage"` guard simply hides the block)。#1662 で
	// handler が実 drive file を解決して渡せるようにした。
	EyeCatchingImage any
	// AttachedFiles are the packed Drive files referenced by image blocks in
	// the page content (#1662)。nil leaves PackPage's default empty array; a
	// non-nil slice (possibly empty) overrides it.
	AttachedFiles []any
	// IsLiked is the viewer's like state. nil omits the field (upstream
	// `pages/show` returns undefined when there is no logged-in viewer).
	IsLiked *bool
}

// PackPageWithContext extends PackPage by adding `user`, `eyeCatchingImage`,
// and `isLiked` fields when the context provides them. Upstream
// PageEntityService.pack always populates `user`; mk-go list endpoints used
// to omit it and the frontend MkPagePreview crashed silently (template
// `page.user.username` reference) — see #1134.
func PackPageWithContext(p *model.Page, ctx PackPageContext) map[string]any {
	out := PackPage(p, ctx.IDGen)
	if out == nil {
		return nil
	}
	if ctx.Owner != nil {
		out["user"] = PackUserLite(ctx.Owner)
	}
	// 注: Owner が nil の場合は user field を omit する。null 出しすると
	// frontend MkPagePreview の page.user.username で再 throw するので、
	// caller (list handler) は owner lookup miss の行を skip する責務がある
	// (#1134 で対応した list endpoint 群はすべて owner 確保 → pack の順)。
	if ctx.EyeCatchingImage != nil {
		out["eyeCatchingImage"] = ctx.EyeCatchingImage
	}
	// attachedFiles は content の image block から解決した drive file 群 (#1662)。
	// nil なら PackPage の default 空配列を維持する。
	if ctx.AttachedFiles != nil {
		out["attachedFiles"] = ctx.AttachedFiles
	}
	if ctx.IsLiked != nil {
		out["isLiked"] = *ctx.IsLiked
	}
	return out
}

// PageDriveFileLookup is the narrow drive-file repository view used to resolve
// a page's attachedFiles / eyeCatchingImage (#1662)。repository.DriveFileRepository
// が構造的に満たすので、entity は repository に依存しない。
type PageDriveFileLookup interface {
	FindByIDs(ids []string) ([]*model.DriveFile, error)
	FindByID(id string) (*model.DriveFile, error)
}

// CollectPageImageFileIDs walks the page content blocks (jsonb) recursively and
// returns the fileId of every `type==image` block in document order (upstream
// PageEntityService の collectFile)。children を持つ block は再帰する。不正
// JSON / 空は nil。
func CollectPageImageFileIDs(content []byte) []string {
	var blocks []any
	if len(content) == 0 || json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var ids []string
	var walk func(xs []any)
	walk = func(xs []any) {
		for _, x := range xs {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "image" {
				if fid, _ := m["fileId"].(string); fid != "" {
					ids = append(ids, fid)
				}
			}
			if ch, ok := m["children"].([]any); ok {
				walk(ch)
			}
		}
	}
	walk(blocks)
	return ids
}

// ResolvePageDriveFiles resolves a page's attachedFiles (drive files referenced
// by image blocks in page.content, page owner-scope) and eyeCatchingImage
// (#1662、upstream PageEntityService.pack)。lookup nil / p nil なら (nil, nil) を
// 返し、呼び出し元は PackPage の default (空配列 / null) を維持する。
//
// attachedFiles は content (block) 順を保ち image block ごとに pack する
// (upstream は block 単位で push するため同一 file が複数 block にあれば重複)。
// owner-scope は upstream findOneBy({id, userId: page.userId}) と一致。
// eyeCatchingImage は owner-scope 無しで id から pack する (upstream 同様)。
func ResolvePageDriveFiles(p *model.Page, lookup PageDriveFileLookup, idGen id.Generator) ([]any, any) {
	if lookup == nil || p == nil {
		return nil, nil
	}
	attached := []any{}
	if ids := CollectPageImageFileIDs(p.Content); len(ids) > 0 {
		uniq := make([]string, 0, len(ids))
		seen := make(map[string]struct{}, len(ids))
		for _, fid := range ids {
			if _, dup := seen[fid]; dup {
				continue
			}
			seen[fid] = struct{}{}
			uniq = append(uniq, fid)
		}
		byID := make(map[string]*model.DriveFile, len(uniq))
		if files, err := lookup.FindByIDs(uniq); err == nil {
			for _, f := range files {
				if f.UserID != nil && *f.UserID == p.UserID {
					byID[f.ID] = f
				}
			}
		}
		for _, fid := range ids {
			if f, ok := byID[fid]; ok {
				attached = append(attached, PackDriveFile(f, idGen))
			}
		}
	}
	var eyeCatchingImage any
	if p.EyeCatchingImageID != nil {
		if f, err := lookup.FindByID(*p.EyeCatchingImageID); err == nil && f != nil {
			eyeCatchingImage = PackDriveFile(f, idGen)
		}
	}
	return attached, eyeCatchingImage
}

// migratePageContent applies upstream PageEntityService.migrate (#1773 re-audit)
// at pack time: legacy `type:'input'` blocks are rewritten to `textInput` /
// `numberInput` for backwards compatibility with very old pages, and a
// numberInput's `default` is coerced to an integer (parseInt). The walk recurses
// into `children` like upstream.
//
// When no legacy block is rewritten the original bytes are returned verbatim, so
// modern pages (which never carry `type:'input'`) serialize byte-identically to
// the stored jsonb. Empty / non-array / invalid content falls back to
// jsonArrayOrEmpty.
//
// Unlike upstream, the migrated content is NOT written back to the DB: that is a
// pure read-side optimization with no client-visible effect (the transform is
// idempotent), and entity packers must remain free of DB side effects.
func migratePageContent(content []byte) any {
	if len(content) == 0 {
		return []any{}
	}
	var blocks []any
	if json.Unmarshal(content, &blocks) != nil {
		// content が array でない / 不正 JSON は stored bytes を verbatim 返す。
		return json.RawMessage(content)
	}
	migrated := false
	var walk func(xs []any)
	walk = func(xs []any) {
		for _, x := range xs {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "input" {
				switch it, _ := m["inputType"].(string); it {
				case "text":
					m["type"] = "textInput"
					migrated = true
				case "number":
					m["type"] = "numberInput"
					// upstream: if (x.default) x.default = parseInt(x.default, 10)
					if d, ok := m["default"]; ok && jsTruthy(d) {
						m["default"] = jsParseInt10(d)
					}
					migrated = true
				}
			}
			if ch, ok := m["children"].([]any); ok {
				walk(ch)
			}
		}
	}
	walk(blocks)
	if !migrated {
		// 何も書き換えていなければ stored bytes を verbatim 返す (modern page は
		// re-marshal による byte ずれを起こさない)。
		return json.RawMessage(content)
	}
	return blocks
}

// jsTruthy mirrors JavaScript truthiness for the values decoded from page block
// JSON (used by migratePageContent's `if (x.default)` guard)。
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return true
	}
}

// jsParseInt10 mirrors JavaScript `parseInt(v, 10)` for page numberInput
// defaults. Strings are parsed up to the leading integer prefix (NaN → null to
// match JSON.stringify(NaN)); numbers are truncated toward zero.
func jsParseInt10(v any) any {
	switch t := v.(type) {
	case float64:
		return int(math.Trunc(t))
	case string:
		s := strings.TrimSpace(t)
		i := 0
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return nil
		}
		n, err := strconv.Atoi(s[:j])
		if err != nil {
			return nil
		}
		return n
	default:
		return v
	}
}

// rawJSONBytes returns the raw JSON bytes as json.RawMessage so JSON encoders
// emit jsonb column content verbatim. Empty bytes become nil (JSON null).
func rawJSONBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// jsonArrayOrEmpty returns the raw jsonb array bytes verbatim, or an empty
// array `[]` when the column is empty/null. Used for fields the client casts
// to a non-nullable List (e.g. page.variables) so they never serialize to null.
func jsonArrayOrEmpty(b []byte) any {
	if len(b) == 0 {
		return []any{}
	}
	return json.RawMessage(b)
}
