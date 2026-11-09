/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 利用者の側の独自の API (自分のロールの割り当て、自分の Play と、いいねした Play) を
// 叩き、応答を /api.json の宣言で照合する (#4093)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { rootFixture } from '../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

test.describe('account: the Elythia endpoints answer as declared', () => {
  test.skip(isTsBackend, 'ここで叩く口は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('my assignment of a public role', async ({ request }) => {
    const root = rootFixture();
    const user = await signupUser(request, randomUsername('accra'));
    const created = await callApi(request, 'admin/roles/create', {
      i: root.token, name: `pw-my-assignment-${Date.now()}`, description: '', color: null, iconUrl: null,
      target: 'manual', condFormula: {}, isPublic: true, isExplorable: false,
      isAdministrator: false, isModerator: false, asBadge: false,
      canEditMembersByModerator: false, displayOrder: 0, policies: {},
    });
    expect(created.status(), await created.text()).toBe(200);
    const roleId = ((await created.json()) as { id: string }).id;
    try {
      expect(await callChecked(request, 'roles/assignment-show', { i: user.token, roleId })).toMatchObject({ assigned: false, role: { id: roleId } });
      expect((await callApi(request, 'admin/roles/assign', { i: root.token, roleId, userId: user.id })).status()).toBe(204);
      expect(await callChecked(request, 'roles/assignment-show', { i: user.token, roleId })).toMatchObject({ assigned: true, role: { id: roleId } });
    } finally {
      expect((await callApi(request, 'admin/roles/delete', { i: root.token, roleId })).status()).toBe(204);
    }
  });

  test('my plays, and the plays I liked', async ({ request }) => {
    const author = await signupUser(request, randomUsername('accfa'));
    const fan = await signupUser(request, randomUsername('accff'));
    const resp = await callApi(request, 'flash/create', { i: author.token, title: 'pw play', summary: '', script: '/// @ 1.0.0', permissions: [], visibility: 'public' });
    expect(resp.status(), await resp.text()).toBe(200);
    const flash = (await resp.json()) as { id: string };
    try {
      expect((await callChecked<{ id: string }[]>(request, 'i/flashs', { i: author.token })).map((f) => f.id)).toEqual([flash.id]);
      expect((await callApi(request, 'flash/like', { i: fan.token, flashId: flash.id })).status()).toBe(204);
      const liked = await callChecked<{ flash: { id: string } }[]>(request, 'i/flashs/likes', { i: fan.token });
      expect(liked.map((l) => l.flash.id)).toEqual([flash.id]);
    } finally {
      // public の Play は「みつける」に出るので残さない
      expect((await callApi(request, 'flash/delete', { i: author.token, flashId: flash.id })).status()).toBe(204);
    }
  });
});
