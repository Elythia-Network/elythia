/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { getUsers } from '@/plugin-api.js';

const mocks = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi: mocks.api }));
vi.mock('@/i.js', () => ({ $i: null }));
// UIの起動・ルーター・ネットワーク初期化を行わず、実際のgetUsersだけを検証する。
vi.mock('@/components/MkInput.vue', () => ({ default: {} }));
vi.mock('@/components/MkButton.vue', () => ({ default: {} }));
vi.mock('@/components/MkFolder.vue', () => ({ default: {} }));
vi.mock('@/components/MkSelect.vue', () => ({ default: {} }));
vi.mock('@/components/MkSwitch.vue', () => ({ default: {} }));
vi.mock('@/components/global/MkLoading.vue', () => ({ default: {} }));
vi.mock('@/components/global/MkAvatar.vue', () => ({ default: {} }));
vi.mock('@/components/global/MkUserName.vue', () => ({ default: {} }));
vi.mock('@/components/global/MkTime.vue', () => ({ default: {} }));
vi.mock('@/components/global/PageWithHeader.vue', () => ({ default: {} }));
vi.mock('@/composables/use-mkselect.js', () => ({ useMkSelect: vi.fn() }));
vi.mock('@/page.js', () => ({ definePage: vi.fn() }));

beforeEach(() => { mocks.api.mockReset(); });

describe('プラグインの公開ユーザー取得', () => {
	it('空の一覧ではAPIを呼ばない', async () => {
		expect(await getUsers([])).toEqual([]);
		expect(mocks.api).not.toHaveBeenCalled();
	});

	it('重複を除き100人ずつ取得して結果をまとめる', async () => {
		mocks.api.mockImplementation(async (_endpoint: string, params: { userIds: string[] }) => params.userIds.map(id => ({ id })));
		const ids = Array.from({ length: 101 }, (_, index) => `u${index}`);
		expect(await getUsers([...ids, ids[0]])).toEqual(ids.map(id => ({ id })));
		expect(mocks.api).toHaveBeenCalledTimes(2);
		expect(mocks.api).toHaveBeenNthCalledWith(1, 'users/show', { userIds: ids.slice(0, 100) });
		expect(mocks.api).toHaveBeenNthCalledWith(2, 'users/show', { userIds: ids.slice(100) });
	});

	it('APIの失敗を呼び出し元へ伝える', async () => {
		const error = new Error('unavailable');
		mocks.api.mockRejectedValue(error);
		await expect(getUsers(['u1'])).rejects.toBe(error);
	});
});
