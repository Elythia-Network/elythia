/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// バックアップの取得・検証と、画面から戻す操作 (#3462、#3463) を叩き、応答を /api.json の
// 宣言で照合する (#4102)。処理はバックアップのデーモンが行うので、デーモンを付けた専用の
// スタック (backup-stack/compose.yml) でだけ流す。最後の確定は DB を取得の時点へ入れ替える
// ので、このスタックは流し終えたら捨てる。
//
// 再認証はどの操作にも要り、ここでは 8 回になる。TOTP のコードは同じ値を 2 回使えないので、
// まだ使っていないコードを nextCode で取る (足りなければ次の段まで待つ)。

import { expect, test, type APIRequestContext } from '@playwright/test';
import { callApi } from '../../fixtures/api';
import { callChecked, expectApiResponse, knownDateBug } from '../../fixtures/api_schema';
import { rootFixture } from '../../fixtures/admin_table';
import { withDb } from '../../fixtures/db';
import { isTsBackend } from '../../fixtures/backend';
import { createNote, showNoteRaw } from '../../fixtures/notes';
import { totpAdmin, type TotpAdmin } from '../../fixtures/reauth';

const DAEMON_URL = process.env.BACKUP_DAEMON_URL ?? 'http://backup-daemon:3010';
const SERVICE_TOKEN = process.env.BACKUP_SERVICE_TOKEN ?? '';

interface Job { kind: string; trigger: string; generationId?: string; startedAt: string }
interface JobResult extends Job { finishedAt: string; ok: boolean; stage?: string; error?: string }
interface RestoreStatus { record: { opId: string; kind: string; step: string; generationId: string; target: string; error?: string }; final: boolean }
interface Ticket { opId: string; ticket: string }

/** Reads the daemon's own status (docs/backup.md「制御API」), which needs no re-authentication. */
async function daemonStatus(request: APIRequestContext): Promise<{ lastTake: JobResult | null; lastVerify: JobResult | null }> {
  const resp = await request.get(`${DAEMON_URL}/status`, { headers: { Authorization: `Bearer ${SERVICE_TOKEN}` } });
  expect(resp.status(), await resp.text()).toBe(200);
  return resp.json();
}

/** Waits until the daemon finishes the job that started at `job.startedAt`, and returns its result. */
async function finished(request: APIRequestContext, job: Job, key: 'lastTake' | 'lastVerify'): Promise<JobResult> {
  let result: JobResult | null = null;
  await expect.poll(async () => {
    result = (await daemonStatus(request))[key];
    return result?.startedAt === job.startedAt;
  }, { timeout: 120_000, intervals: [1_000] }).toBe(true);
  return result as unknown as JobResult;
}

/**
 * Waits until the restore operation reaches `step`, reading the progress the
 * way the admin page does (`session` instead of `i`). While the switch holds
 * the maintenance, the server may answer 503, so it only checks the body once
 * the step is reached.
 */
async function reachStep(request: APIRequestContext, token: string, t: Ticket, step: string): Promise<RestoreStatus> {
  const body = { session: token, ...t };
  await expect.poll(async () => {
    const resp = await callApi(request, 'admin/backup/restore/status', body);
    if (resp.status() !== 200) return `${resp.status()}`;
    const st = (await resp.json()) as RestoreStatus;
    // 別の段で終わった (失敗など) なら、待たずに落とす
    if (st.final && st.record.step !== step) throw new Error(`the restore ended at ${st.record.step}: ${st.record.error ?? ''}`);
    return st.record.step;
  }, { timeout: 300_000, intervals: [1_000] }).toBe(step);
  const resp = await callApi(request, 'admin/backup/restore/status', body);
  const text = await resp.text();
  return expectApiResponse<RestoreStatus>(request, 'admin/backup/restore/status', resp, knownDateBug(text, '#4091'));
}

test.describe('admin: take, verify and restore a backup', () => {
  test.skip(isTsBackend, 'バックアップは Elythia 独自');
  // 取得と検証、戻す準備を 2 回、切り替えを待つ。TOTP のコードが尽きれば 30 秒待つこともある
  test.setTimeout(600_000);
  // 途中で落ちたとき、同じスタックで流し直しても前の操作が残っていて通らない。作り直して流す
  test.describe.configure({ retries: 0 });

  test('take, verify, prepare and abort, then prepare and confirm', async ({ request }) => {
    const root = rootFixture();
    const reauth: TotpAdmin = await totpAdmin(request, root, 'admbkr');
    const token = reauth.admin.token;
    const auth = async () => ({ i: token, password: reauth.password, token: await reauth.nextCode() });

    // 取得の前にあったノートは戻した後も残り、取得の後に作ったノートは消える。切り替えが
    // 本当に取得の時点の DB へ入れ替えたことを、これで確かめる
    const before = await createNote(request, token, { text: 'before the backup' });

    const take = await callChecked<{ accepted: boolean; job: Job }>(request, 'admin/backup/take', await auth(), { knownBug: '#4091' });
    expect(take).toMatchObject({ accepted: true, job: { kind: 'take', trigger: 'api' } });
    const taken = await finished(request, take.job, 'lastTake');
    expect(taken, taken.error).toMatchObject({ ok: true });
    const id = taken.generationId as string;
    expect(id).toMatch(/^\d{8}T\d{6}Z$/);

    const after = await createNote(request, token, { text: 'after the backup' });

    const verify = await callChecked<{ accepted: boolean; job: Job }>(request, 'admin/backup/verify', { ...(await auth()), id }, { knownBug: '#4091' });
    expect(verify).toMatchObject({ accepted: true, job: { kind: 'verify', trigger: 'api', generationId: id } });
    const verified = await finished(request, verify.job, 'lastVerify');
    expect(verified, verified.error).toMatchObject({ ok: true, generationId: id });

    // 退避した DB は、まだ無い
    const candidatesResp = await callApi(request, 'admin/backup/restore/candidates', await auth());
    const candidatesText = await candidatesResp.text();
    const candidates = await expectApiResponse<{ database: string; databases: { name: string }[] }>(
      request, 'admin/backup/restore/candidates', candidatesResp, knownDateBug(candidatesText, '#4091'),
    );
    expect(candidates).toEqual({ database: 'misskey', databases: [] });

    // 準備して中止する。作った DB は消え、元の DB のまま動く
    const first = await callChecked<Ticket & { status: RestoreStatus }>(request, 'admin/backup/restore/prepare', { ...(await auth()), id }, { knownBug: '#4091' });
    expect(first.status.record).toMatchObject({ kind: 'restore', generationId: id });
    const ready = await reachStep(request, token, first, 'ready');
    const target = ready.record.target;
    expect(target).toMatch(/^misskey_restore_/);
    await callChecked(request, 'admin/backup/restore/abort', { ...(await auth()), opId: first.opId, ticket: first.ticket });
    await reachStep(request, token, first, 'aborted');
    // 切り替えは DB への接続が 0 になるのを待つので、繋いだら必ず閉じる (withDb が閉じる)
    const left = await withDb((db) => db.query('SELECT 1 FROM pg_database WHERE datname = $1', [target]));
    expect(left.rowCount, 'the abort drops the database it restored into').toBe(0);
    expect((await showNoteRaw(request, token, after.id)).status(), 'the abort keeps the database').toBe(200);

    // 準備して切り替える
    const second = await callChecked<Ticket & { status: RestoreStatus }>(request, 'admin/backup/restore/prepare', { ...(await auth()), id }, { knownBug: '#4091' });
    expect(second.opId).not.toBe(first.opId);
    await reachStep(request, token, second, 'ready');
    await callChecked(request, 'admin/backup/restore/confirm', { ...(await auth()), opId: second.opId, ticket: second.ticket });
    await reachStep(request, token, second, 'done');

    expect((await showNoteRaw(request, token, before.id)).status(), 'the note before the backup is back').toBe(200);
    const gone = await showNoteRaw(request, token, after.id);
    expect(gone.status(), 'the note after the backup is gone').toBe(400);
    expect(((await gone.json()) as { error: { code: string } }).error.code).toBe('NO_SUCH_NOTE');

    // 切り替える前の DB は、退避した DB として一覧に出る
    const keptResp = await callApi(request, 'admin/backup/restore/candidates', await auth());
    const keptText = await keptResp.text();
    const kept = await expectApiResponse<{ database: string; databases: { name: string }[] }>(
      request, 'admin/backup/restore/candidates', keptResp, knownDateBug(keptText, '#4091'),
    );
    expect(kept.databases).toEqual([expect.objectContaining({ name: expect.stringMatching(/^misskey_before_restore_/) })]);
  });
});
