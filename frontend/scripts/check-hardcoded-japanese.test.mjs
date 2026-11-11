/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';

import { japaneseInScript, japaneseInVue, normalize, parseAllowlist, run, scriptKindOf } from './check-hardcoded-japanese.mjs';

/** @param {{ line: number, text: string }[]} found */
const texts = (found) => found.map((f) => f.text);

test('normalize collapses whitespace', () => {
	assert.equal(normalize('  こんにちは\n\t世界  '), 'こんにちは 世界');
});

test('japaneseInScript finds string and template literals, and skips comments and non-Japanese strings', () => {
	const source = [
		"// コメントの日本語",
		"/* ブロックの日本語 */",
		"const a = 'こんにちは';",
		'const b = "English";',
		'const c = `前${x}後`;',
		'const d = <div>本文</div>;',
	].join('\n');
	const found = japaneseInScript(source, 0, 'tsx');
	assert.deepEqual(texts(found), ['こんにちは', '前', '後', '本文']);
	assert.equal(found[0].line, 3);
	assert.equal(japaneseInScript("x = 'あ';", 10)[0].line, 11);
});

test('japaneseInScript finds every part of a template literal, including one without substitutions', () => {
	assert.deepEqual(texts(japaneseInScript('const a = `置換なし`;')), ['置換なし']);
	assert.deepEqual(texts(japaneseInScript('const a = `一${x}二${y}三`;')), ['一', '二', '三']);
});

test('japaneseInScript finds kana of every width, rare and compatibility kanji, and Japanese punctuation or fullwidth forms', () => {
	const found = texts(japaneseInScript("x = ['ﾆｬ', '\u{2000B}', '\uFA11', '「」', 'ＡＢＣ', 'abc', '\u3000'];"));
	assert.deepEqual(found, ['ﾆｬ', '\u{2000B}', '\uFA11', '「」', 'ＡＢＣ']);
});

test('japaneseInScript reads .ts as TypeScript, so a generic arrow function does not turn the comments after it into text', () => {
	const source = 'const scope = <T>(fn: () => T) => {\n\t// 読み込み中の数を数える\n\treturn fn();\n};\nconst y = <string>z; // 型の主張\n';
	assert.deepEqual(japaneseInScript(source, 0, 'ts'), []);
	assert.deepEqual(japaneseInScript(source), [], 'the default must be TypeScript, not TSX');
	assert.deepEqual(texts(japaneseInScript('const d = <div>本文</div>;', 0, 'tsx')), ['本文']);
	assert.equal(scriptKindOf('a.tsx'), 'tsx');
	assert.equal(scriptKindOf('a.ts'), 'ts');
	assert.equal(scriptKindOf('a.mjs'), 'js');
});

test('japaneseInVue reads lang="ts" as TypeScript and lang="tsx" as TSX', () => {
	const generic = '<script setup lang="ts" generic="T">\nconst scope = <U>(fn: () => U) => {\n\t// 読み込み中の数を数える\n\treturn fn();\n};\n</script>\n';
	assert.deepEqual(japaneseInVue(generic), []);
	assert.deepEqual(texts(japaneseInVue('<script setup lang="tsx">\nconst d = <div>本文</div>;\n</script>\n')), ['本文']);
	assert.deepEqual(texts(japaneseInVue('<script setup>\nconst d = <div>JSの本文</div>;\n</script>\n')), ['JSの本文']);
});

test('japaneseInScript finds kana extensions, enclosed forms and the newest kanji, and skips halfwidth arrows', () => {
	assert.deepEqual(texts(japaneseInScript("x = ['\u31F0', '㈱', '㍻', '\u{1B001}', '\u{30000}', '\uFFE9', '\uFFED'];")), ['\u31F0', '㈱', '㍻', '\u{1B001}', '\u{30000}']);
});

test('japaneseInVue skips keywords only on SearchMarker', () => {
	assert.deepEqual(texts(japaneseInVue('<template><SearchMarker :keywords="[\'検索語\']"/><MkFoo :keywords="[\'出る語\']"/></template>\n')), ['出る語']);
});

test('japaneseInVue finds text, static attributes, expressions and scripts, and skips comments and search words', () => {
	const source = [
		'<template>',
		'<div title="属性">',
		'	<!-- コメント -->',
		'	本文',
		"	{{ ok ? 'はい' : 'いいえ' }}",
		"	<MkButton :label=\"'束縛'\" @click=\"say('処理')\"/>",
		"	<SearchMarker :keywords=\"['検索語']\"><span>{{ i18n.ts.ok }}</span></SearchMarker>",
		'</div>',
		'</template>',
		'<script lang="ts" setup>',
		"const s = 'スクリプト';",
		'</script>',
	].join('\n');
	const found = japaneseInVue(source);
	assert.deepEqual(texts(found).sort(), ['いいえ', 'はい', 'スクリプト', '処理', '属性', '本文', '束縛'].sort());
	assert.equal(found.find((f) => f.text === 'スクリプト')?.line, 11);
	assert.equal(found.find((f) => f.text === '本文')?.line, 4);
});

test('japaneseInVue reads the plain script block too', () => {
	assert.deepEqual(texts(japaneseInVue("<script lang=\"ts\">\nexport default { name: '名前' };\n</script>\n")), ['名前']);
});

test('parseAllowlist reads file, text and reason, and rejects malformed or repeated lines', () => {
	const parsed = parseAllowlist('# c\n\na.vue\t*\tall\na.vue\tあ\treason\nb.vue\tい\nc.vue\tう\tr\textra\na.vue\tあ\tagain\nd.vue\tえ\t  \n');
	assert.deepEqual([...parsed.entries.keys()], ['a.vue\t*', 'a.vue\tあ']);
	assert.equal(parsed.malformed.length, 4);
	assert.match(parsed.malformed.join('\n'), /a\.vue あ is listed twice/);
});

/**
 * Runs the script on a temporary frontend/ with the given files and allowlist.
 *
 * @param {Record<string, string>} files
 * @param {string} allowlist
 * @param {{ minFiles: number, requiredFiles: string[] }} [bounds]
 */
function runOn(files, allowlist, bounds = { minFiles: 1, requiredFiles: ['packages/frontend/src/a.vue'] }) {
	const root = mkdtempSync(join(tmpdir(), 'hardcoded-japanese-'));
	try {
		for (const [path, text] of Object.entries(files)) {
			mkdirSync(dirname(join(root, path)), { recursive: true });
			writeFileSync(join(root, path), text);
		}
		writeFileSync(join(root, 'allowlist.tsv'), allowlist);
		/** @type {string[]} */
		const errors = [];
		/** @type {string[]} */
		const logs = [];
		const code = run(root, { log: (l) => logs.push(l), error: (l) => errors.push(l) }, Object.keys(files), join(root, 'allowlist.tsv'), bounds);
		return { code, errors, logs };
	} finally {
		rmSync(root, { recursive: true, force: true });
	}
}

const ok = { 'packages/frontend/src/a.vue': '<template><div>{{ i18n.ts.ok }}</div></template>\n' };

test('passes when no Japanese string is written directly', () => {
	const result = runOn(ok, '');
	assert.equal(result.code, 0, result.errors.join('\n'));
});

test('fails on a Japanese string in a template or a script, with the file and line', () => {
	const result = runOn({ ...ok, 'packages/frontend/src/b.vue': '<template>\n<div>直書き</div>\n</template>\n', 'packages/frontend/src/c.ts': "\nexport const t = 'スクリプト';\n" }, '');
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /packages\/frontend\/src\/b\.vue:2: "直書き" is written directly/);
	assert.match(result.errors.join('\n'), /packages\/frontend\/src\/c\.ts:2: "スクリプト" is written directly/);
});

test('the allowlist allows a string or a whole file, and fails on entries that are not used', () => {
	const files = { ...ok, 'packages/frontend/src/b.vue': '<template><div>名前</div><div>別</div></template>\n', 'packages/frontend/src/c.ts': "export const t = ['甲', '乙'];\n" };
	assert.equal(runOn(files, 'packages/frontend/src/b.vue\t名前\tr\npackages/frontend/src/b.vue\t別\tr\npackages/frontend/src/c.ts\t*\tr\n').code, 0);

	const partly = runOn(files, 'packages/frontend/src/b.vue\t名前\tr\npackages/frontend/src/c.ts\t*\tr\n');
	assert.equal(partly.code, 1);
	assert.match(partly.errors.join('\n'), /"別" is written directly/);

	const unused = runOn(files, 'packages/frontend/src/b.vue\t名前\tr\npackages/frontend/src/b.vue\t別\tr\npackages/frontend/src/c.ts\t*\tr\npackages/frontend/src/a.vue\t消えた\tr\n');
	assert.equal(unused.code, 1);
	assert.match(unused.errors.join('\n'), /packages\/frontend\/src\/a\.vue 消えた is listed but not found/);
});

test('run reads .ts files as TypeScript, so comments after a generic arrow function do not fail', () => {
	const result = runOn({ ...ok, 'packages/frontend/src/e.ts': 'export const scope = <T>(fn: () => T) => {\n\t// 読み込み中の数を数える\n\treturn fn();\n};\n' }, '');
	assert.equal(result.code, 0, result.errors.join('\n'));
});

test('cannot be trusted with too few files, a missing named file, a malformed allowlist, or a file that does not parse', () => {
	const few = runOn(ok, '', { minFiles: 2, requiredFiles: [] });
	assert.equal(few.code, 2);
	assert.match(few.errors.join('\n'), /only 1 files were read \(expected at least 2\)/);

	const missing = runOn(ok, '', { minFiles: 1, requiredFiles: ['packages/frontend/src/z.vue'] });
	assert.equal(missing.code, 2);
	assert.match(missing.errors.join('\n'), /packages\/frontend\/src\/z\.vue was not read/);

	assert.equal(runOn(ok, 'no-tabs\n').code, 2);

	const broken = runOn({ ...ok, 'packages/frontend/src/b.vue': '<template><div></template>\n' }, '');
	assert.equal(broken.code, 2);
	assert.match(broken.errors.join('\n'), /b\.vue: cannot parse/);
});
