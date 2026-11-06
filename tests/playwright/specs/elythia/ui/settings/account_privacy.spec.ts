/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、/settings/privacy のもの。効き目は、別の新しい
// 利用者から API で見た振る舞いで確かめる。行の利用者は行ごとに新しいので、既定の
// 値 (isLocked=false、publicReactions=true、preventAiLearning=true、chatScope=mutual
// など) から始まる。行の値は既定と逆向きにし、変異の確かめ (設定を変えない) で
// 効き目の確かめが落ちるようにしてある。

import { expect, type Locator, type Page, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { type AccountRow, expectControl, labeledSwitch, runAccountRow, setControl } from '../../../../fixtures/settings_table';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { apiOk, follow, postNote, runTag } from '../../../../fixtures/ui_timeline';

/**
 * The innermost block (MkSelect, MkInput, FormSlot) whose first child is a
 * label div reading exactly `label`. MkSelect has no `<label>` element, so it
 * is found by the text of its label slot.
 */
async function labeled(page: Page, label: string): Promise<Locator> {
  const n = await pageBody(page).evaluate((body, text) => {
    for (const el of Array.from(body.querySelectorAll('[data-pw-labeled]'))) el.removeAttribute('data-pw-labeled');
    // SearchMarker の div も部品を最初の子に持つので、最初の子が操作できる要素を含まない
    // (ラベルだけの div である) ものに絞る
    const hits = Array.from(body.querySelectorAll<HTMLElement>('div')).filter((el) => {
      const first = el.firstElementChild;
      return first?.tagName === 'DIV' && first.querySelector('input, textarea, [tabindex]') == null && (first.textContent ?? '').trim() === text;
    });
    if (hits.length !== 1) return hits.length;
    hits[0].setAttribute('data-pw-labeled', '');
    return 1;
  }, label);
  expect(n, `ラベルが「${label}」の部品 (ちょうど 1 つ)`).toBe(1);
  return pageBody(page).locator('[data-pw-labeled]');
}

/** Sets the switch labeled `label` to `on` (clicks only when it differs). */
async function setSwitch(page: Page, label: string, on: boolean): Promise<void> {
  const toggle = await labeledSwitch(page, label);
  await toggle.scrollIntoViewIfNeeded();
  if ((await toggle.getAttribute('data-checked')) !== String(on)) await toggle.click();
}

async function expectSwitch(page: Page, label: string, on: boolean): Promise<void> {
  await expect(await labeledSwitch(page, label)).toHaveAttribute('data-checked', String(on));
}

async function chooseOption(page: Page, label: string, option: string): Promise<void> {
  await setControl(page, await labeled(page, label), { kind: 'select', label: option });
}

async function expectOption(page: Page, label: string, option: string): Promise<void> {
  await expectControl(await labeled(page, label), { kind: 'select', label: option });
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

const LOCKED = 'Follow requests require approval';
const AUTO_ACCEPT = "Automatically approve follow requests from users you're following";
const LOCAL_APPROVAL = 'Follows from local users';
const REMOTE_APPROVAL = 'Follows from remote users';
const APPROVAL_ACTION = 'Follows from accounts younger than the period';
const CUSTOM_PERIOD = 'Specify a period';
const FOLLOWERS_ONLY_BEFORE = 'Make past notes to be displayed only to followers';
const HIDDEN_BEFORE = 'Make past notes private';
const OLDER_THAN_DATE = 'Notes before the specified date and time';

// 過去のノートを隠す 2 行で使う。「指定の日時より前」は選んだ時点の時刻 (秒) が入り、
// それ以前に作ったノートが対象になる。効き目で作ったノートは対象にならないので、
// set の中で選ぶ前に作っておく。変異の確かめでは set を省くので、effect で作る
const pastNotes: Record<string, string | undefined> = {};

async function postPastNote(row: string, request: Parameters<typeof postNote>[0], user: Parameters<typeof postNote>[1]): Promise<void> {
  pastNotes[row] = await postNote(request, user, runTag('pwpast'));
  // 選んだ時刻は秒に切り捨てられるので、作成の時刻がそれより前になるよう 1 秒以上空ける
  await sleep(1_500);
}

/** A number of seconds close to now: what choosing "before the specified date" stores. */
function isNowInSeconds(v: unknown): boolean {
  return typeof v === 'number' && Math.abs(v - Date.now() / 1000) < 600;
}

test.describe('settings: account (privacy)', () => {
  test.setTimeout(60_000);
  const ROWS: AccountRow[] = [
    {
      name: 'isLocked → true', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, LOCKED, true),
      read: (me) => me.isLocked, expected: true,
      shows: ({ page }) => expectSwitch(page, LOCKED, true),
      effect: async ({ request, user }) => {
        const other = await signupUser(request, randomUsername('pwlock'));
        await apiOk(request, 'following/create', { i: other.token, userId: user.id });
        const shown = await apiOk(request, 'users/show', { i: other.token, userId: user.id });
        expect({ following: shown.isFollowing, pending: shown.hasPendingFollowRequestFromYou }).toEqual({ following: false, pending: true });
      },
    },
    {
      name: 'followApprovalLocalSeconds → 604800', route: '/settings/privacy',
      // 「期間を指定」を選ぶと、既定の 7 日 (followApprovalPeriodFromSeconds) で送る
      set: ({ page }) => chooseOption(page, LOCAL_APPROVAL, CUSTOM_PERIOD),
      read: (me) => me.followApprovalLocalSeconds, expected: 604800,
      shows: ({ page }) => expectOption(page, LOCAL_APPROVAL, CUSTOM_PERIOD),
      effect: async ({ request, user }) => {
        // 作ったばかりのローカルの利用者からのフォローは、期間の内なのでリクエストになる
        const other = await signupUser(request, randomUsername('pwnewf'));
        await apiOk(request, 'following/create', { i: other.token, userId: user.id });
        const shown = await apiOk(request, 'users/show', { i: other.token, userId: user.id });
        expect({ following: shown.isFollowing, pending: shown.hasPendingFollowRequestFromYou }).toEqual({ following: false, pending: true });
      },
    },
    {
      name: 'followApprovalRemoteSeconds → 604800', route: '/settings/privacy',
      set: ({ page }) => chooseOption(page, REMOTE_APPROVAL, CUSTOM_PERIOD),
      read: (me) => me.followApprovalRemoteSeconds, expected: 604800,
      shows: ({ page }) => expectOption(page, REMOTE_APPROVAL, CUSTOM_PERIOD),
      effect: { skip: 'リモートの作りたてのアカウントからのフォローにだけ効く (internal/core/following/follow_approval.go)。この spec のスタックにはリモートの利用者がいない' },
    },
    {
      name: 'followApprovalAction → silentRequest', route: '/settings/privacy',
      set: ({ page }) => chooseOption(page, APPROVAL_ACTION, 'Turn into a follow request without notifying'),
      read: (me) => me.followApprovalAction, expected: 'silentRequest',
      shows: ({ page }) => expectOption(page, APPROVAL_ACTION, 'Turn into a follow request without notifying'),
      effect: async ({ request, user }) => {
        // 止め方は期間の制限が効いているときだけ使われるので、期間を API で入れておく
        await apiOk(request, 'i/update', { i: user.token, followApprovalLocalSeconds: 604800 });
        const other = await signupUser(request, randomUsername('pwsilent'));
        await apiOk(request, 'following/create', { i: other.token, userId: user.id });
        const shown = await apiOk(request, 'users/show', { i: other.token, userId: user.id });
        expect(shown.hasPendingFollowRequestFromYou, 'リクエストにはなる').toBe(true);
        // 通知を出さないことを、後から作る試験の通知 (受信の設定に掛からない) を目印にして見る。
        // 目印が届いた後も少し待ち、リクエストの通知が無いことを確かめる
        await apiOk(request, 'notifications/test-notification', { i: user.token });
        await expect.poll(async () => (await apiOk(request, 'i/notifications', { i: user.token, limit: 100 })).some((n: { type: string }) => n.type === 'test'), { timeout: 10_000 }).toBe(true);
        await sleep(2_000);
        const list = await apiOk(request, 'i/notifications', { i: user.token, limit: 100 }) as { type: string; userId?: string }[];
        expect(list.filter((n) => n.type === 'receiveFollowRequest' && n.userId === other.id)).toEqual([]);
      },
    },
    {
      // 登録した利用者は、本家と同じくオンから始まる (SignupService が true で作る)。オフにする
      name: 'autoAcceptFollowed → false', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, AUTO_ACCEPT, false),
      read: (me) => me.autoAcceptFollowed, expected: false,
      shows: ({ page }) => expectSwitch(page, AUTO_ACCEPT, false),
      effect: async ({ request, user }) => {
        // 承認制にしたうえで、自分がフォローしている相手からのフォローも、自動では成立せずリクエストになる
        await apiOk(request, 'i/update', { i: user.token, isLocked: true });
        const other = await signupUser(request, randomUsername('pwauto'));
        await follow(request, user, other);
        await apiOk(request, 'following/create', { i: other.token, userId: user.id });
        const shown = await apiOk(request, 'users/show', { i: other.token, userId: user.id });
        expect({ following: shown.isFollowing, pending: shown.hasPendingFollowRequestFromYou }).toEqual({ following: false, pending: true });
      },
    },
    {
      name: 'publicReactions → false', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, 'Set reaction history to public', false),
      read: (me) => me.publicReactions, expected: false,
      shows: ({ page }) => expectSwitch(page, 'Set reaction history to public', false),
      effect: async ({ request, user }) => {
        const other = await signupUser(request, randomUsername('pwreact'));
        const resp = await callApi(request, 'users/reactions', { i: other.token, userId: user.id });
        expect(resp.status()).toBe(400);
        expect((await resp.json()).error?.code).toBe('REACTIONS_NOT_PUBLIC');
      },
    },
    {
      name: 'followingVisibility → private', route: '/settings/privacy',
      set: ({ page }) => chooseOption(page, 'Visibility of follows', 'Private'),
      read: (me) => me.followingVisibility, expected: 'private',
      shows: ({ page }) => expectOption(page, 'Visibility of follows', 'Private'),
      effect: async ({ request, user }) => {
        const other = await signupUser(request, randomUsername('pwfing'));
        const resp = await callApi(request, 'users/following', { i: other.token, userId: user.id });
        expect(resp.status()).toBe(400);
        expect((await resp.json()).error?.code).toBe('FORBIDDEN');
      },
    },
    {
      name: 'followersVisibility → private', route: '/settings/privacy',
      set: ({ page }) => chooseOption(page, 'Visibility of followers', 'Private'),
      read: (me) => me.followersVisibility, expected: 'private',
      shows: ({ page }) => expectOption(page, 'Visibility of followers', 'Private'),
      effect: async ({ request, user }) => {
        const other = await signupUser(request, randomUsername('pwfers'));
        const resp = await callApi(request, 'users/followers', { i: other.token, userId: user.id });
        expect(resp.status()).toBe(400);
        expect((await resp.json()).error?.code).toBe('FORBIDDEN');
      },
    },
    {
      name: 'hideOnlineStatus → true', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, 'Hide online status', true),
      read: (me) => me.hideOnlineStatus, expected: true,
      shows: ({ page }) => expectSwitch(page, 'Hide online status', true),
      effect: async ({ request, user }) => {
        // 行の利用者はブラウザで動いた直後なので、隠さなければ online になる
        const other = await signupUser(request, randomUsername('pwonline'));
        const shown = await apiOk(request, 'users/show', { i: other.token, userId: user.id });
        expect(shown.onlineStatus).toBe('unknown');
      },
    },
    {
      name: 'noCrawle → true', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, 'Reject crawler indexing', true),
      read: (me) => me.noCrawle, expected: true,
      shows: ({ page }) => expectSwitch(page, 'Reject crawler indexing', true),
      effect: async ({ request, baseURL, user }) => {
        // サーバーが描くプロフィールの HTML に noindex が入る (internal/server/ssr_meta.go の userHead)
        await expect.poll(async () => (await (await request.get(`${baseURL}/@${user.username}`)).text()).includes('<meta name="robots" content="noindex">')).toBe(true);
      },
    },
    {
      name: 'preventAiLearning → false', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, 'Reject usage in Machine Learning (Generative AI)', false),
      read: (me) => me.preventAiLearning, expected: false,
      shows: ({ page }) => expectSwitch(page, 'Reject usage in Machine Learning (Generative AI)', false),
      effect: async ({ request, baseURL, user }) => {
        // 既定は拒否 (true) で、HTML の meta と X-Robots-Tag に noai が入る。許可するとどちらからも消える
        await expect.poll(async () => {
          const resp = await request.get(`${baseURL}/@${user.username}`);
          const html = await resp.text();
          return { meta: html.includes('<meta name="robots" content="noai">'), header: (resp.headers()['x-robots-tag'] ?? '').includes('noai') };
        }).toEqual({ meta: false, header: false });
      },
    },
    {
      name: 'isExplorable → false', route: '/settings/privacy',
      set: ({ page }) => setSwitch(page, 'Make account visible in "Explore"', false),
      read: (me) => me.isExplorable, expected: false,
      shows: ({ page }) => expectSwitch(page, 'Make account visible in "Explore"', false),
      effect: async ({ request, user }) => {
        // 新しい順で取り、直前に作った利用者 (他方) が出ることを、一覧が空振りしていない目印にする
        const other = await signupUser(request, randomUsername('pwexpl'));
        const list = await apiOk(request, 'users', { i: other.token, origin: 'local', sort: '+createdAt', state: 'all', limit: 100 }) as { id: string }[];
        const ids = list.map((u) => u.id);
        expect(ids, '目印の利用者が一覧に出ない').toContain(other.id);
        expect(ids).not.toContain(user.id);
      },
    },
    {
      name: 'chatScope → everyone', route: '/settings/privacy',
      set: ({ page }) => chooseOption(page, 'Who to allow chatting with', 'Everyone'),
      read: (me) => me.chatScope, expected: 'everyone',
      shows: ({ page }) => expectOption(page, 'Who to allow chatting with', 'Everyone'),
      effect: async ({ request, user }) => {
        // 既定は相互フォローだけ。誰でもにすると、フォロー関係の無い相手からも送れる
        const other = await signupUser(request, randomUsername('pwchat'));
        await apiOk(request, 'chat/messages/create-to-user', { i: other.token, toUserId: user.id, text: runTag('pwchat') });
      },
    },
    {
      name: 'requireSigninToViewContents → true', route: '/settings/privacy',
      set: async ({ page }) => {
        await (await labeledSwitch(page, 'Require sign-in to view contents')).click();
        // 連合が有効なサーバーでは、オンにするときに確認が出る (privacy.vue の update_requireSigninToViewContents)
        const ok = page.getByTestId('modal-dialog-ok');
        if (await ok.waitFor({ state: 'visible', timeout: 5_000 }).then(() => true, () => false)) await ok.click();
      },
      read: (me) => me.requireSigninToViewContents ?? false, expected: true,
      shows: ({ page }) => expectSwitch(page, 'Require sign-in to view contents', true),
      effect: async ({ request, user }) => {
        const noteId = await postNote(request, user, runTag('pwsignin'));
        const resp = await callApi(request, 'notes/show', { noteId });
        expect(resp.status()).toBe(400);
        expect((await resp.json()).error?.code).toBe('CONTENT_RESTRICTED_BY_USER');
      },
    },
    {
      name: 'makeNotesFollowersOnlyBefore → absolute', route: '/settings/privacy',
      set: async ({ page, request, user }) => {
        await postPastNote('followersOnly', request, user);
        await chooseOption(page, FOLLOWERS_ONLY_BEFORE, OLDER_THAN_DATE);
      },
      read: (me) => isNowInSeconds(me.makeNotesFollowersOnlyBefore), expected: true,
      shows: ({ page }) => expectOption(page, FOLLOWERS_ONLY_BEFORE, OLDER_THAN_DATE),
      effect: async ({ request, user }) => {
        const noteId = pastNotes.followersOnly ?? await postNote(request, user, runTag('pwpast'));
        // フォロワーでない利用者には、設定の時刻より前の公開ノートがフォロワー限定に下がって隠れる
        const other = await signupUser(request, randomUsername('pwfonly'));
        const shown = await apiOk(request, 'notes/show', { i: other.token, noteId });
        expect(shown.isHidden).toBe(true);
      },
    },
    {
      name: 'makeNotesHiddenBefore → absolute', route: '/settings/privacy',
      set: async ({ page, request, user }) => {
        await postPastNote('hidden', request, user);
        await chooseOption(page, HIDDEN_BEFORE, OLDER_THAN_DATE);
      },
      read: (me) => isNowInSeconds(me.makeNotesHiddenBefore), expected: true,
      shows: ({ page }) => expectOption(page, HIDDEN_BEFORE, OLDER_THAN_DATE),
      effect: async ({ request, user }) => {
        const noteId = pastNotes.hidden ?? await postNote(request, user, runTag('pwpast'));
        // フォロワーにも隠れる。フォロワー限定 (前の行) と区別するため、フォローしてから見る
        const other = await signupUser(request, randomUsername('pwhide'));
        await follow(request, other, user);
        const shown = await apiOk(request, 'notes/show', { i: other.token, noteId });
        expect(shown.isHidden).toBe(true);
      },
    },
  ];
  for (const row of ROWS) test(row.name, coversRoutes('/settings/privacy'), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
