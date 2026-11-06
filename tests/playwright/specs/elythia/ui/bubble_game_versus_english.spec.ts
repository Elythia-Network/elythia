/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 英語の UI で、バブルゲームの対戦 (Elythia 独自、#3231) の画面に日本語の文言が
// 混ざらないことを確かめる (#4047)。以前は対戦の語が en-US に無く、ja-JP に戻って
// 日本語のまま出ていた。
//
// /bubble-game は、対戦の欄に届いている招待まで出した状態で見る (相手から招待を
// 受けておく)。対戦の履歴の画面は、記録の無い利用者で見る (説明と「記録が無い」の文言が出る)。
// 招待は finally で取り消す。

import { expect, test } from '@playwright/test';
import { isTsBackend } from '../../../fixtures/backend';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { JAPANESE } from '../../../fixtures/ui_locale';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, pageBody } from '../../../fixtures/ui_timeline';

test.describe('UI: bubble game versus in English', () => {
  test.skip(isTsBackend, 'バブルゲームの対戦は Elythia 独自 (#3231)');
  test.setTimeout(90_000);

  test('the versus section and the history page show no Japanese', coversRoutes('/bubble-game', '/bubble-game/versus/history/:userId?'), async ({ page, baseURL, request }) => {
    const me = await freshUser(request, 'pwbgen');
    const peer = await freshUser(request, 'pwbgpeer');
    const invitation = await apiOk(request, 'bubble-game/versus/invite', { i: peer.token, userId: me.id, gameMode: 'normal' });
    try {
      await uiSigninAs(page, request, baseURL!, me);

      await gotoRoute(page, baseURL!, '/bubble-game');
      const body = pageBody(page);
      await expect(body).toContainText('Invitations received', { timeout: 20_000 });
      await expect(body).toContainText('Invite to a match');
      await expect(body.getByRole('button', { name: 'Accept' })).toHaveCount(1);
      await expect(body).not.toContainText(JAPANESE);

      await gotoRoute(page, baseURL!, '/bubble-game/versus/history');
      const history = pageBody(page);
      await expect(history).toContainText('No match records', { timeout: 20_000 });
      await expect(history).toContainText('Match records are deleted 30 days after the match ends.');
      await expect(history).not.toContainText(JAPANESE);
    } finally {
      await apiOk(request, 'bubble-game/versus/cancel', { i: peer.token, matchId: invitation.id });
    }
  });
});
