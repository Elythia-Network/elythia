/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// unit test にはサーバーが無い。部品が mount の中で API を叩くと、happy-dom の fetch が
// `localhost:3000` へ実際に繋ぎに行き、ECONNREFUSED で reject する。`catch` の無い
// 呼び出し (`pages/user/index.files.vue` の `users/notes` など) では、それがテストの
// 実行中に着地して vitest の unhandled rejection (exit 1) になる。本家の
// `test/unit/home.test.ts` は await を挟まないので表に出にくいだけで、挟むと落ちる
// (#3369、実測)。
//
// **テストの実行中に、同じ origin の `/api/` への要求は応答が来ないまま待たせる**
// (サーバーが黙っているのと同じ)。成功で答えると、部品が中身を読んで別の形で落ちる
// (例えばチャートが統計の形でない値を読む)。`signal` で中止されたときだけ、その理由
// (既定は `AbortError`) で reject する。それ以外の URL は元の fetch に渡す。
//
// **ファイルの読み込み (collect) の間は待たせない** (最初のテストの前の `beforeAll` で
// 切り替える)。vitest には読み込みの待ち時間の上限が無いので、モジュールの最上位で
// 答えの無い API を await すると、どのファイルのどの API かも出さずに止まったまま
// 終わらない (実測)。読み込みの間は元の fetch に渡し、URL 付きのエラーですぐ落とす。
//
// **待たせる対象を `/api/` の外 (同じ origin の全部など) へ広げない。** 本家の
// `frontend-shared/js/locale.ts` は最上位で locale の JSON を fetch して待つ。
// `setup.unit.ts` の locale の mock (`/^\/assets\/locales\//`) は、vitest-fetch-mock が
// 正規表現を絶対 URL に当てるので一度も当たらず、この要求は元の fetch へ行って
// ECONNREFUSED を console に出す (`() => null` で拾われるので失敗にはならない)。
// 広げると、この読み込みが止まる。
//
// **`setup.unit.ts` より前に置く** (`vitest.config.unit.ts` の `setupFiles` の先頭)。
// `setup.unit.ts` の `createFetchMock` は作った時点の `globalThis.fetch` を「元の
// fetch」として掴むので、先にこれを入れておけば、`fetchMock.mockIf` などで答えを
// 決めたテストはこれまでどおりその答えを受け取り、決めていない要求だけがここに来る。
// テストファイルが `globalThis.fetch` を自分で差し替えれば、そちらが優先される。
//
// 既知の性質:
// - API を mock せずにテストの中で応答を await するテストは、ECONNREFUSED ですぐ
//   落ちる代わりに、テストの timeout で落ちる。原因が分かるよう、テストが失敗した
//   ときは、そのテストの本体の間に始まって待ったままの要求の URL を console に出す。
//   出すのはテストの本体の要求だけで、hook (`beforeAll` などの中で待つと hook の
//   timeout で落ちる) と `test.concurrent` (要求が別のテストの記録に入る) では出ない
// - 応答を await しないテストは、答えの無い API を叩いても黙って通る

import { beforeAll, beforeEach } from 'vitest';
import { createHoldingFetch } from './holding-fetch.js';

let collecting = true;
beforeAll(() => {
	collecting = false;
});

// 今のテストの間に始まり、まだ待っている要求 (method と URL ごとの数)。
let pendingInCurrentTest = new Map<string, number>();
beforeEach((context) => {
	const pending = new Map<string, number>();
	pendingInCurrentTest = pending;
	context.onTestFailed(() => {
		if (pending.size === 0) return;
		console.error(`[setup-network] unanswered API requests still pending when the test failed (no fetchMock answer, no server in unit tests):\n${[...pending].map(([label, count]) => `  ${label}${count > 1 ? ` (x${count})` : ''}`).join('\n')}`);
	});
});

globalThis.fetch = createHoldingFetch(globalThis.fetch, {
	collecting: () => collecting,
	pending: () => pendingInCurrentTest,
});
