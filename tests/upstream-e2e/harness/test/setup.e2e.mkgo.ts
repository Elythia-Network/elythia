/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/*
 * mk-go 版の e2e setup。
 *
 * 本家の `setup.e2e.ts` は `initTestDb(false)` で TypeORM のエンティティ定義から
 * スキーマを作り直すが、それをやると mk-go の migration でしか作られないテーブル
 * (relay_observed_user 等) が消える。代わりに mk-go の `/api/reset-db` を叩いて
 * 全テーブルを truncate する (schema_migrations は保護される)。
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { beforeAll, beforeEach } from 'vitest';
import { loadConfig } from '@/config.js';
import { sendEnvResetRequest } from './utils.js';

const config = loadConfig();

/*
 * 既知の乖離を expected-failure として扱う。
 *
 * mk-go では『通らないことが正しい』テストが存在する (mk-go 独自の role
 * policy、意図的に採用していない upstream の検索順序、ワーカーを同一プロセスで
 * 動かすことによるハーネスの構造差など)。それらを fail のまま残すと CI が常に
 * 赤になり、本物の regression が埋もれる。
 *
 * skip ではなく `task.fails` を立てるのが要点で、乖離が解消して通るように
 * なったテストは『Expected test to fail』で落ちる。一覧が陳腐化しても
 * 気付けるようにするため。一覧の実体と根拠は
 * tests/upstream-e2e/known-divergences.json を参照。
 */
// このファイルは実行のたびに本家の取得先 (`.cache/misskey/<版>/packages/backend/test/`)
// へコピーされる (#3378) ので、自分の場所からの相対パスでは mk-go のリポジトリを
// 指せない。Makefile が渡す MKGO_CWD (= mk-go のリポジトリ) から組み立てる。
const mkgoCwd = process.env.MKGO_CWD;
if (!mkgoCwd) {
	throw new Error('MKGO_CWD is not set (run via `make upstream-e2e-test`)');
}
const knownDivergencesPath = resolve(mkgoCwd, 'tests/upstream-e2e/known-divergences.json');

function loadKnownDivergences(): Map<string, Set<string>> {
	const parsed = JSON.parse(readFileSync(knownDivergencesPath, 'utf8')) as {
		divergences: { file: string; reason: string; names: string[] }[];
	};
	const byFile = new Map<string, Set<string>>();
	for (const entry of parsed.divergences) {
		const names = byFile.get(entry.file) ?? new Set<string>();
		for (const name of entry.names) names.add(name);
		byFile.set(entry.file, names);
	}
	return byFile;
}

const knownDivergences = loadKnownDivergences();

// describe を ' > ' で連ねたフルネーム。一覧の name と突き合わせる。
function fullTestName(task: any): string {
	const parts: string[] = [];
	for (let node = task; node != null; node = node.suite) {
		if (typeof node.name === 'string' && node.name !== '') parts.unshift(node.name);
	}
	return parts.join(' > ');
}

function collectTests(suite: any, out: any[] = []): any[] {
	for (const task of suite.tasks ?? []) {
		if (task.type === 'test') out.push(task);
		else if (task.type === 'suite') collectTests(task, out);
	}
	return out;
}

// テストごとに、一覧の name として受け付ける名前。フルネームに加え、同じフルネームの
// テストがファイル内に複数あるときは、宣言の順で何番目か (1 始まり) を付けた
// `<フルネーム> #N` も受け付ける。vitest 5 は test.each の `$label` の後ろに続く
// 日本語まで変数名として読む (`$labelこと` は `labelこと` を引いて undefined になる)
// ので、本家の行が全て同じ名前になり、名前だけでは 1 件を指せない。
function acceptedNamesByTaskId(file: any): Map<string, string[]> {
	const tests = collectTests(file);
	const counts = new Map<string, number>();
	for (const task of tests) {
		const name = fullTestName(task);
		counts.set(name, (counts.get(name) ?? 0) + 1);
	}
	const seen = new Map<string, number>();
	const byId = new Map<string, string[]>();
	for (const task of tests) {
		const name = fullTestName(task);
		const accepted = [name];
		if ((counts.get(name) ?? 0) > 1) {
			const ordinal = (seen.get(name) ?? 0) + 1;
			seen.set(name, ordinal);
			accepted.push(`${name} #${ordinal}`);
		}
		byId.set(task.id, accepted);
	}
	return byId;
}

function divergencesFor(file: string): Set<string> | undefined {
	for (const [suffix, names] of knownDivergences) {
		if (file.endsWith(suffix)) return names;
	}
	return undefined;
}

let acceptedNames = new Map<string, string[]>();

// vitest 5 の hook は第 1 引数に fixture の文脈 (分割代入の形でないと落ちる)、
// 第 2 引数に suite (ここではファイル) を渡す。
beforeAll(({}, file: any) => {
	acceptedNames = acceptedNamesByTaskId(file);
	const names = typeof file?.name === 'string' ? divergencesFor(file.name) : undefined;
	if (names == null) return;
	// 一覧の名前が、どのテストにも当たらなければ落とす。当たらない名前は expected-failure
	// にならないまま黙って残り、vitest の名前の付け方が変わったとき (4 → 5 で、引用符が
	// 外れ、`$labelこと` が undefined になった) に、既知の乖離が本物の失敗として出た。
	const all = new Set([...acceptedNames.values()].flat());
	const stale = [...names].filter(name => !all.has(name));
	if (stale.length > 0) {
		throw new Error(`known-divergences.json has names that match no test in ${file.name}: ${JSON.stringify(stale)}`);
	}
});

beforeEach((ctx: any) => {
	const file = ctx.task?.file?.name;
	if (typeof file !== 'string') return;
	const names = divergencesFor(file);
	if (names == null) return;
	const accepted = acceptedNames.get(ctx.task.id) ?? [fullTestName(ctx.task)];
	if (accepted.some(name => names.has(name))) {
		ctx.task.fails = true;
	}
});

async function resetDb(): Promise<void> {
	const res = await fetch(`http://127.0.0.1:${config.port}/api/reset-db`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: '{}',
	});

	// 本家同様、失敗したら即座に落とす。半端な状態でテストを続けると原因の
	// 切り分けが困難になる。
	if (!res.ok) {
		throw new Error(`reset-db failed with status ${res.status}`);
	}
}

beforeAll(async () => {
	// 本家と同じ順序: 先にサーバーを作り直してから DB を掃除する。
	await sendEnvResetRequest();
	await resetDb();
});
