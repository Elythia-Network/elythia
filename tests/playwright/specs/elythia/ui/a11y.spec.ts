/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 画面のアクセシビリティを axe-core で確かめる (#4082)。WCAG 2.1 の A / AA の規則で各画面を検査し、
// 画面ごとに出る規則の組を基準 (a11y-baseline.json) と比べる。
//
//   - 基準に無い規則が出たら落とす (新しい違反を持ち込まない)
//   - 基準にある規則が出なくなったら落とす (直した分を基準から消して固定する)
//   - 名前の無いボタン (button-name) は要素の単位で見る。基準の buttonNameAllowed に書いた印 (data-testid か、
//     すべてのアイコンの ti-* を + でつないだもの) のボタンだけを許す。規則の単位だけだと、残したボタンのせいで全画面に規則が残り、
//     直したボタン (ノートのフッターなど) が戻っても落ちないため。使われていない印は落とす
//
// 要素の数では比べない。タイムラインや一覧の中身は他の spec が作った投稿や利用者で変わり、同じ画面でも
// 数が回ごとに変わるため。数が変わるのと同じ理由で、中身の大半が他の spec の作ったもので埋まる画面
// (未ログインのトップ・みつける・チャンネル) は対象に入れない。本家の配色などで全画面に出うる規則
// (color-contrast など) は、中身しだいで出たり出なかったりするので、基準の ignoredRules に理由と一緒に書いて、
// 画面ごとには比べない。どの画面でも出なかった ignoredRules は落とす。
//
// 違反が出たときは、落ちたメッセージに出る規則と要素 (selector と HTML の頭) を見て直すか、
// 直さないなら基準に足して理由を書く。基準の更新は手で行う。

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import AxeBuilder from '@axe-core/playwright';
import { expect, type Page, test } from '@playwright/test';
import { rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { type Principal } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { signinWithState } from '../../../fixtures/settings_table';
import { gotoRoute } from '../../../fixtures/ui_route';
import { freshUser, postNote } from '../../../fixtures/ui_timeline';

type Baseline = {
  screens: Record<string, string[]>;
  reasons: Record<string, string>;
  ignoredRules: Record<string, string>;
  buttonNameAllowed: Record<string, string>;
};
const BASELINE: Baseline = JSON.parse(readFileSync(join(__dirname, '../../../a11y-baseline.json'), 'utf-8'));

const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];

type Who = 'anon' | 'user' | 'admin';
const SCREENS: { who: Who; path: string }[] = [
  { who: 'anon', path: '/about' },
  { who: 'anon', path: '/signup-application' },
  ...['/', '/my/notifications', '/my/drive', '/settings', '/settings/profile', '/settings/preferences', '/settings/other',
    '/search', '/chat', '/@:me', '/notes/:note', '/about-elythia', '/bubble-game', '/bubble-game/versus/history',
    '/emoji-request', '/my/follow-requests'].map((path) => ({ who: 'user' as const, path })),
  ...['/admin', '/admin/users', '/admin/settings', '/admin/moderation', '/admin/backups', '/admin/federation',
    '/admin/ip-search', '/admin/signup-applications', '/admin/server-plugins', '/admin/files', '/admin/database',
    '/admin/modlog', '/custom-emojis-manager'].map((path) => ({ who: 'admin' as const, path })),
];
const key = (s: { who: Who; path: string }) => `${s.who} ${s.path}`;

/**
 * The mark of a button without an accessible name: its data-testid, or else the
 * icon classes (ti-*) of all its icons. CSS Modules classes are hashed and are
 * not used.
 */
function buttonMark(html: string): string {
  const testId = /data-testid="([^"]+)"/.exec(html)?.[1];
  if (testId != null) return `testid:${testId}`;
  // 最初のアイコンだけで作ると、別のボタンと同じ印になる (開閉の ti-chevron-up と、ノートのページの
  // 前後を読む ti-chevron-up + ti-user など)。すべてのアイコンを並べる
  const icons = [...html.matchAll(/<i [^>]*class="([^"]*)"/g)]
    .map((m) => m[1].split(/\s+/).filter((c) => c.startsWith('ti-') && c !== 'ti-fw').join('.'))
    .filter((c) => c !== '');
  return icons.length > 0 ? `icon:${icons.join('+')}` : `html:${html.slice(0, 40)}`;
}
const seenMarks = new Set<string>();
const seenIgnored = new Set<string>();
let screensChecked = 0;

test.describe('a11y: axe-core against the baseline', () => {
  // 基準は Elythia の frontend の違反。TS backend の実行では本家の frontend が出るので合わない
  test.skip(isTsBackend, '基準は Elythia の frontend のもの');
  test.setTimeout(60_000);

  test('the baseline has no screen that is not checked', async () => {
    const checked = new Set(SCREENS.map(key));
    const unused = Object.keys(BASELINE.screens).filter((k) => !checked.has(k));
    expect(unused, '基準に、検査していない画面の行が残っている').toEqual([]);
    const missing = SCREENS.map(key).filter((k) => !(k in BASELINE.screens));
    expect(missing, '基準に行の無い画面がある (違反が無いなら空の配列で書く)').toEqual([]);
    const ruleNames = new Set(Object.values(BASELINE.screens).flat());
    const unexplained = [...ruleNames].filter((r) => !(r in BASELINE.reasons));
    expect(unexplained, '基準に残す規則には、理由を書く').toEqual([]);
    const unusedReasons = Object.keys(BASELINE.reasons).filter((r) => !ruleNames.has(r));
    expect(unusedReasons, '使われていない理由が残っている').toEqual([]);
    const both = Object.keys(BASELINE.ignoredRules).filter((r) => ruleNames.has(r));
    expect(both, '無視する規則が、画面ごとの基準にも書かれている').toEqual([]);
    // axe は 300 文字を超える要素を開始タグだけに縮めるので、アイコンが取れずに html: の印になる。
    // その頭には CSS Modules のハッシュが入り、ビルドごとに変わるので許可リストには書かせない
    const htmlMarks = Object.keys(BASELINE.buttonNameAllowed).filter((m) => !/^(testid|icon):/.test(m));
    expect(htmlMarks, 'buttonNameAllowed の印は testid: か icon: で書く (html: はビルドごとに変わる)').toEqual([]);
  });

  let user: Principal;
  let noteId: string;
  test.beforeAll(async ({ request }) => {
    user = await freshUser(request, 'pwa11y');
    noteId = await postNote(request, user, 'a11y note');
  });

  /** Signs `page` in as `who` (each test has a context of its own). */
  async function signin(page: Page, request: import('@playwright/test').APIRequestContext, baseURL: string, who: Who): Promise<void> {
    if (who === 'anon') {
      await page.addInitScript(() => window.localStorage.setItem('modifiedVersionMustProminentlyOfferInAgplV3Section13Read', 'true'));
    } else if (who === 'user') {
      await signinWithState(page, request, baseURL, user);
    } else {
      await signinAdmin(page, baseURL, rootFixture());
    }
  }

  for (const screen of SCREENS) {
    test(key(screen), async ({ page, request, baseURL }) => {
      await signin(page, request, baseURL!, screen.who);
      await gotoRoute(page, baseURL!, screen.path.replace(':me', user.username).replace(':note', noteId));
      const result = await new AxeBuilder({ page }).withTags(TAGS).analyze();
      for (const v of result.violations) if (v.id in BASELINE.ignoredRules) seenIgnored.add(v.id);
      const violations = result.violations.filter((v) => !(v.id in BASELINE.ignoredRules));
      const found = violations.map((v) => v.id).sort();
      const nameless = violations.find((v) => v.id === 'button-name')?.nodes ?? [];
      const marked = nameless.map((n) => ({ mark: buttonMark(n.html), html: n.html.slice(0, 120) }));
      for (const m of marked) seenMarks.add(m.mark);
      screensChecked++;
      const unexpected = marked.filter((m) => !(m.mark in BASELINE.buttonNameAllowed)).map((m) => `${m.mark} :: ${m.html}`);
      expect(unexpected, '名前の無いボタンが増えた (直すか、基準の buttonNameAllowed に理由と一緒に足す)').toEqual([]);
      const expected = [...(BASELINE.screens[key(screen)] ?? [])].sort();
      const added = violations.filter((v) => !expected.includes(v.id));
      const detail = added.map((v) => `${v.id} (${v.impact}): ${v.nodes.slice(0, 3).map((n) => `${n.target.join(' ')} :: ${n.html.slice(0, 120)}`).join(' | ')}`).join('\n');
      expect(added.map((v) => v.id), `基準に無い違反が出た\n${detail}`).toEqual([]);
      const fixed = expected.filter((id) => !found.includes(id));
      expect(fixed, '基準にある違反が出なくなった。直したなら基準から消す').toEqual([]);
    });
  }

  // 各画面のテストの後に流れる (同じファイルのテストは書いた順に 1 つの worker で流れる)。afterAll に
  // しないのは、afterAll の失敗は最後のテストの失敗として retry され、retry の worker では数え直しになって
  // 判定を飛ばし、flaky として通ってしまうため
  test('the baseline has no unused marks or ignored rules', () => {
    if (screensChecked < SCREENS.length) {
      // このテストの retry では、各画面を流し直さないので判定できない。通さずに落とす
      expect(test.info().retry, 'retry では、使われていない印と ignoredRules を判定できない').toBe(0);
      // 一部の画面だけを流したとき (-g など) や、前の画面が落ちて worker が作り直されたときは判定できない。
      // 黙って飛ばさず、飛ばしたことを出す
      console.warn(`a11y: ${screensChecked}/${SCREENS.length} 画面しか流れていないので、使われていない印と ignoredRules の判定を飛ばした`);
      return;
    }
    const unused = {
      buttonNameAllowed: Object.keys(BASELINE.buttonNameAllowed).filter((m) => !seenMarks.has(m)),
      ignoredRules: Object.keys(BASELINE.ignoredRules).filter((r) => !seenIgnored.has(r)),
    };
    expect(unused, '基準に、どの画面でも出なかった印か ignoredRules が残っている').toEqual({ buttonNameAllowed: [], ignoredRules: [] });
  });
});
