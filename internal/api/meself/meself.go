// Package meself promotes a packed UserDetailed into MeDetailed when the target
// user is the viewer themselves.
//
// upstream Misskey の UserEntityService.pack は `isDetailed && isMe` のとき
// MeDetailed 専用 field を足す。要求された schema が 'UserDetailed' でも、
// 対象が viewer 自身なら MeDetailed が返る。つまり users/show の単体だけでなく、
// users のリストや hashtags/users のように「結果に自分が混ざりうる」endpoint 全てが
// 対象になる。mk-go は users/show の単体でしか昇格しておらず、他の endpoint では
// 自分だけ field が欠けた UserDetailed が返っていた。
//
// MeDetailed の一部 (未読 / policies / roles / pinnedPage) は entity 層では
// 計算できず、それらの依存を全 handler へ配線するのは現実的でない。そこで
// 補完だけを Enricher として切り出し、全依存を既に持つ /api/i の handler を
// 包んだ Packer を router で 1 つ作り、昇格させる handler へ渡す (#3784)。
// 以前はパッケージの変数に 1 度だけ登録していた。
package meself

import (
	"context"
	"encoding/json"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// Enricher fills the MeDetailed fields that only a handler with the full
// dependency set can resolve (unread counters, role policies, pinned page).
// resp は MeDetailed を JSON 展開した map で、実装は必要な key を上書きする。
type Enricher interface {
	EnrichSelf(ctx context.Context, u *model.User, profile *model.UserProfile, resp map[string]any)
}

// Packer promotes the viewer's own UserDetailed into MeDetailed, filling the
// handler-only fields through its Enricher.
//
// nil の *Packer と、Enricher を持たない値は補完しない (entity 層の MeDetailed の
// 既定値のまま返す)。渡し忘れてもエラーにならず補完が抜けるだけなので、配線は
// internal/server の配線のテストが handler ごとに見る。
type Packer struct {
	enricher Enricher
}

// NewPacker returns a Packer that fills the MeDetailed fields with e.
// A nil e yields a Packer that does not enrich.
func NewPacker(e Enricher) *Packer {
	return &Packer{enricher: e}
}

// IsSelf reports whether the packed target is the viewer.
func IsSelf(viewer *model.User, targetID string) bool {
	return viewer != nil && viewer.ID == targetID
}

// Pack returns d as-is for anyone but the viewer, and a MeDetailed map for the
// viewer themselves.
//
// 戻り値が any なのは、同じ配列に UserDetailed と MeDetailed が混ざるため
// (upstream の packMany も self だけ field が増えた object を返す)。
func (p *Packer) Pack(ctx context.Context, d entity.UserDetailed, u *model.User, profile *model.UserProfile, viewer *model.User) any {
	if !IsSelf(viewer, u.ID) {
		return d
	}
	return p.PackMe(ctx, d, u, profile)
}

// PackMe promotes an already-packed UserDetailed into the MeDetailed map,
// without re-checking the viewer. 呼び出し側が self を判定済みのとき用。
func (p *Packer) PackMe(ctx context.Context, d entity.UserDetailed, u *model.User, profile *model.UserProfile) any {
	me := entity.AsMeDetailed(d, u, profile)
	b, err := json.Marshal(me)
	if err != nil {
		// MeDetailed は JSON-safe な型だけで構成されるので実際には起きない。
		// 起きたときは昇格を諦めて UserDetailed をそのまま返す (field が
		// 欠けるだけで壊れた応答にはしない)。
		return d
	}
	resp := map[string]any{}
	if err := json.Unmarshal(b, &resp); err != nil {
		return d
	}
	// policies は role 依存で entity 層では出せず、MeDetailed 側も omitempty
	// なので未設定だと key ごと消える。misskey_dart の MeDetailed.fromJson は
	// 非 null Map を要求する (#1240) ため、Enricher が権威値で上書きする前に
	// 既定値を入れておく。
	if _, ok := resp["policies"]; !ok {
		resp["policies"] = role.DefaultPolicies()
	}
	if p != nil && p.enricher != nil {
		p.enricher.EnrichSelf(ctx, u, profile, resp)
	}
	return resp
}
