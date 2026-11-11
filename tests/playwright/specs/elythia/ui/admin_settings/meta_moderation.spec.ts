/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の設定の表 (#4066): /admin/moderation。項目ごとに保存の仕方が違う (選ぶとすぐ保存、
// 枠の中の Save で保存)。効き目は登録・投稿・名前の変更・未ログインでの表示で見る。
// 行の後は元の値に戻す (この spec は admin/update-meta を書くので meta の project で流れる)。

import { expect, type Page, test } from '@playwright/test';
import { type AdminRowContext, coversMeta, expectApiError, expectApiOk, type MetaRow, pickSelect, runMetaRow, saveFolder, setSwitch, switchByLabel, fieldByLabel } from '../../../../fixtures/admin_table';
import { callApi } from '../../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername } from '../../../../fixtures/auth';
import { clickButtonByText } from '../../../../fixtures/ui_click';
import { fillFolderTextarea } from '../../../../fixtures/ui_folder';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { pageBody } from '../../../../fixtures/ui_timeline';

const ROUTE = '/admin/moderation';
const REGISTRATION = ['registrationClosed', 'approvalRequiredForSignup', 'disableRegistration'];
const MODE = 'How registrations are accepted';
const RESERVED = `pwres${Date.now().toString(36)}`;

/** Runs `action` and waits for the `admin/update-meta` it sends (the controls that save as soon as they change). */
async function savedBy(page: Page, action: () => Promise<void>): Promise<void> {
  const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/update-meta', { timeout: 15_000 });
  await action();
  expect((await saved).ok(), 'admin/update-meta が失敗した').toBe(true);
}

/** The textarea of the folder whose header is exactly `label`. */
function folderTextarea(page: Page, label: string) {
  return pageBody(page).getByTestId('folder-header').filter({ has: page.getByText(label, { exact: true }) })
    .locator('xpath=ancestor::*[@role="group"][1]').locator('textarea');
}

/** A row that writes `lines` into the textarea of a folder and saves it with the folder's Save. */
function wordsRow(key: string, folder: string, lines: string[], effect: MetaRow['effect']): MetaRow {
  return {
    name: key, keys: [key], route: ROUTE,
    set: async ({ page }) => {
      await fillFolderTextarea(page, folder, lines.join('\n'));
      await saveFolder(page, folder);
    },
    read: (meta) => meta[key],
    expected: lines,
    shows: async ({ page }) => { await expect(folderTextarea(page, folder)).toHaveValue(lines.join('\n')); },
    effect,
  };
}

/** A row that picks a registration mode (the three flags are saved together and restored together). */
function registrationRow(key: string, option: string, confirm: boolean, code: string): MetaRow {
  return {
    name: `registration mode → ${option}`, keys: [key], route: ROUTE, restore: REGISTRATION,
    set: async ({ page }) => {
      await savedBy(page, async () => {
        await (await fieldByLabel(page, MODE)).getByText(option, { exact: true }).click();
        if (confirm) await clickButtonByText(page, 'OK');
      });
    },
    read: (meta) => meta[key],
    expected: true,
    shows: async ({ page }) => {
      await expect((await fieldByLabel(page, MODE)).locator('[role="checkbox"]').filter({ hasText: option })).toHaveAttribute('aria-checked', 'true');
    },
    effect: async ({ request }) => {
      const resp = await callApi(request, 'signup', { username: randomUsername('pwreg'), password: DEFAULT_TEST_PASSWORD });
      const json = await resp.json().catch(() => ({})) as { error?: { code?: string } };
      expect(json.error?.code, `登録が ${code} で拒否されない (status ${resp.status()})`).toBe(code);
    },
  };
}

/** The message of a signup refused by the username checks (they answer `{ message: 'Error: <CODE>' }`). */
async function expectSignupRefused(ctx: AdminRowContext, username: string, code: string): Promise<void> {
  const resp = await callApi(ctx.request, 'signup', { username, password: DEFAULT_TEST_PASSWORD });
  const text = await resp.text();
  expect(text, `ユーザー名 ${username} の登録が ${code} で拒否されない (status ${resp.status()})`).toContain(code);
}

const ROWS: MetaRow[] = [
  registrationRow('disableRegistration', 'Invite only', false, 'INVITATION_CODE_INVALID'),
  registrationRow('approvalRequiredForSignup', 'Approval required', false, 'APPROVAL_REQUIRED'),
  registrationRow('registrationClosed', 'Closed', true, 'REGISTRATION_CLOSED'),
  {
    name: 'signupApplicationForm', keys: ['signupApplicationForm'], route: ROUTE, restore: ['signupApplicationForm', ...REGISTRATION],
    // 申請フォームの枠は承認制のときだけ出るので、先に承認制にしておく
    setup: async ({ request, root }) => { await expectApiOk(request, 'admin/update-meta', { i: root.token, registrationClosed: false, approvalRequiredForSignup: true, disableRegistration: false }); },
    set: async ({ page }) => {
      const group = pageBody(page).getByTestId('folder-header').filter({ has: page.getByText('Application form questions', { exact: true }) }).locator('xpath=ancestor::*[@role="group"][1]');
      await group.getByRole('button', { name: 'Add a question' }).click();
      await (await fieldByLabel(page, 'Question')).locator('input').last().fill('pw fediverse account');
      await savedBy(page, () => group.getByRole('button', { name: 'Save' }).click());
    },
    read: (meta) => (meta.signupApplicationForm as { label: string }[] | undefined)?.at(-1)?.label,
    expected: 'pw fediverse account',
    shows: async ({ page }) => { await expect((await fieldByLabel(page, 'Question')).locator('input').last()).toHaveValue('pw fediverse account'); },
    effect: async ({ request }) => {
      const form = (await expectApiOk(request, 'meta', { detail: true })).signupApplicationForm as { label: string }[];
      expect(form.map((f) => f.label), '/api/meta の申請フォームに項目が出ない').toContain('pw fediverse account');
    },
  },
  {
    name: 'emailRequiredForSignup', keys: ['emailRequiredForSignup'], route: ROUTE,
    set: async ({ page }) => { await savedBy(page, async () => setSwitch(await switchByLabel(page, 'Require email address for sign-up (Recommended)'), true)); },
    read: (meta) => meta.emailRequiredForSignup,
    expected: true,
    shows: async ({ page }) => { await expect(await switchByLabel(page, 'Require email address for sign-up (Recommended)')).toHaveAttribute('data-checked', 'true'); },
    effect: async ({ request }) => {
      const resp = await callApi(request, 'signup', { username: randomUsername('pwmail'), password: DEFAULT_TEST_PASSWORD });
      expect(await resp.text(), 'メールアドレス無しの登録が拒否されない').toContain('emailAddress is required');
    },
  },
  {
    name: 'ugcVisibilityForVisitor → none', keys: ['ugcVisibilityForVisitor'], route: ROUTE,
    set: async ({ page }) => {
      await savedBy(page, async () => pickSelect(page, await fieldByLabel(page, 'Visibility of user-generated content to guests'), 'Everything is private'));
    },
    read: (meta) => meta.ugcVisibilityForVisitor,
    expected: 'none',
    shows: async ({ page }) => { await expect((await fieldByLabel(page, 'Visibility of user-generated content to guests')).locator('[tabindex="0"]').first()).toContainText('Everything is private'); },
    effect: async ({ request, user }) => {
      const note = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw ugc' });
      await expectApiError(request, 'notes/show', { noteId: note.createdNote.id }, 'CONTENT_RESTRICTED_BY_SERVER');
    },
  },
  {
    name: 'minimumUsernameLength', keys: ['minimumUsernameLength'], route: ROUTE,
    set: async ({ page }) => {
      await (await fieldByLabel(page, 'Minimum username length')).locator('input').fill('10');
      await saveFolder(page, 'Minimum username length');
    },
    read: (meta) => meta.minimumUsernameLength,
    expected: 10,
    shows: async ({ page }) => { await expect((await fieldByLabel(page, 'Minimum username length')).locator('input')).toHaveValue('10'); },
    effect: (ctx) => expectSignupRefused(ctx, `pw${Date.now().toString(36).slice(-4)}`, 'USERNAME_TOO_SHORT'),
  },
  // 変異の確かめでは登録が通ってアカウントが残るので、名前を回ごとに変える
  wordsRow('preservedUsernames', 'Reserved usernames', [RESERVED], (ctx) => expectSignupRefused(ctx, RESERVED, 'USED_USERNAME')),
  wordsRow('sensitiveWords', 'Sensitive words', ['pwsensword'], async ({ request, user }) => {
    // 本家と同じく拒否はせず、公開範囲を「ホーム」に下げる
    const note = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw pwsensword', visibility: 'public' });
    expect(note.createdNote.visibility, 'センシティブな語を含む投稿がホームに下がらない').toBe('home');
  }),
  wordsRow('prohibitedWords', 'Prohibited words', ['pwbanword'], ({ request, user }) => expectApiError(request, 'notes/create', { i: user.token, text: 'pw pwbanword' }, 'CONTAINS_PROHIBITED_WORDS')),
  wordsRow('prohibitedWordsForNameOfUser', 'Prohibited words for usernames', ['pwbadname'], ({ request, user }) => expectApiError(request, 'i/update', { i: user.token, name: 'pw pwbadname' }, 'YOUR_NAME_CONTAINS_PROHIBITED_WORDS')),
  wordsRow('hiddenTags', 'Hidden hashtags', ['pwhiddentag'], { skip: 'トレンドの集計 (hashtags/trend) から外す設定で、集計には複数の利用者の投稿と時間が要る。ハッシュタグの検索と表示には効かない (本家も同じ)' }),
  wordsRow('silencedHosts', 'Silenced instances', ['remote.example'], { skip: 'リモートのサーバーの利用者と投稿にだけ効き、このスタックには居ない' }),
  wordsRow('mediaSilencedHosts', 'Media-silenced servers', ['remote.example'], { skip: 'リモートのサーバーの添付と絵文字にだけ効き、このスタックには居ない' }),
  wordsRow('blockedHosts', 'Blocked Instances', ['remote.example'], { skip: 'リモートのサーバーとの連合にだけ効き、このスタックには居ない' }),
];

test.describe('admin: meta (/admin/moderation)', () => {
  test.setTimeout(90_000);
  for (const row of ROWS) {
    test(row.name, { annotation: [...coversRoutes(ROUTE).annotation, ...coversMeta(...row.keys).annotation] }, ({ page, request, baseURL }) => runMetaRow({ page, request, baseURL }, row));
  }
});
