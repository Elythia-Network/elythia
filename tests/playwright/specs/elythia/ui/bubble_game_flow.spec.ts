/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// バブルゲームの画面の操作を通す (#4072)。一人で遊ぶときはモード (#3194) と物理 (#3216) を選んで
// 始め、選んだ組のモードで盤面が出ることを見る。対戦 (#3231) は、届いた招待の辞退と承諾、
// ロビーでの準備完了とその取り消し、退出、こちらからの招待と取り消し、対戦中に開き直したときの
// 投了、終わった対戦の記録の公開とリプレイ (#3232) を画面で行う。
//
// 準備完了は API ではなく対戦のチャンネルで送るので、画面の操作の結果は bubble-game/versus/show で
// 見る。辞退と取り消しをした対戦は消えるので、show が見つからないと返すことで確かめる。

import { type APIRequestContext, expect, test } from '@playwright/test';
import { pickSelect, setSwitch } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { type Principal } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { playVersusToEnd, startVersus } from '../../../fixtures/route_seed';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser } from '../../../fixtures/ui_timeline';

type Match = { status: string; gameMode: string; user1Ready: boolean; user2Ready: boolean };

/** The match as `user` sees it, or null when it no longer exists (NO_SUCH_MATCH). Other errors throw. */
async function showMatch(request: APIRequestContext, user: Principal, matchId: string): Promise<Match | null> {
  const r = await callApi(request, 'bubble-game/versus/show', { i: user.token, matchId });
  if (r.ok()) return await r.json() as Match;
  const code = (await r.json().catch(() => ({})) as { error?: { code?: string } }).error?.code;
  if (code === 'NO_SUCH_MATCH') return null;
  throw new Error(`bubble-game/versus/show failed: ${r.status()} ${code}`);
}

test.describe('UI: bubble game', () => {
  test.skip(isTsBackend, 'モードと物理の選択、対戦は Elythia 独自');
  test.setTimeout(90_000);

  for (const [mode, physics, expected] of [['SQUARE', 'FRICTION', 'square-friction'], ['SPACE', null, 'space']] as const) {
    test(`start ${expected}`, coversRoutes('/bubble-game'), async ({ page, request, baseURL }) => {
      const me = await freshUser(request, 'pwbgs');
      await uiSigninAs(page, request, baseURL!, me);
      await gotoRoute(page, baseURL!, '/bubble-game');
      await pickSelect(page, page.getByTestId('bubble-game-mode'), mode);
      if (physics === null) {
        // SPACE は物理を選ばせない
        await expect(page.getByTestId('bubble-game-physics')).toHaveCount(0);
      } else {
        await pickSelect(page, page.getByTestId('bubble-game-physics'), physics);
      }
      await page.getByTestId('bubble-game-start').click();
      await expect(page.getByTestId('bubble-game-playing')).toHaveAttribute('data-game-mode', expected);
    });
  }

  test('decline an invitation', coversRoutes('/bubble-game'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwbgd');
    const peer = await freshUser(request, 'pwbgp');
    const { id } = await apiOk(request, 'bubble-game/versus/invite', { i: peer.token, userId: me.id, gameMode: 'normal' }) as { id: string };
    try {
      await uiSigninAs(page, request, baseURL!, me);
      await gotoRoute(page, baseURL!, '/bubble-game');
      const invitation = page.locator(`[data-testid="versus-invitation"][data-match-id="${id}"]`);
      await invitation.getByTestId('versus-invitation-decline').click();
      await expect(invitation, '辞退した招待が一覧に残る').toHaveCount(0);
      await expect.poll(() => showMatch(request, peer, id), { message: '辞退しても対戦が残る' }).toBeNull();
    } finally {
      await callApi(request, 'bubble-game/versus/cancel', { i: peer.token, matchId: id });
    }
  });

  test('accept an invitation, get ready, cancel it and leave', coversRoutes('/bubble-game', '/bubble-game/versus/:matchId'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwbga');
    const peer = await freshUser(request, 'pwbgp');
    const { id } = await apiOk(request, 'bubble-game/versus/invite', { i: peer.token, userId: me.id, gameMode: 'normal' }) as { id: string };
    try {
      await uiSigninAs(page, request, baseURL!, me);
      await gotoRoute(page, baseURL!, '/bubble-game');
      await page.locator(`[data-testid="versus-invitation"][data-match-id="${id}"]`).getByTestId('versus-invitation-accept').click();
      await expect(page.getByTestId('versus-lobby')).toHaveAttribute('data-status', 'accepted', { timeout: 15_000 });

      // 招待された側は user2
      await page.getByTestId('versus-ready').click();
      await expect.poll(async () => (await showMatch(request, me, id))?.user2Ready, { message: '準備完了が届かない' }).toBe(true);
      await page.getByTestId('versus-not-ready').click();
      await expect.poll(async () => (await showMatch(request, me, id))?.user2Ready, { message: '準備完了を取り消せない' }).toBe(false);

      await page.getByTestId('versus-leave').click();
      await expect(page).toHaveURL(/\/bubble-game$/);
      await expect.poll(() => showMatch(request, peer, id), { message: '退出しても対戦が残る' }).toBeNull();
    } finally {
      await callApi(request, 'bubble-game/versus/cancel', { i: peer.token, matchId: id });
    }
  });

  test('invite a user and cancel the invitation', coversRoutes('/bubble-game', '/bubble-game/versus/:matchId'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwbgi');
    const peer = await freshUser(request, 'pwbgp');
    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/bubble-game');
    await page.getByTestId('versus-invite').click();
    await page.getByTestId('user-select-username').locator('input').fill(peer.username);
    await page.locator(`[data-testid="user-select-user"][data-user-id="${peer.id}"]`).click();
    const invited = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/bubble-game/versus/invite', { timeout: 15_000 });
    await page.getByTestId('modal-window-ok').click();
    const resp = await invited;
    expect(resp.ok(), 'bubble-game/versus/invite が失敗した').toBe(true);
    const { id } = await resp.json() as { id: string };
    try {
      await expect(page.getByTestId('versus-lobby')).toHaveAttribute('data-status', 'invited', { timeout: 15_000 });
      expect((await showMatch(request, peer, id))?.gameMode, '選んでいたモードで招待されない').toBe('normal');
      await page.getByTestId('versus-cancel').click();
      await expect(page).toHaveURL(/\/bubble-game$/);
      await expect.poll(() => showMatch(request, peer, id), { message: '取り消しても招待が残る' }).toBeNull();
    } finally {
      await callApi(request, 'bubble-game/versus/cancel', { i: me.token, matchId: id });
    }
  });

  test('surrender a match opened again while playing', coversRoutes('/bubble-game/versus/:matchId'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwbgx');
    const peer = await freshUser(request, 'pwbgp');
    // 盤面は始めた端末にしか無いので、始まった対戦を開き直すと投了だけができる
    const id = await startVersus(request, me, peer);
    try {
      await uiSigninAs(page, request, baseURL!, me);
      await gotoRoute(page, baseURL!, `/bubble-game/versus/${id}`);
      await expect(page.getByTestId('versus-lobby')).toHaveAttribute('data-status', 'playing', { timeout: 15_000 });
      await page.getByTestId('versus-surrender').click();
      const reported = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/bubble-game/versus/report', { timeout: 15_000 });
      await page.getByTestId('modal-dialog-ok').click();
      expect((await reported).ok(), 'bubble-game/versus/report が失敗した').toBe(true);
      // 招待した側は user1
      const match = await apiOk(request, 'bubble-game/versus/show', { i: me.token, matchId: id }) as { user1Result: { reason: string } | null };
      expect(match.user1Result?.reason, '投了が記録されない').toBe('surrender');
    } finally {
      // 相手の報告で対戦を終わらせる (残すと時間切れまで playing のまま)
      await callApi(request, 'bubble-game/versus/report', { i: peer.token, matchId: id, score: 0, frame: 0, reason: 'opponentEnded', logs: [] });
    }
  });

  test('make a match record public and open its replay', coversRoutes('/bubble-game/versus/history/:userId?', '/bubble-game/versus/replay/:matchId'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwbgh');
    const peer = await freshUser(request, 'pwbgp');
    const id = await playVersusToEnd(request, me, peer);
    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/bubble-game/versus/history');
    const toggle = page.locator(`[data-testid="versus-record"][data-match-id="${id}"]`).getByTestId('versus-record-public').locator('[data-checked]');
    const set = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/bubble-game/versus/set-public', { timeout: 15_000 });
    await setSwitch(toggle, true);
    expect((await set).ok(), 'bubble-game/versus/set-public が失敗した').toBe(true);
    const history = await apiOk(request, 'bubble-game/versus/history', { i: me.token }) as { id: string; user1Public: boolean }[];
    expect(history.find((r) => r.id === id)?.user1Public, '公開の切り替えが保存されない').toBe(true);

    await page.locator(`[data-testid="versus-record"][data-match-id="${id}"]`).getByTestId('versus-record-replay').click();
    await expect(page.getByTestId('versus-replay'), 'リプレイが開かない').toHaveAttribute('data-match-id', id, { timeout: 15_000 });
  });
});
