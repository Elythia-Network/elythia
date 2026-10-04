/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterAll, beforeAll, describe, expect, test, vi } from 'vitest';

const reconnect = vi.fn();
const heartbeat = vi.fn();

vi.mock('@/i.js', () => ({ $i: { token: 'TOKEN' } }));
vi.mock('@@/js/config.js', () => ({ wsOrigin: 'wss://misskey.test' }));
vi.mock('misskey-js', () => ({
	Stream: class {
		public reconnect = reconnect;
		public heartbeat = heartbeat;
	},
}));

function setVisibilityState(state: 'visible' | 'hidden'): void {
	Object.defineProperty(window.document, 'visibilityState', {
		configurable: true,
		get: () => state,
	});
}

// stream.ts はモジュールシングルトンなので、「起動時点で hidden」の検査は
// 他のケースと同じファイルに置けない (useStream() を呼び直せない)。
// vitest はテストファイル単位で隔離するので、ここだけ別ファイルにしてある。
describe('useStream (hidden のまま起動した場合)', () => {
	beforeAll(() => {
		vi.useFakeTimers();
	});

	afterAll(() => {
		vi.useRealTimers();
	});

	test('隠れた状態で起動しても、復帰時に張り直す', async () => {
		// バックグラウンドタブとして復元されたセッションや、裏で起動した PWA。
		// hidden への遷移を観測できないので、useStream() の側で初期化しないと
		// 何時間放置されても復帰時に張り直さない。
		setVisibilityState('hidden');

		const { useStream } = await import('@/stream.js');
		useStream();

		vi.setSystemTime(Date.now() + (20 * 1000));
		setVisibilityState('visible');
		window.document.dispatchEvent(new Event('visibilitychange'));

		expect(reconnect).toHaveBeenCalledTimes(1);
	});
});
