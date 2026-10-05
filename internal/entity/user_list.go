package entity

import (
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// UserList is the upstream Misskey TS-compatible packed shape for
// users/lists/* endpoints (= /api/users/lists/{create,show,list}).
//
// upstream の users/lists/create は { id, createdAt, name, userIds, isPublic }
// を返す。旧 mk-go は model.UserList をそのまま JSON 化していて
// `{ id, userId, name, isPublic }` 形になっており、`createdAt` / `userIds`
// が欠落、逆に `userId` が露出する drift があった (#871)。本構造体は
// upstream と一致する shape を提供する。
type UserList struct {
	ID        string   `json:"id"`
	CreatedAt string   `json:"createdAt"`
	Name      string   `json:"name"`
	UserIDs   []string `json:"userIds"`
	IsPublic  bool     `json:"isPublic"`
}

// PackUserList converts a model.UserList plus its member IDs into the
// upstream-compatible JSON shape. createdAt は idGen.ParseTime(list.ID) で
// aidx ID から導出する (mk-go は user_list table に createdAt 列を持たない
// 設計のため)。memberIDs が nil なら空配列で返す (upstream 同様 list でも
// `userIds: []` を返すので JSON では `null` ではなく `[]` を保証)。
func PackUserList(list *model.UserList, memberIDs []string, idGen id.Generator) UserList {
	createdAt := ""
	if idGen != nil {
		if t, err := idGen.ParseTime(list.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	if memberIDs == nil {
		memberIDs = []string{}
	}
	return UserList{
		ID:        list.ID,
		CreatedAt: createdAt,
		Name:      list.Name,
		UserIDs:   memberIDs,
		IsPublic:  list.IsPublic,
	}
}
