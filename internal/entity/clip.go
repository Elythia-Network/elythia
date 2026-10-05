package entity

import (
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// ClipExtras carries the viewer-dependent fields of the clip shape (#1562).
// upstream ClipEntityService.pack は favoritedCount を実カウント、isFavorited
// を me 有時のみ、notesCount を owner 閲覧時のみ出力する
// (ClipEntityService.ts:54-56)。
type ClipExtras struct {
	// FavoritedCount is the number of clip_favorite rows for this clip.
	FavoritedCount int64
	// IsFavorited reports whether the viewer favorited this clip. nil なら
	// フィールド自体を省略する (匿名 viewer、upstream は undefined)。
	IsFavorited *bool
	// NotesCount is the number of notes in this clip. nil ならフィールド自体を
	// 省略する (upstream は viewer != owner のとき undefined)。
	//
	// upstream ClipEntityService と同じく **その場で clip_note を数える**。
	// mk-go は以前 clip.notesCount 列に非正規化していたが、この列は upstream
	// に存在せず TS 製 DB では生えないため drop-in で壊れる。加えて TS と
	// mk-go を行き来すると TS 側での追加/削除がカウンタに反映されず恒久的に
	// ずれる (#2243)。
	NotesCount *int
}

// PackClip converts a model.Clip into the Misskey-compatible clip shape.
//
// misskey_dart の Clip.fromJson は createdAt (String) / userId (String) /
// user (UserLite) / isPublic (bool) / favoritedCount (num) を非null必須とする
// ため、これらを必ず出す (#1237)。owner は clip の所有ユーザーで、user フィールド
// を埋めるために caller が渡す (nil なら user は省略するが、misskey_dart 互換の
// ため呼び出し側は必ず owner を解決して渡すこと)。createdAt は clip ID (aidx)
// から復元する。
func PackClip(cl *model.Clip, idGen id.Generator, owner *model.User, extras ClipExtras) map[string]any {
	if cl == nil {
		return nil
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	// lastClippedAt は createdAt や codebase 内の他 timestamp と同じ ISO ms
	// 形式に揃える (nil なら null)。misskey_dart は nullable で受けるが形式は統一。
	var lastClippedAt any
	if cl.LastClippedAt != nil {
		lastClippedAt = cl.LastClippedAt.UTC().Format(tsFormat)
	}
	out := map[string]any{
		"id":             cl.ID,
		"userId":         cl.UserID,
		"name":           cl.Name,
		"description":    cl.Description,
		"isPublic":       cl.IsPublic,
		"lastClippedAt":  lastClippedAt,
		"favoritedCount": extras.FavoritedCount,
	}
	// notesCount は owner 閲覧時のみ (upstream は他人には undefined を返し
	// note 件数を露出しない、#1562)。
	if extras.NotesCount != nil {
		out["notesCount"] = *extras.NotesCount
	}
	if extras.IsFavorited != nil {
		out["isFavorited"] = *extras.IsFavorited
	}
	if idGen != nil {
		if t, err := idGen.ParseTime(cl.ID); err == nil {
			out["createdAt"] = t.UTC().Format(tsFormat)
		}
	}
	if owner != nil {
		out["user"] = PackUserLite(owner)
	}
	return out
}
