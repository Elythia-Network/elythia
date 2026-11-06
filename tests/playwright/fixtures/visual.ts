/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 見た目の差分 (toHaveScreenshot) の spec (#4054) で共有する helper。
//
// **毎回同じ絵にする。** 撮る画面に出るものは、次のどれかにする。
// - 固定の利用者 (`vr_alice` など) と、その利用者だけが作った固定のデータ。
//   他の spec のノートが混ざらないよう、ホームのタイムラインは固定の利用者
//   同士だけでフォローし合う。種まきは 1 つの DB で 1 回だけ行う (手元では
//   DB を作り直さずに 2 回目を流すため)
// - 時刻で変わるもの (相対時刻、日付) は消し、他の spec のデータが出るもの
//   (入口のタイムライン、人数) は場所を保ったまま見えなくする (`snap`)
// - アニメーションは OS の「動きを減らす」で止める (prefer の animation と
//   animatedMfm の既定がこれに従う)。CSS のアニメーションは toHaveScreenshot の
//   animations: 'disabled' が止める
// - ウィジェットの既定 (カレンダー・トレンド) は日付と他の spec のデータで
//   変わるので、起動の前に端末の設定を書き換えて差し替える
//
// **基準画像は runner の container で作る** (`make playwright-update-snapshots`)。
// 手元の OS で作ると文字の描画が違う (docs/playwright.md)。

import { readFileSync } from 'node:fs';
import { deflateSync } from 'node:zlib';
import { type APIRequestContext, expect, type Locator, type Page } from '@playwright/test';
import { callApi } from './api';
import { waitForMountedPage } from './route_check';
import { DEFAULT_TEST_PASSWORD, type Principal, signin } from './auth';
import { uiSigninViaForm } from './ui_auth';
import { uploadPNG } from './ui_drive';
import { apiOk } from './ui_timeline';

export type Scheme = 'light' | 'dark';
export const SCHEMES: readonly Scheme[] = ['light', 'dark'];

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let j = 0; j < 8; j++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[i] = c >>> 0;
  }
  return t;
})();

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

type RGB = readonly [number, number, number];

/**
 * Encodes a deterministic RGB PNG: diagonal stripes of `a` and `b`, so that a
 * scaled or cropped image still looks different from a solid block.
 */
function stripedPNG(width: number, height: number, a: RGB, b: RGB, stripe = 16): Buffer {
  const raw = Buffer.alloc((width * 3 + 1) * height);
  for (let y = 0; y < height; y++) {
    const row = y * (width * 3 + 1);
    raw[row] = 0;
    for (let x = 0; x < width; x++) {
      const [r, g, bl] = Math.floor((x + y) / stripe) % 2 === 0 ? a : b;
      raw.set([r, g, bl], row + 1 + x * 3);
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;
  ihdr[9] = 2;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

const PALETTE: Record<string, [RGB, RGB]> = {
  alice: [[86, 108, 214], [168, 182, 245]],
  bob: [[222, 128, 64], [247, 205, 160]],
  carol: [[62, 160, 112], [170, 222, 190]],
  banner: [[44, 52, 110], [120, 98, 180]],
  photo1: [[200, 70, 90], [250, 190, 200]],
  photo2: [[60, 140, 200], [180, 220, 245]],
  photo3: [[220, 180, 50], [250, 236, 170]],
  photo4: [[110, 80, 160], [205, 190, 235]],
};

function paletteImage(name: keyof typeof PALETTE, width = 256, height = 256): Buffer {
  const [a, b] = PALETTE[name];
  return stripedPNG(width, height, a, b);
}

export interface VisualPageOptions {
  /** Widgets of the right column, by name (none by default). */
  widgets?: readonly string[];
  /** More account-dependent preferences, such as the deck profiles. */
  accountPrefs?: Record<string, unknown>;
  /** The UI to boot into (`deck` for the deck). */
  ui?: 'deck';
}

/**
 * Prepares `page` for a screenshot in `scheme` before anything is loaded: the
 * color scheme and reduced motion of the OS, a seeded Math.random, and the
 * account's widgets replaced by `options.widgets` (none by default). The
 * default widgets change by date (calendar) and by what other specs post
 * (trends), and the notifications widget fills in at its own pace.
 */
export async function prepareVisualPage(page: Page, scheme: Scheme, options: VisualPageOptions = {}): Promise<void> {
  await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' });
  await page.addInitScript((opts) => {
    // 起動画面の星など、乱数で描くものを毎回同じにする
    let seed = 0x4054;
    Math.random = () => {
      seed = (seed + 0x6d2b79f5) | 0;
      let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
    // AGPL の第 13 条のソースコードの案内は、最初の起動で 1 回だけ出る。撮る画面では出さない
    window.localStorage.setItem('modifiedVersionMustProminentlyOfferInAgplV3Section13Read', 'true');
    if (opts.ui) window.localStorage.setItem('ui', opts.ui);
    // ウィジェットはアカウントごとの設定なので、そのアカウントの scope にも書く。
    // 書かないと、起動時の正規化がアカウントの scope に既定 (カレンダー) を足す
    try {
      const account = JSON.parse(window.localStorage.getItem('account') ?? 'null') as { id?: string } | null;
      const raw = window.localStorage.getItem('preferences');
      const profile = raw ? JSON.parse(raw) : { id: 'visual', version: '', type: 'main', modifiedAt: 0, name: '', preferences: {} };
      const prefs: Record<string, unknown> = {
        widgets: opts.widgets.map((name) => ({ name, id: `vrw-${name}`, place: 'right', data: {} })),
        ...opts.accountPrefs,
      };
      for (const [key, value] of Object.entries(prefs)) {
        profile.preferences[key] = [[{}, value, {}]];
        if (account?.id) profile.preferences[key].push([{ server: location.host, account: account.id }, value, {}]);
      }
      window.localStorage.setItem('preferences', JSON.stringify(profile));
    } catch {
      // 読めない形なら何もしない (撮った絵の差分で分かる)
    }
  }, { widgets: options.widgets ?? [], accountPrefs: options.accountPrefs ?? {}, ui: options.ui ?? null });
}

// 撮るたびに変わる文字で、<time> の外に出るもの。クラス名は本番のビルドでハッシュに
// なるので、文字で探して消す (snap)。
// - en-US の書式の日時 (11/6/2026, 9:36:27 AM): ノートの見出し、登録日。AM / PM の前の
//   空白は、Chromium の Intl では U+202F (狭い空白) になるので \s で受ける
// - 年と月 (2026/11): ドライブの、作った月ごとの見出し。種まきは CI の実行のたびなので、
//   月が変わると変わる
// - 版 (2.1.0、v2.1.0、Elythia 2.1.0 (abc1234)、2.2.0-rc.1、2026.10.1): about のページ。
//   版を上げるたびに変わる。本文の中の版まで消さないよう、テキスト全体で縛る
const VOLATILE_TEXT = [
  String.raw`\d{1,2}/\d{1,2}/\d{4},?\s\d{1,2}:\d{2}(:\d{2})?\s[AP]M`,
  String.raw`^\s*\d{4}/\d{1,2}\s*$`,
  String.raw`^\s*(Elythia\s+)?v?\d+\.\d+\.\d+([-+][\w.]+)?(\s+\([0-9a-f]*\))?\s*$`,
];

/**
 * Waits until the page is still (fonts loaded, images decoded) and compares it
 * with the baseline `name`. `keepPointer` leaves the pointer where it is (for
 * tooltips), and `clip` cuts out a region of the page. Text that changes on
 * every run (`<time>`, and dates and versions printed outside it) is removed,
 * and `hide` (CSS selectors) is made invisible in place; `mask` paints boxes
 * on top of everything, so use it only for what no dialog covers.
 */
export async function snap(page: Page, name: string, options: {
  mask?: Locator[];
  hide?: string[];
  keepPointer?: boolean;
  clip?: { x: number; y: number; width: number; height: number };
} = {}): Promise<void> {
  // ページを開いた直後に、一覧を描き足している途中で撮らないよう、通信が静まるのを
  // 待つ。今の文書で一度静まっていればすぐに返るので、同じページで操作した後の撮影
  // では効かない。そちらは、出てくる要素を spec で待ち、toHaveScreenshot が 2 枚
  // 続けて同じになるまで撮り直すことに任せる。ストリーミングの WebSocket は数えられない
  await page.waitForLoadState('networkidle');
  await page.evaluate(() => document.fonts.ready.then(() => undefined));
  // 遅延読み込みの画像は、画面に入るまで読み込まれない (complete にならない)。
  // 画面に入っているものだけを待つ
  await page.waitForFunction(() => Array.from(document.images).every((img) => {
    if (img.complete) return true;
    const r = img.getBoundingClientRect();
    return r.bottom <= 0 || r.top >= window.innerHeight || r.right <= 0 || r.left >= window.innerWidth;
  }), undefined, { timeout: 20_000 });
  // 入力欄のキャレットとホバーの影が残らないよう、ポインタを隅へ寄せる
  if (!options.keepPointer) await page.mouse.move(0, 0);
  // 変わる文字は場所ごと消し、hide に渡したものは場所を保ったまま見えなくする。mask の
  // 四角は、要素がダイアログやツールチップの背後にあっても一番上に描かれ、手前のものを
  // 隠すため。時刻は文字の長さ (3m ago と 10m ago) で幅が変わり、場所を保つと隣がずれる
  // ので消す。<time> の外の文字には印を付けて選ぶ。toHaveScreenshot には page.screenshot
  // の style が無い (知らない指定は黙って無視される) ので、ページに <style> を足す
  await page.evaluate(({ patterns, hidden }) => {
    const res = patterns.map((p) => new RegExp(p));
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent ?? '';
      if (res.some((re) => re.test(text))) node.parentElement?.setAttribute('data-vr-volatile', '');
    }
    const style = document.createElement('style');
    style.textContent = `time, [data-vr-volatile] { display: none !important; }${hidden.length ? ` ${hidden.join(', ')} { visibility: hidden !important; }` : ''}`;
    document.head.append(style);
  }, { patterns: VOLATILE_TEXT, hidden: options.hide ?? [] });
  // threshold は 1 画素の色の許容 (既定 0.2)。既定では、テーマの色を少し変えた
  // ぐらい (パネルを #0e1433 から #162048 へ) は同じ色とみなされて通る (#4054 で
  // 変異させて確かめた)。許容を超えた画素は 8 つまで認める。プロフィールの大きな
  // アイコンの縁が、同じスタックでもときどき 5 画素ずれるため。文字を 1 つ変えると
  // 12〜19 画素変わる (所在地の Tokyo を Tokyp にして測った) ので、それは落ちる。
  // 割合で認めると (1% で約 1 万画素)、件数の数字や時刻の文字が変わっても通る
  await expect(page).toHaveScreenshot(`${name}.png`, {
    animations: 'disabled',
    caret: 'hide',
    mask: options.mask ?? [],
    threshold: 0.02,
    maxDiffPixels: 8,
    timeout: 20_000,
    ...(options.clip ? { clip: options.clip } : {}),
  });
}

export interface VisualUser extends Principal {
  name: string;
}

export interface VisualWorld {
  alice: VisualUser;
  bob: VisualUser;
  carol: VisualUser;
  /** A user alice does not follow, who makes the notifications of one test and deletes the notes afterwards. */
  dave: VisualUser;
  /** alice's pinned note, which bob and carol reacted to and carol quoted. */
  aliceNoteId: string;
  /** bob's reply in a conversation with carol (a parent above and a child below). */
  conversationNoteId: string;
  /**
   * alice's oldest note, which dave replies to, renotes, quotes and reacts to
   * in the notifications test. Deleting a renote does not decrease the renote
   * count (as upstream), so the count grows on every run on the same stack;
   * the note is the oldest so that no screenshot showing the count (the home
   * timeline, the profile) reaches it. The notifications screen shows it, but
   * without the count.
   */
  notifyTargetNoteId: string;
  /** Folders and files in alice's drive root. */
  driveFolders: string[];
  driveFiles: string[];
}

// frontend の src/tips.ts の TIPS。本家で増えたら、その案内が撮った絵に出て差分になる
const TIPS = ['drive', 'uploader', 'postFormUploader', 'clips', 'userLists', 'postForm', 'deck', 'tl.home', 'tl.local', 'tl.social', 'tl.global', 'abuses'];

const SEED_MARK = 'Visual regression fixture (#4054)';
const CONVERSATION_REPLY = 'A book about the stars.';
const NOTIFY_TARGET = 'Any plans for the weekend?';

// ログインの通知はサインインのたびに増え、実績の通知は画面を開いた順で増える
// (実績の多くは frontend が付ける)。どちらも撮るたびに変わるので受け取らない
const QUIET_NOTIFICATIONS = { login: { type: 'never' }, achievementEarned: { type: 'never' } };

async function userFor(request: APIRequestContext, username: string, name: string): Promise<VisualUser> {
  const signup = await callApi(request, 'signup', { username, password: DEFAULT_TEST_PASSWORD });
  const token = signup.status() === 200 ? (await signup.json()).token as string : await signin(request, username, DEFAULT_TEST_PASSWORD);
  const me = await apiOk(request, 'i', { i: token });
  await apiOk(request, 'i/update', { i: token, notificationRecieveConfig: { ...me.notificationRecieveConfig, ...QUIET_NOTIFICATIONS } });
  return { id: me.id, token, username, name };
}

type Seeded = Pick<VisualWorld, 'aliceNoteId' | 'conversationNoteId' | 'notifyTargetNoteId'>;

async function seed(request: APIRequestContext, baseURL: string, w: Omit<VisualWorld, keyof Seeded>): Promise<Seeded> {
  const { alice, bob, carol, dave } = w;
  const upload = async (user: Principal, name: keyof typeof PALETTE, folderId?: string, size?: [number, number]) => {
    const file = await uploadPNG(request, baseURL, user.token, `${name}.png`, paletteImage(name, ...(size ?? [256, 256])));
    if (folderId) await apiOk(request, 'drive/files/update', { i: user.token, fileId: file.id, folderId });
    return file;
  };

  // alice のドライブ: フォルダ 2 つと、ルートに画像 4 つ。アイコンとバナーはフォルダに入れる
  // 種まきが途中で落ちると、retry でもう一度最初から走る。2 回目に落ちる段 (フォルダ・
  // ロール・フォロー) は、あるものを使う形にする。アップロードとノートは重なるが、
  // 重なると撮った絵が変わって落ちる (黙って通ることはない)。ただし印 (自己紹介) を
  // 書くまでは後のテストも種をまき直すので、その shard の見た目のテストはほぼ全部落ちる。
  // そのときは shard を流し直す
  const folders = await apiOk(request, 'drive/folders', { i: alice.token, limit: 100 }) as { id: string; name: string }[];
  const folder = async (name: string) => folders.find((f) => f.name === name) ?? apiOk(request, 'drive/folders/create', { i: alice.token, name });
  const photos = await folder(w.driveFolders[0]);
  await folder(w.driveFolders[1]);
  for (const name of w.driveFiles) await upload(alice, name as keyof typeof PALETTE);

  for (const [user, key] of [[alice, 'alice'], [bob, 'bob'], [carol, 'carol'], [dave, 'photo3']] as const) {
    const avatar = await upload(user, key, user === alice ? photos.id : undefined);
    await apiOk(request, 'i/update', { i: user.token, name: user.name, avatarId: avatar.id, hideOnlineStatus: true });
  }
  const banner = await upload(alice, 'banner', photos.id, [1200, 400]);
  await apiOk(request, 'i/update', {
    i: alice.token,
    bannerId: banner.id,
    location: 'Tokyo',
    fields: [{ name: 'Website', value: 'https://example.com' }, { name: 'Pronouns', value: 'they/them' }],
  });

  // alice をモデレーターにする (管理画面を alice で撮るため)。公開のロールにして、
  // プロフィールにバッジが出るようにする
  const root = JSON.parse(readFileSync('.auth/root.json', 'utf-8')) as { token: string };
  const badge = await upload(alice, 'photo2', photos.id, [64, 64]);
  const roles = await apiOk(request, 'admin/roles/list', { i: root.token }) as { id: string; name: string }[];
  const role = roles.find((r) => r.name === 'Visual moderators') ?? await apiOk(request, 'admin/roles/create', {
    i: root.token, name: 'Visual moderators', description: 'Moderators of the visual fixture', color: '#5b6be0', iconUrl: (badge as unknown as { url: string }).url,
    target: 'manual', condFormula: {}, isPublic: true, isExplorable: false,
    isAdministrator: false, isModerator: true, asBadge: true,
    canEditMembersByModerator: false, displayOrder: 0, policies: {},
  });
  const assigned = await apiOk(request, 'admin/roles/users', { i: root.token, roleId: role.id, limit: 100 }) as { user: { id: string } }[];
  if (!assigned.some((a) => a.user.id === alice.id)) await apiOk(request, 'admin/roles/assign', { i: root.token, roleId: role.id, userId: alice.id });

  // 3 人は互いにフォローする (チャットは相互フォローで開く)。dave は誰もフォローしない
  for (const [a, b] of [[alice, bob], [bob, alice], [alice, carol], [carol, alice], [bob, carol], [carol, bob]] as const) {
    const resp = await callApi(request, 'following/create', { i: a.token, userId: b.id });
    if (!resp.ok() && (await resp.json()).error?.code !== 'ALREADY_FOLLOWING') throw new Error(`following/create failed: ${resp.status()}`);
  }

  // ノートは古い順に作る。タイムラインには新しい順に出る。
  //
  // **alice への返信は作らない。** ホームのタイムラインは、Redis の記録からは
  // 「自分への返信」を出すが、記録が無いときの DB からの組み立てでは出さない
  // (本家も同じ)。Playwright の globalSetup は流すたびに Redis を空にするので、
  // 同じスタックの 2 回目で絵が変わる。返信は自分のノートへの返信 (どちらでも
  // 出る) と、alice が当事者でない会話 (どちらでも出ない) にする
  const note = async (user: Principal, body: Record<string, unknown>) => (await apiOk(request, 'notes/create', { i: user.token, ...body })).createdNote.id as string;
  const notifyTargetNoteId = await note(alice, { text: NOTIFY_TARGET });
  const question = await note(carol, { text: 'What are you reading these days?' });
  const conversationNoteId = await note(bob, { text: CONVERSATION_REPLY, replyId: question });
  await note(carol, { text: 'Sounds nice, tell me more.', replyId: conversationNoteId });
  const aliceNoteId = await note(alice, { text: `Hello from Alice. @${carol.username} have a look.` });
  await apiOk(request, 'notes/reactions/create', { i: bob.token, noteId: aliceNoteId, reaction: '👍' });
  await apiOk(request, 'notes/reactions/create', { i: carol.token, noteId: aliceNoteId, reaction: '❤' });
  await apiOk(request, 'i/pin', { i: alice.token, noteId: aliceNoteId });
  await note(carol, { text: 'Quoting Alice.', renoteId: aliceNoteId });
  await note(carol, { text: `@${alice.username} a mention for you.` });
  const plain = await note(carol, { text: 'A plain note with **bold**, `code`, a #visual tag and a mention of nobody.' });
  await note(bob, { renoteId: plain });
  await note(bob, { cw: 'Spoiler for the next episode', text: 'The hidden part of the note.' });
  await note(carol, { text: 'Which do you prefer?', poll: { choices: ['Tea', 'Coffee', 'Water'], multiple: false } });
  const thread = await note(bob, { text: 'A thread, part 1.' });
  await note(bob, { text: 'A thread, part 2.', replyId: thread });
  const one = await upload(bob, 'photo1');
  await note(bob, { text: 'One picture.', fileIds: [one.id] });
  const four = await Promise.all((['photo1', 'photo2', 'photo3', 'photo4'] as const).map((n) => upload(carol, n)));
  await note(carol, { text: 'Four pictures.', fileIds: four.map((f) => f.id) });

  // チャット: bob と alice のやり取り
  for (const [from, to, text] of [[bob, alice, 'Hi Alice!'], [alice, bob, 'Hi Bob, how are you?'], [bob, alice, 'Great, thanks.']] as const) {
    await apiOk(request, 'chat/messages/create-to-user', { i: from.token, toUserId: to.id, text });
  }

  // 自己紹介は最後に書く。これがあれば種まきは済んでいる
  await apiOk(request, 'i/update', { i: alice.token, description: `${SEED_MARK}. Likes tea and quiet evenings.` });
  return { aliceNoteId, conversationNoteId, notifyTargetNoteId };
}

async function findNote(request: APIRequestContext, user: Principal, text: string): Promise<string> {
  const notes = await apiOk(request, 'users/notes', { i: user.token, userId: user.id, limit: 100, withReplies: true });
  const hit = (notes as { id: string; text: string | null }[]).find((n) => n.text === text);
  expect(hit, `${user.username} has no note "${text}"`).toBeTruthy();
  return hit!.id;
}

/**
 * Marks everything alice has unread as read: notifications (after the ones
 * still being delivered have arrived), chat and announcements. Other specs
 * create announcements, and the badges for them would differ by shard.
 */
export async function clearUnread(request: APIRequestContext, alice: Principal): Promise<void> {
  // 通知はジョブで遅れて届くので、件数が 3 回続けて変わらなくなるまで待つ
  let last = -1;
  let same = 0;
  for (let i = 0; i < 40 && same < 3; i++) {
    const count = (await apiOk(request, 'i/notifications', { i: alice.token, limit: 100 })).length;
    same = count === last ? same + 1 : 0;
    last = count;
    await new Promise((r) => setTimeout(r, 250));
  }
  await apiOk(request, 'notifications/mark-all-as-read', { i: alice.token });
  await apiOk(request, 'chat/read-all', { i: alice.token });
  const announcements = await apiOk(request, 'announcements', { i: alice.token, isActive: true, limit: 100 });
  for (const a of announcements as { id: string; isRead?: boolean }[]) {
    if (!a.isRead) await apiOk(request, 'i/read-announcement', { i: alice.token, announcementId: a.id });
  }
  const me = await apiOk(request, 'i', { i: alice.token });
  expect({ notifications: me.hasUnreadNotification, chat: me.hasUnreadChatMessages, announcements: me.hasUnreadAnnouncement }).toEqual({ notifications: false, chat: false, announcements: false });
}

/**
 * Returns the fixed users and their data, seeding them on the first call in a
 * database, and clears what alice has unread (`clearUnread`).
 */
export async function visualWorld(request: APIRequestContext, baseURL: string): Promise<VisualWorld> {
  const alice = await userFor(request, 'vr_alice', 'Alice Example');
  const bob = await userFor(request, 'vr_bob', 'Bob Example');
  const carol = await userFor(request, 'vr_carol', 'Carol Example');
  const dave = await userFor(request, 'vr_dave', 'Dave Example');
  const base = { alice, bob, carol, dave, driveFolders: ['Photos', 'Documents'], driveFiles: ['photo1', 'photo2', 'photo3', 'photo4'] };
  const me = await apiOk(request, 'i', { i: alice.token });
  let seeded: Seeded;
  if (typeof me.description === 'string' && me.description.startsWith(SEED_MARK)) {
    seeded = {
      aliceNoteId: me.pinnedNoteIds[0],
      conversationNoteId: await findNote(request, bob, CONVERSATION_REPLY),
      notifyTargetNoteId: await findNote(request, alice, NOTIFY_TARGET),
    };
  } else {
    seeded = await seed(request, baseURL, base);
  }
  await clearUnread(request, alice);
  // 通知は Redis にしか無く、Playwright の globalSetup が流すたびに消す。種まきの
  // 回だけ残ると 2 回目と絵が変わるので、ふだんは空にする。通知の画面のテストは
  // 自分で作ってから撮る
  await apiOk(request, 'notifications/flush', { i: alice.token });
  return { ...base, ...seeded };
}

/** The region from the top of `from` to the top of `until`, as wide as `from`. */
export async function regionAbove(from: Locator, until: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const a = await from.boundingBox();
  const b = await until.boundingBox();
  expect(a && b, 'region boxes').toBeTruthy();
  return { x: a!.x, y: a!.y, width: a!.width, height: b!.y - a!.y };
}

/** Signs in as `user` in the prepared page, at the viewport the screenshots use. */
export async function visualSignin(page: Page, request: APIRequestContext, baseURL: string, user: Principal, viewport = { width: 1280, height: 800 }): Promise<void> {
  const wizard = await callApi(request, 'i/registry/set', { i: user.token, scope: ['client', 'base'], key: 'accountSetupWizard', value: -1 });
  expect(wizard.ok(), `i/registry/set failed: ${wizard.status()}`).toBe(true);
  await uiSigninViaForm(page, baseURL, user.username);
  // 「Tips & Tricks」の案内を全部閉じる。閉じた印は端末の状態 (IndexedDB の
  // pizzax::base) にあり、起動の前に書くとアプリの読み込みと順序を競うので、
  // サインインの後に書いて読み込み直す
  await page.evaluate((tips) => new Promise<void>((resolve, reject) => {
    const open = indexedDB.open('keyval-store');
    open.onupgradeneeded = () => open.result.createObjectStore('keyval');
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const db = open.result;
      const tx = db.transaction('keyval', 'readwrite');
      const store = tx.objectStore('keyval');
      const read = store.get('pizzax::base');
      read.onsuccess = () => store.put({ ...(read.result ?? {}), tips: Object.fromEntries(tips.map((t) => [t, true])) }, 'pizzax::base');
      tx.oncomplete = () => { db.close(); resolve(); };
      tx.onerror = () => reject(tx.error);
    };
  }), TIPS);
  await page.setViewportSize(viewport);
  await page.reload({ waitUntil: 'domcontentloaded' });
  // デッキでは投稿ボタンの testid が無いので、起動画面が消えるのを待つ
  await waitForMountedPage(page);
}

/**
 * Seeds the world (`visualWorld`), prepares the page in `scheme` and signs in
 * as alice: the start of most screenshots.
 */
export async function startAsAlice(
  page: Page,
  request: APIRequestContext,
  baseURL: string,
  scheme: Scheme,
  options: VisualPageOptions & { viewport?: { width: number; height: number } } = {},
): Promise<VisualWorld> {
  const w = await visualWorld(request, baseURL);
  await prepareVisualPage(page, scheme, options);
  await visualSignin(page, request, baseURL, w.alice, options.viewport);
  return w;
}
