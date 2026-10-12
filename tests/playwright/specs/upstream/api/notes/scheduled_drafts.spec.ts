/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #1045 Phase 2-E: scheduled note (= note_draft with isActuallyScheduled=true)
// の drop-in 互換 e2e。upstream Misskey TS frontend が叩く /api/notes/drafts/*
// endpoint shape を mk-go が返すことを seal する。
//
// 本 spec の scope:
//   - drafts/create with scheduledAt + isActuallyScheduled=true の正常 200
//   - failure 経路 (= SCHEDULED_AT_REQUIRED / SCHEDULED_AT_MUST_BE_IN_FUTURE)
//   - drafts/update での scheduledAt 変更
//   - drafts/delete (= clearSchedule 経路、API shape 確認)
//
// scope 外 (= Phase 2-E-2 別 PR 候補):
//   - 実 publish 待ち e2e (= scheduledAt 経過 → note 公開を Playwright 内で
//     wait + 検証)、time 依存で重い + CI flaky の懸念で本 PR では除外。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

// upstream は SCHEDULED_AT_* に endpoint 固有の id を割り当てる
// (notes/drafts/create.ts と update.ts で別値)。本 spec は create 経路のみを
// 叩くので create 側の id を使う。
// 旧値 (94a89a43… / b34d0c1b…) は NoteDraftService 内部の IdentifiableError id
// で、endpoint 側の switch で endpoint 固有 id に翻訳される前のもの。
const UUID_SCHEDULED_AT_REQUIRED = '15e28a55-e74c-4d65-89b7-8880cdaaa87d';
const UUID_SCHEDULED_AT_MUST_BE_IN_FUTURE = 'e4bed6c9-017e-4934-aed0-01c22cc60ec1';

test.describe('notes: scheduled drafts', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('creates a scheduled draft with future scheduledAt', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sCre'));
    // 1 hour in the future なら capability check / future validation を通る
    const scheduledAt = Date.now() + 60 * 60 * 1000;

    const resp = await callApi(request, 'notes/drafts/create', {
      i: me.token,
      text: 'scheduled hello',
      visibility: 'public',
      scheduledAt,
      isActuallyScheduled: true,
    });

    expect(resp.status()).toBe(200);
    const body = await resp.json();
    // upstream notes/drafts/create の res は { createdDraft: NoteDraft }。
    expect(body.createdDraft?.id).toBeTruthy();
    expect(body.createdDraft?.text).toBe('scheduled hello');
    expect(body.createdDraft?.visibility).toBe('public');
  });

  test('rejects scheduledAt missing when isActuallyScheduled=true', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sReq'));

    const resp = await callApi(request, 'notes/drafts/create', {
      i: me.token,
      text: 'no scheduled at',
      isActuallyScheduled: true,
    });

    expect(resp.status()).toBe(400);
    const body = await resp.json();
    expect(body.error?.code).toBe('SCHEDULED_AT_REQUIRED');
    expect(body.error?.id).toBe(UUID_SCHEDULED_AT_REQUIRED);
  });

  test('rejects past scheduledAt', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sPast'));
    const pastAt = Date.now() - 60 * 60 * 1000;

    const resp = await callApi(request, 'notes/drafts/create', {
      i: me.token,
      text: 'too late',
      scheduledAt: pastAt,
      isActuallyScheduled: true,
    });

    expect(resp.status()).toBe(400);
    const body = await resp.json();
    expect(body.error?.code).toBe('SCHEDULED_AT_MUST_BE_IN_FUTURE');
    expect(body.error?.id).toBe(UUID_SCHEDULED_AT_MUST_BE_IN_FUTURE);
  });

  test('isActuallyScheduled=false keeps scheduledAt as draft metadata only', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sFlag'));
    // isActuallyScheduled を立てなければ scheduledAt が指定されても validation
    // が走らず draft 保存のみ (= "下書きとしての希望時刻" 状態)。
    const scheduledAt = Date.now() + 60 * 60 * 1000;

    const resp = await callApi(request, 'notes/drafts/create', {
      i: me.token,
      text: 'flagged off',
      scheduledAt,
      isActuallyScheduled: false,
    });

    expect(resp.status()).toBe(200);
  });

  test('updates a scheduled draft scheduledAt and returns updated draft', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sUpd'));
    const initial = Date.now() + 60 * 60 * 1000;
    const created = await callApi(request, 'notes/drafts/create', {
      i: me.token,
      text: 'reschedule me',
      scheduledAt: initial,
      isActuallyScheduled: true,
    });
    expect(created.status()).toBe(200);
    const draftBody = await created.json();
    const draftId = draftBody.createdDraft.id;

    const updated = Date.now() + 2 * 60 * 60 * 1000;
    const updResp = await callApi(request, 'notes/drafts/update', {
      i: me.token,
      draftId,
      scheduledAt: updated,
      isActuallyScheduled: true,
    });
    expect(updResp.status()).toBe(200);
  });

  test('deletes a scheduled draft (clearSchedule path)', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sDel'));
    const scheduledAt = Date.now() + 60 * 60 * 1000;
    const created = await callApi(request, 'notes/drafts/create', {
      i: me.token,
      text: 'will be removed',
      scheduledAt,
      isActuallyScheduled: true,
    });
    expect(created.status()).toBe(200);
    const draftId = (await created.json()).createdDraft.id;

    const delResp = await callApi(request, 'notes/drafts/delete', {
      i: me.token,
      draftId,
    });
    // upstream は 204 No Content を返す。
    expect(delResp.status()).toBe(204);
  });
});
