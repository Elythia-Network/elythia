// Package trustlevel grants a role to local users once they meet the operator's
// criteria, so an instance can start everyone on a conservative base policy and
// open things up as accounts establish a track record.
//
// #2585 の検証台。**新しい公開面を足さずにどこまで書けるか**を確かめるために
// 作った。足りなかったものは README に記録する。
//
// # policy を持たない
//
// 判定した結果はロールとして `admin/roles/assign` で永続化し、プラグインは
// 判定状態しか持たない。解決時に policy を差し込む形だと、TS へ切り替えた瞬間に
// プラグインが消えて**利用者の権限が黙って変わる**。ロールなら切り替えても残り、
// プラグインを止めても最後に確定した状態が生きる。
// `EffectivePolicies`はロールで表現できない解決時の寄与に使えるが、このプラグインは
// 停止・TS切り戻し後も確定済みの昇格を残すため、引き続きロールを正本とする。
//
// # 緩和方向にしか動かさない
//
// base policy を控えめにしておき、条件を満たしたら足す。判定が失敗したときに
// 昇格しないだけで済むよう、初期状態を制限側に置くのが前提。
package trustlevel

import (
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/plugin"
)

// Plugin is the entry point the build-time generator references.
var Plugin = plugin.Definition{
	Name:       "trustlevel",
	Version:    "1.0.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
}

var migrations = []plugin.Migration{
	{Version: 1, SQL: `
		CREATE TABLE subjects (
			user_id      text PRIMARY KEY,
			granted      boolean NOT NULL DEFAULT false,
			held         boolean NOT NULL DEFAULT false,
			reason       text NOT NULL DEFAULT '',
			last_error   text NOT NULL DEFAULT '',
			evaluated_at timestamptz
		);
		CREATE INDEX subjects_granted_idx ON subjects (granted);

		-- reconcile 1 周の記録。サブ issue を切るときの根拠になるので、
		-- 「遅かった気がする」ではなく実測を残す (#2586)。
		CREATE TABLE runs (
			id           bigserial PRIMARY KEY,
			started_at   timestamptz NOT NULL,
			finished_at  timestamptz NOT NULL,
			scanned      integer NOT NULL,
			granted      integer NOT NULL,
			failed       integer NOT NULL,
			error        text NOT NULL DEFAULT ''
		);
		CREATE INDEX runs_started_at_idx ON runs (started_at DESC);
	`},
}

// config mirrors the `plugins.trustlevel` section of the instance config.
type config struct {
	// RoleID is the role granted once the criteria are met.
	RoleID string `json:"roleId"`
	// ActorID is the administrator the plugin acts as.
	//
	// **`AsSystem` は無い** (#2476)。管理操作は必ず誰かの権限で行われ、
	// モデレーションログにもこの ID で残る。
	ActorID string `json:"actorId"`
	// MinAccountAgeDays is how long an account must have existed.
	MinAccountAgeDays int `json:"minAccountAgeDays"`
	// MinNotes is the note count threshold.
	MinNotes int `json:"minNotes"`
	// Cron controls how often the reconcile runs (5-field, UTC).
	Cron string `json:"cron"`
	// PageSize is how many users to fetch per admin/show-users call.
	PageSize int `json:"pageSize"`
}

// errUnconfigured means the operator has not set the plugin up yet.
//
// ビルドに取り込んだ時点で有効になる (runtime の enabled は既定 true)。未設定で
// 起動を止めると、`disabled: true` を消しただけで config を書いていない
// インスタンスが全断するので、この場合だけは何も登録せずに見送る。
var errUnconfigured = errors.New("trustlevel: 未設定")

func loadConfig(ctx plugin.Context) (config, error) {
	c := config{
		MinAccountAgeDays: 7,
		MinNotes:          10,
		Cron:              "0 * * * *",
		PageSize:          100,
	}
	if err := ctx.Config().Unmarshal(&c); err != nil {
		return c, err
	}
	// roleId / actorId が両方とも空なら「まだ設定していない」とみなす。
	// **同梱プラグインなので、未設定で起動を止めてはいけない。** ルート登録が
	// エラーを返すとインスタンスごと起動しなくなる (plugin_wiring.go)。ビルドに
	// 含まれただけの状態で全断するのは割に合わない。
	if c.RoleID == "" && c.ActorID == "" {
		return c, errUnconfigured
	}
	// 片方だけ書いてあるのは書き途中か書き間違い。**ここは落とす** — 黙って何も
	// しないと、運営者は「設定したのに昇格しない」を延々と追うことになる。
	if c.RoleID == "" {
		return c, errors.New("roleId を設定してください (付与するロールの ID)")
	}
	if c.ActorID == "" {
		return c, errors.New("actorId を設定してください (実行主体になる管理者の ID)")
	}
	if c.MinAccountAgeDays < 0 {
		return c, fmt.Errorf("minAccountAgeDays は 0 以上で指定してください (%d)", c.MinAccountAgeDays)
	}
	if c.MinNotes < 0 {
		return c, fmt.Errorf("minNotes は 0 以上で指定してください (%d)", c.MinNotes)
	}
	// admin/show-users の limit は上限 100 (pagination.ResolveLimit)。
	// 超える値を渡すと mk-go 側で丸められ、offset 送りと件数がずれる。
	if c.PageSize <= 0 || c.PageSize > 100 {
		return c, fmt.Errorf("pageSize は 1〜100 で指定してください (%d)", c.PageSize)
	}
	return c, nil
}
