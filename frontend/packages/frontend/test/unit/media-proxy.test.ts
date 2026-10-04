/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { MediaProxy } from '@@/js/media-proxy.js';
import type * as Misskey from 'misskey-js';

const INSTANCE = 'https://example.com';

function makeProxy(mediaProxy = `${INSTANCE}/proxy`): MediaProxy {
	return new MediaProxy({ mediaProxy } as Misskey.entities.MetaDetailed, INSTANCE);
}

describe('MediaProxy.getStaticImageUrl', () => {
	// #2913: アバター未設定の利用者の avatarUrl は相対の `/identicon/<username>`。
	// これをプロキシに包むと allowlist に無いので 403 + max-age=86400 になり、
	// 静止画設定を有効にした利用者にはアイコンが 1 日壊れて見えていた。
	test('leaves a relative identicon URL alone', () => {
		expect(makeProxy().getStaticImageUrl('/identicon/alice'))
			.toBe(`${INSTANCE}/identicon/alice`);
	});

	test('leaves an absolute same-origin identicon URL alone', () => {
		expect(makeProxy().getStaticImageUrl(`${INSTANCE}/identicon/bob@remote.example`))
			.toBe(`${INSTANCE}/identicon/bob@remote.example`);
	});

	// **同一オリジン限定であること。** 他インスタンスの `/identicon/` はこちらの
	// 生成物ではないので、素通しにすると静止画設定が効かなくなる。
	test('still proxies an identicon URL from another instance', () => {
		const url = makeProxy().getStaticImageUrl('https://other.example/identicon/carol');
		expect(url).toContain(`${INSTANCE}/proxy/static.webp?`);
		expect(new URL(url).searchParams.get('url')).toBe('https://other.example/identicon/carol');
		expect(new URL(url).searchParams.get('static')).toBe('1');
	});

	// 以下は回帰防止。identicon の分岐を足したことで既存の経路が変わっていないこと。
	test('proxies a drive file URL', () => {
		const url = makeProxy().getStaticImageUrl(`${INSTANCE}/files/abc.gif`);
		expect(url).toContain(`${INSTANCE}/proxy/static.webp?`);
		expect(new URL(url).searchParams.get('url')).toBe(`${INSTANCE}/files/abc.gif`);
	});

	test('appends static=1 to an emoji URL', () => {
		expect(makeProxy().getStaticImageUrl(`${INSTANCE}/emoji/foo.webp`))
			.toBe(`${INSTANCE}/emoji/foo.webp?static=1`);
	});

	test('appends static=1 to an already-proxied URL', () => {
		const already = `${INSTANCE}/proxy/avatar.webp?url=https%3A%2F%2Fremote.example%2Fa.gif`;
		expect(makeProxy().getStaticImageUrl(already)).toBe(`${already}&static=1`);
	});

	// mediaProxy が外部ホストでも、identicon の判定は自インスタンスの URL で行う。
	test('uses the instance origin, not the media proxy origin, to detect identicons', () => {
		expect(makeProxy('https://proxy.example').getStaticImageUrl('/identicon/dave'))
			.toBe(`${INSTANCE}/identicon/dave`);
	});
});
