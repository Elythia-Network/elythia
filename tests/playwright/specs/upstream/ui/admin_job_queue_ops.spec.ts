/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ジョブキューの画面の操作 (#4072)。ジョブは、届かない URL (.invalid) への webhook を作って投稿し、
// webhook のキューのジョブとして作る。ジョブのデータには投稿が入るので、投稿の本文の目印で見分け、
// 画面でそのジョブだけを消す。キューの一時停止と再開も webhook のキューで行い、終わったら必ず
// 再開しておく (キューは spec 全体で共有するため)。

import { expect, test } from '@playwright/test';
import { cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { pageBody } from '../../../fixtures/route_check';
import { clickButtonContainingText } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, postNote } from '../../../fixtures/ui_timeline';

type Job = { id: string; data: unknown };

test.describe('UI: admin job queue', () => {
  test.setTimeout(90_000);

  test('remove a job of the webhook queue', coversRoutes('/admin/job-queue'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const me = await freshUser(request, 'pwjob');
    const url = `https://pw-job-${Date.now().toString(36)}.invalid/hook`;
    await apiOk(request, 'i/webhooks/create', { i: me.token, name: 'pw job', url, secret: 'pw-secret', on: ['note'] });
    const marker = `pw-job-marker-${Date.now().toString(36)}`;
    await postNote(request, me, marker);
    const findJob = async () => (await expectApiOk(request, 'admin/queue/jobs', { i: root.token, queue: 'webhook', state: ['delayed'], limit: 100 }) as Job[])
      .find((j) => JSON.stringify(j.data).includes(marker));
    let job: Job | undefined;
    await expect.poll(async () => (job = await findJob())?.id, { message: 'webhook のジョブができない (前提)', timeout: 20_000 }).toBeTruthy();
    try {
      // 消した後に照会が 404 になることで見るので、消す前は照会が通ることを確かめておく (対照)
      expect((await callApi(request, 'admin/queue/show-job', { i: root.token, queue: 'webhook', jobId: job!.id })).status(), 'ジョブを照会できない (前提)').toBe(200);
      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, '/admin/job-queue');
      await page.locator('button').filter({ hasText: /^\s*webhook\s*$/ }).first().click();
      // 一覧には終わったジョブも並ぶので、状態のタブで「Delayed」に絞る (届かない先へのジョブは、
      // 再試行を待つ間この状態になる)
      const delayed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/queue/jobs' && JSON.stringify(r.request().postDataJSON()?.state) === '["delayed"]', { timeout: 15_000 });
      await pageBody(page).locator('button').filter({ hasText: /^\s*Delayed\s*$/ }).click();
      expect((await delayed).ok(), 'admin/queue/jobs が失敗した').toBe(true);
      // 見出しは「#<id><ジョブの名前>」(ジョブの名前は webhook:user)。上の概要の枠の見出し (#1:6379
      // など) にも「#」が出るので、id の直後に webhook が続く形で絞る
      const row = page.locator('[data-testid="folder-header"]').filter({ hasText: new RegExp(`^\\s*#${job!.id}\\s*webhook`) });
      await expect(row, '一覧にジョブが出ない').toHaveCount(1, { timeout: 20_000 });
      await row.click();
      // 外側の枠のフッターにも「Remove jobs」があるので、開いた行の枠の中の「Remove」に絞る
      const rowFolder = row.locator('xpath=ancestor::*[@role="group"][1]');
      await rowFolder.locator('button').filter({ hasText: /^\s*Remove\s*$/ }).click();
      const removed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/queue/remove-job', { timeout: 15_000 });
      await page.getByTestId('modal-dialog-ok').click();
      expect((await removed).ok(), 'admin/queue/remove-job が失敗した').toBe(true);
      // 遅延の一覧から外れるだけなら、再試行で動き出しても起きるので、ジョブそのものが無くなったことを見る
      await expect.poll(async () => (await callApi(request, 'admin/queue/show-job', { i: root.token, queue: 'webhook', jobId: job!.id })).status(), { message: '消したジョブが残る' }).toBe(404);
    } finally {
      if (job !== undefined) await callApi(request, 'admin/queue/remove-job', { i: root.token, queue: 'webhook', jobId: job.id });
    }
  });

  test('pause and resume the webhook queue', coversRoutes('/admin/job-queue'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const paused = async () => ((await expectApiOk(request, 'admin/queue/queues', { i: root.token }) as { name: string; isPaused: boolean }[]).find((q) => q.name === 'webhook'))?.isPaused;
    expect(await paused(), 'webhook のキューが止まっている (前提)').toBe(false);
    try {
      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, '/admin/job-queue');
      await page.locator('button').filter({ hasText: /^\s*webhook\s*$/ }).first().click();
      await clickButtonContainingText(page, 'Pause queue');
      const pausedResp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/queue/pause', { timeout: 15_000 });
      await page.getByTestId('modal-dialog-ok').click();
      expect((await pausedResp).ok(), 'admin/queue/pause が失敗した').toBe(true);
      await expect.poll(paused, { message: '一時停止にならない' }).toBe(true);

      const resumed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/queue/resume', { timeout: 15_000 });
      await clickButtonContainingText(page, 'Resume queue');
      expect((await resumed).ok(), 'admin/queue/resume が失敗した').toBe(true);
      await expect.poll(paused, { message: '再開できない' }).toBe(false);
    } finally {
      await cleanupApi(request, 'admin/queue/resume', { i: root.token, queue: 'webhook' });
    }
  });
});
