/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect } from 'vitest';
import { resolveRateLimitStop } from '@/utility/rate-limit-stop.js';

/**
 * mk-go: 429 を受けたときに自動追い読みを止める判断 (#2955)。
 *
 * **サーバー側の制限は「叩くのをやめる」まで解けない。** store が拒否した
 * リクエストも記録するので、429 のまま叩き続けると窓が前へ押し戻され続ける
 * (実測で `Retry-After` が 58 秒前後に張り付いたまま解けなかった)。無限
 * スクロールが握り潰して再試行を続けると、自分のバケットを自分で開かない
 * まま固定し続ける。
 *
 * **「止めるかどうか」と「どちらを止めるか」の両方をここで固定する。** 呼び出し
 * 側に条件を残すと、片方を落とし忘れても型もテストも通ってしまう。
 */
describe('resolveRateLimitStop', () => {
	test('429 なら、その向きの追い読みを止める', () => {
		expect(resolveRateLimitStop({ code: 'RATE_LIMIT_EXCEEDED' }, 'older'))
			.toEqual({ rateLimited: true, stop: 'older' });
		expect(resolveRateLimitStop({ code: 'RATE_LIMIT_EXCEEDED' }, 'newer'))
			.toEqual({ rateLimited: true, stop: 'newer' });
	});

	test('**向きを取り違えない。** older で止めたのに newer を落とすと、止めたい側が止まらない', () => {
		expect(resolveRateLimitStop({ code: 'RATE_LIMIT_EXCEEDED' }, 'older').stop).not.toBe('newer');
		expect(resolveRateLimitStop({ code: 'RATE_LIMIT_EXCEEDED' }, 'newer').stop).not.toBe('older');
	});

	test('**他のエラーは止めない。** ネットワーク断は一時的で、再試行すれば直る', () => {
		for (const err of [
			{ code: 'INTERNAL_ERROR' },
			{ code: 'NO_SUCH_USER' },
			{ code: 'FORBIDDEN' },
			new TypeError('Failed to fetch'),
			null,
			undefined,
			'RATE_LIMIT_EXCEEDED', // 文字列そのものは err.code ではない
		]) {
			expect(resolveRateLimitStop(err, 'older')).toEqual({ rateLimited: false, stop: null });
		}
	});
});
