/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// プロフィールページのアイコンを押すとライトボックスが開く配線を固定する (#3124)。
//
// upstream のプロフィールのアイコンは押せない素の画像なので、これは mk-go 独自の
// 追加。**`home.test.ts` には足さない** — あちらは upstream のファイルで、追従の
// たびに競合させる意味が無い。
//
// 見るのは配線だけで、実際に拡大された絵が出るかは Playwright 側
// (`specs/mkgo/ui/profile_avatar_lightbox.spec.ts`) が実ブラウザで見る。

import { afterAll, afterEach, assert, beforeAll, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render, type RenderResult } from '@testing-library/vue';
import * as Misskey from 'misskey-js';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import XHome from '@/pages/user/home.vue';
import 'intersection-observer';

// `popupAsyncWithDialog` だけ差し替える。`home.vue` は `popupMenu` など他の
// os API も使うので、モジュールごと置き換えると無関係な箇所が壊れる。
//
// **引数に型を付けておく。** 付けないと `mock.calls[0]` が長さ 0 のタプルに
// 推論され、2 番目の要素 (props) を読む行が `vue-tsc` で落ちる。
const popupAsyncWithDialog = vi.fn(
	async (_component: unknown, _props: unknown, _events?: unknown) => ({ dispose: vi.fn() }),
);
// `vi.mock` の factory は巻き上げられて `const` より先に評価されうるので、
// 外側の変数は**呼び出し時に解決される形**で渡す (直接書くと TDZ になる)。
vi.mock('@/os.js', async (importOriginal) => ({
	...await importOriginal<typeof import('@/os.js')>(),
	popupAsyncWithDialog: (component: unknown, props: unknown, events?: unknown) =>
		popupAsyncWithDialog(component, props, events),
}));

// `showAvatar` は `import('@/components/MkLightbox.vue')` を即時に評価するが、
// モックした `popupAsyncWithDialog` はその promise を await しない。テストが先に
// 終わると読み込み途中で環境が壊され `EnvironmentTeardownError` になる (実測で
// 3 回中 2 回落ちた)。各テストの後で解決を待って窓を閉じる。
//
// **`vi.mock` でも塞げる**が採らない。factory 付き
// (`vi.mock('@/components/MkLightbox.vue', () => ({ default: {} }))`) なら動的
// import にも効くことは実測した。ただしそれは SFC のスタブを自前で持つことに
// なり、`MkLightbox` の公開面が変わるたびに追従が要る。ここで見たいのは
// 「何を渡したか」だけなので、読み込みは実体に任せる。
// (factory 無しの automock は実体を読むので、こちらは逆に壊れる)
async function settleLightboxImport(): Promise<void> {
	await Promise.all(
		popupAsyncWithDialog.mock.calls.map((call) => call[0] as Promise<unknown>),
	);
}

const AVATAR_URL = 'https://example.com/avatar.png';

// `home.vue` は `XFiles` 経由で `users/notes` を叩く (**あれは `disableNotes` の
// 外**にあり、props では止められない)。unit test にサーバーは無いので必ず失敗し、
// 下の `settleLightboxImport` で await を挟むとその rejection がテスト実行中に
// 着地して vitest の unhandled error (exit 1) になる。**空配列で答えて黙らせる** —
// ここで見ているのはライトボックスの配線であって API ではない。
//
// **`vi.unstubAllGlobals()` は使わない。** あれは `setup.unit.ts` が張った stub
// (`localStorage` / `AudioContext` など) まで巻き戻すので、この後ろに hook を足した
// 瞬間に壊れる。自分が差し替えたものだけを戻す。
const realFetch = globalThis.fetch;

beforeAll(() => {
	globalThis.fetch = (async () => new Response('[]', {
		status: 200,
		headers: { 'content-type': 'application/json' },
	})) as typeof globalThis.fetch;
});

afterAll(() => {
	globalThis.fetch = realFetch;
});

// `home.vue` のアクティビティのチャート (`XActivity`) は `defineAsyncComponent` で
// 読み込まれる。最初の描画では `narrow` が `null` なので `.sub` 側が描画され、
// `renderHome()` のたびに読み込みが始まる。テストの await の間に読み込みが終わると
// `MkChart` の setup で `initChart` がテーマ未用意のまま
// `currentCompiledTheme!.fg` を読み、unhandled error (exit 1) になる (#3366。CI で
// 不定期に落ち、手元でも 15 回中 1 回再現した)。ここで見ているのはライトボックスの
// 配線なので、チャートは空の部品に差し替える。
//
// **`__esModule: true` を付ける。** 付けないと `defineAsyncComponent` がモジュールの
// オブジェクトそのものを component として扱い、mock が `__isTeleport` の参照で
// throw して全件が落ちる。
vi.mock('@/pages/user/index.activity.vue', async () => {
	const { h } = await import('vue');
	return { __esModule: true, default: { render: () => h('div') } };
});

describe('プロフィールのアイコンを押すと拡大表示が開く', () => {
	const renderHome = (): RenderResult => {
		return render(XHome, {
			props: {
				user: {
					id: 'blobcat',
					name: 'blobcat',
					username: 'blobcat',
					host: null,
					roles: [],
					createdAt: '1970-01-01T00:00:00.000Z',
					fields: [],
					pinnedNotes: [],
					avatarUrl: AVATAR_URL,
					avatarDecorations: [],
				} as unknown as Misskey.entities.UserDetailed,
				disableNotes: true,
			},
			global: { directives, components },
		});
	};

	const getAvatar = (home: RenderResult): HTMLElement => {
		const avatar = home.container.querySelector<HTMLElement>('.avatar');
		assert.exists(avatar, 'プロフィールのアイコンが描画されている');
		return avatar as HTMLElement;
	};

	afterEach(async () => {
		await settleLightboxImport();
		popupAsyncWithDialog.mockClear();
		cleanup();
	});

	// `MkAvatar` の root は span なので、そのままではフォーカスも押下もできない。
	// `role` / `tabindex` は `MkAvatar` が `emits` に持たない属性なので
	// **fallthrough attribute として root に載る** — この Vue の挙動に依存して
	// いるため、実際に載っていることを固定しておく。
	test('アイコンがボタンとして公開されている', () => {
		const avatar = getAvatar(renderHome());
		expect(avatar.getAttribute('role')).toBe('button');
		expect(avatar.getAttribute('tabindex')).toBe('0');
		expect(avatar.getAttribute('aria-label')).toBeTruthy();
	});

	test('クリックでアイコンの画像をライトボックスに渡す', async () => {
		const avatar = getAvatar(renderHome());
		await fireEvent.click(avatar);

		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(1);
		const props = popupAsyncWithDialog.mock.calls[0][1] as {
			contents: { id: string; type: string; url: string; filename?: string }[];
		};
		expect(props.contents).toHaveLength(1);
		expect(props.contents[0].type).toBe('image');
		// **アイコンと同じ URL を渡している**ことが要点。ここが別の URL に
		// なっていると「拡大表示」ではなくなる。
		expect(props.contents[0].url).toBe(AVATAR_URL);
		// 見出しが空欄にならないこと。
		expect(props.contents[0].filename).toBeTruthy();
	});

	test('Enter でも開く', async () => {
		const avatar = getAvatar(renderHome());
		await fireEvent.keyDown(avatar, { key: 'Enter' });
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(1);
	});

	test('Space でも開く', async () => {
		const avatar = getAvatar(renderHome());
		await fireEvent.keyDown(avatar, { key: ' ' });
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(1);
	});

	// `.prevent` が無いと Space でページがスクロールする。`fireEvent` の戻り値では
	// なく**イベントの `defaultPrevented`** を見る (前者は実装によって意味が変わる)。
	test('Enter と Space は既定動作を止める', async () => {
		const avatar = getAvatar(renderHome());
		for (const key of ['Enter', ' ']) {
			const ev = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
			avatar.dispatchEvent(ev);
			expect(ev.defaultPrevented, `key=${key}`).toBe(true);
		}
	});

	// 押しっぱなし (`event.repeat`) や二度押しで二枚開かないこと。chunk の読み込みが
	// 挟まるぶん、初回はフォーカスがライトボックスへ移る前に 2 回目が入りうる。
	test('開いている間は再入しない', async () => {
		const avatar = getAvatar(renderHome());
		await fireEvent.click(avatar);
		await fireEvent.click(avatar);
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(1);
	});

	// 読み込みに失敗したときもガードが解けること。`popupAsyncWithDialog` は
	// `ASYNC_COMP_LOAD_FAIL` を alert してから **rethrow する**ので、握って解除
	// しないと一度の失敗で以後永久に開かなくなる。
	test('読み込みに失敗してもまた開ける', async () => {
		const avatar = getAvatar(renderHome());
		popupAsyncWithDialog.mockRejectedValueOnce(new Error('ASYNC_COMP_LOAD_FAIL'));

		await fireEvent.click(avatar);
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(1);

		await fireEvent.click(avatar);
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(2);
	});

	// 閉じたら再入ガードが解けること。解けないと 2 回目以降が永久に開かない。
	test('閉じた後はまた開ける', async () => {
		const avatar = getAvatar(renderHome());
		await fireEvent.click(avatar);
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(1);

		const events = popupAsyncWithDialog.mock.calls[0][2] as { closed?: () => void };
		expect(events.closed, 'closed ハンドラを渡している').toBeTypeOf('function');
		events.closed?.();

		await fireEvent.click(avatar);
		expect(popupAsyncWithDialog).toHaveBeenCalledTimes(2);
	});
});
