/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * mk-go: decides whether a failed pagination fetch must stop auto-loading (#2955).
 *
 * **判断をここに閉じる。** 呼び出し側に条件を残すと、`canFetch*` を落とし忘れた
 * ときに**型もテストも通ったまま無限スクロールが自走し続ける**。#2939 で
 * `resolveClientUpdate` を作ったのと同じ理由。
 *
 * **なぜ止める必要があるのか。** サーバー側のレート制限は「叩くのをやめる」まで
 * 解けない — store が拒否したリクエストも記録するので、429 のまま叩き続けると
 * 窓が前へ押し戻され続ける (実測で `Retry-After` が 58 秒前後に張り付いたまま
 * 解けなかった)。握り潰して再試行を続けると、**自分のバケットを自分で開かない
 * まま固定し続ける**。
 *
 * **429 だけを区別する。** ネットワーク断などは従来どおり黙って握り潰す
 * (一時的なもので、再試行すれば直る)。レート制限は逆で、**再試行が状況を
 * 悪化させる**唯一のケース。
 */
export type RateLimitStop = {
	/** 利用者に理由を出すか。 */
	rateLimited: boolean;
	/** 自動追い読みを止める向き。止めないなら null。 */
	stop: 'older' | 'newer' | null;
};

export function resolveRateLimitStop(err: unknown, direction: 'older' | 'newer'): RateLimitStop {
	if ((err as { code?: string } | null)?.code !== 'RATE_LIMIT_EXCEEDED') {
		return { rateLimited: false, stop: null };
	}
	return { rateLimited: true, stop: direction };
}
