package server

import (
	"log/slog"

	"github.com/elythia-network/elythia/plugin"
)

/*
 * peer の受け口に掛ける本文上限を、プラグインごとに決める (#2537 の硬化)。
 *
 * 受け口は **署名を検証していない相手から到達できる**。ブロックしていない
 * インスタンスなら誰でも POST でき、署名が無くても本文は読ませられる
 * (auth.Authenticate が token 抽出のため body を読むので、handler に入る前に
 * 消費が終わっている)。したがって上限は handler ではなく global の
 * BodyLimitByPath に置くしかない。
 *
 * 値を 1 つの定数にすると「一番大きなプラグインに全体が引きずられる」。
 * プラグインごとに宣言させて、露出を実際に必要な分だけに縮める。
 */

// peerDefaultMaxBody is the cap a peered plugin gets without asking, and the
// largest it may claim on its own.
//
// **AP inbox と同じ値**にしてある。これで「プラグインを入れただけでは、
// インスタンスの露出が inbox 以上に広がらない」という性質が立つ。これを
// 超える値は運営者が設定で許可したときだけ通る。
const peerDefaultMaxBody int64 = 64 << 10

// peerHardMaxBody caps even an operator override.
//
// `/api` 全体の body limit と同じ。これより大きい値を設定しても global 側が
// 先に 413 を返すので、通ると思わせないためここで頭打ちにする。
const peerHardMaxBody int64 = 1 << 20

// peerMinMaxBody is the floor.
//
// エンベロープが入らない値を設定して「何を送っても 413」になるのを防ぐ。
const peerMinMaxBody int64 = 1 << 10

// peerMaxBodyKey is the reserved per-plugin setting an operator uses to raise
// (or lower) the cap.
//
// **完全一致で引いてはいけない。** viper は設定ファイル由来の map キーを
// 小文字化するので、`.config/default.yml` に camelCase で書いた値は
// `peermaxbody` で届く (`docs/plugins/operating.md` の config-dump の例が
// `status.maxlength` になっているのはこのため)。一致しないと運営者の設定が
// 一度も効かず、**絞る方向の設定も黙って無視される**。
const peerMaxBodyKey = "peerMaxBody"

// peerBodyLimit resolves the effective cap for one plugin.
//
// 返り値の 2 つ目は、宣言値が運営者の許可なく丸められたかどうか。呼び出し元が
// warn を出すために使う (黙って無視すると「設定したつもり」で通ってしまう)。
func peerBodyLimit(def plugin.Definition, settings map[string]any) (int64, bool) {
	limit := def.PeerMaxBody
	if limit <= 0 {
		limit = peerDefaultMaxBody
	}
	clamped := false
	if override, ok := peerMaxBodyOverride(settings); ok {
		limit = override
	} else if limit > peerDefaultMaxBody {
		// 宣言だけでは既定値を超えられない。
		limit = peerDefaultMaxBody
		clamped = true
	}
	if limit < peerMinMaxBody {
		limit = peerMinMaxBody
	}
	if limit > peerHardMaxBody {
		limit = peerHardMaxBody
	}
	return limit, clamped
}

// peerMaxBodyOverride reads the operator's per-plugin override.
//
// YAML の数値は viper を通ると int / float64 のどちらにもなりうるので両方見る。
// 値が読めないときは override 無しとして扱う (設定の書き間違いで上限が
// 消えるより、宣言側の既定に倒れる方が安全側)。
func peerMaxBodyOverride(settings map[string]any) (int64, bool) {
	v, ok := pluginSetting(settings, peerMaxBodyKey)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return int64(n), n > 0
	case int64:
		return n, n > 0
	case float64:
		return int64(n), n > 0
	}
	return 0, false
}

// peerBodyLimitsByPath maps each peered plugin's endpoint path to its cap.
//
// **BodyLimitByPath に渡すためのもの。** newServer は plugins を受け取って
// いるので、middleware を組む時点で全部分かる。
func peerBodyLimitsByPath(plugins []plugin.Definition, settings map[string]map[string]any) map[string]int64 {
	if len(plugins) == 0 {
		return nil
	}
	out := make(map[string]int64, len(plugins))
	for _, def := range plugins {
		if !def.Peered || !pluginEnabled(settings[def.Name]) {
			continue
		}
		limit, clamped := peerBodyLimit(def, settings[def.Name])
		if clamped {
			slog.Warn("plugin peer body limit clamped",
				"plugin", def.Name,
				"declared", def.PeerMaxBody,
				"effective", limit,
				"hint", "plugins."+def.Name+"."+peerMaxBodyKey+" で運営者が許可したときだけ既定値を超えられる")
		}
		out[peerAPIPrefix+def.Name+peerPath] = limit
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
