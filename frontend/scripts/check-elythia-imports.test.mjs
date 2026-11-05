/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { check, copyrightHolders, importSpecifiers, MIN_ELYTHIA_FILES, REQUIRED_ELYTHIA_FILES, run, trustProblems } from './check-elythia-imports.mjs';

const scriptsDir = fileURLToPath(new URL('.', import.meta.url));

const ELYTHIA_HEADER = '/*\n * SPDX-FileCopyrightText: Elythia-Network\n * SPDX-License-Identifier: AGPL-3.0-only\n */\n\n';
const UPSTREAM_HEADER = '/*\n * SPDX-FileCopyrightText: syuilo and misskey-project\n * SPDX-License-Identifier: AGPL-3.0-only\n */\n\n';
const BOTH_HEADER = '/*\n * SPDX-FileCopyrightText: syuilo and misskey-project\n * SPDX-FileCopyrightText: Elythia-Network\n * SPDX-License-Identifier: AGPL-3.0-only\n */\n\n';
const VUE_ELYTHIA_HEADER = '<!--\nSPDX-FileCopyrightText: Elythia-Network\nSPDX-License-Identifier: AGPL-3.0-only\n-->\n\n';

/**
 * @param {string} path
 * @param {string} specifier
 * @returns {string} an Elythia file at path that imports specifier
 */
function elythiaFileImporting(path, specifier) {
	return path.endsWith('.vue')
		? `${VUE_ELYTHIA_HEADER}<script setup lang="ts">\nimport type * as E from '${specifier}';\n</script>\n`
		: `${ELYTHIA_HEADER}import type * as E from '${specifier}';\n`;
}

/** @returns {{ path: string, text: string }[]} the files that satisfy the lower bound */
function lowerBoundFiles() {
	return [
		...Object.entries(REQUIRED_ELYTHIA_FILES).map(([path, specifier]) => ({ path, text: elythiaFileImporting(path, specifier) })),
		...Array.from({ length: MIN_ELYTHIA_FILES }, (_, i) => ({ path: `packages/frontend/src/f${i}.ts`, text: ELYTHIA_HEADER })),
	];
}

test('only the copyright holders in the comments before the first code count', () => {
	assert.deepEqual(copyrightHolders(ELYTHIA_HEADER), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders(VUE_ELYTHIA_HEADER, 'x.vue'), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders(BOTH_HEADER), ['syuilo and misskey-project', 'Elythia-Network']);
	assert.deepEqual(copyrightHolders(`#!/usr/bin/env node\n${ELYTHIA_HEADER}`), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders('// SPDX-FileCopyrightText: Elythia-Network\n// SPDX-License-Identifier: AGPL-3.0-only\n'), ['Elythia-Network']);
	// 本文の fixture にある SPDX の行は見出しではない
	assert.deepEqual(copyrightHolders(`${ELYTHIA_HEADER}const s = \`\n * SPDX-FileCopyrightText: syuilo and misskey-project\n\`;\n`), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders('export {};\n/*\n * SPDX-FileCopyrightText: Elythia-Network\n */\n'), []);
	// コードより前なら、見出しの前に別のコメントがあってもよい
	assert.deepEqual(copyrightHolders(`// @ts-check\n\n${ELYTHIA_HEADER}`), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders(`\uFEFF${ELYTHIA_HEADER}`), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders(ELYTHIA_HEADER.replaceAll('\n', '\r\n')), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders(`<!-- note -->\n${VUE_ELYTHIA_HEADER}<template><div/></template>\n`, 'x.vue'), ['Elythia-Network']);
	assert.deepEqual(copyrightHolders(`<template><div/></template>\n${VUE_ELYTHIA_HEADER}`, 'x.vue'), []);
	// 値が空の行で、次の行を著作権者として読まない
	assert.deepEqual(copyrightHolders('/*\n * SPDX-FileCopyrightText:\n */\n'), []);
});

test('every import form that reaches a module is found', () => {
	const source = [
		'import a from \'m-default\';',
		'import type * as B from \'m-type\';',
		'import \'m-side-effect\';',
		'export * from \'m-reexport\';',
		'export { c } from \'m-named-reexport\';',
		'import d = require(\'m-import-equals\');',
		'const e = await import(\'m-dynamic\');',
		'const f = require(\'m-require\');',
		'type G = import(\'m-type-query\').G;',
		'/** @type {import(\'m-jsdoc\').H} */',
		'const h = 1;',
		'/** @import { I } from \'m-jsdoc-import\' */',
		'vi.mock(\'m-mocked\');',
		'await vi.importActual(\'m-actual\');',
	].join('\n');
	assert.deepEqual(importSpecifiers('x.ts', source).sort(), [
		'm-default', 'm-dynamic', 'm-import-equals', 'm-jsdoc', 'm-jsdoc-import', 'm-named-reexport', 'm-reexport',
		'm-require', 'm-side-effect', 'm-type', 'm-type-query',
	]);
	assert.deepEqual(importSpecifiers('x.mjs', '/** @param {import(\'m-jsdoc-js\').T} y */\nexport function f(y) {}\n'), ['m-jsdoc-js']);
});

test('both script blocks and the generic attribute of a .vue file are read', () => {
	const vue = `${VUE_ELYTHIA_HEADER}<template><div/></template>\n<script lang="ts">\nimport x from 'm-script';\n</script>\n<script lang="ts" setup generic="T extends import('m-generic').X">\nimport type * as Y from 'm-setup';\n</script>\n`;
	assert.deepEqual(importSpecifiers('x.vue', vue).sort(), ['m-generic', 'm-script', 'm-setup']);
});

test('Elythia files that import misskey-js are reported, and other files are not', () => {
	const result = check([
		{ path: 'packages/frontend/src/a.ts', text: `${ELYTHIA_HEADER}import type * as Misskey from 'misskey-js';\n` },
		{ path: 'packages/frontend/src/b.ts', text: `${ELYTHIA_HEADER}import { acct } from 'misskey-js/built/acct.js';\n` },
		{ path: 'packages/frontend/src/c.vue', text: `${VUE_ELYTHIA_HEADER}<script setup lang="ts">\nimport * as Misskey from 'misskey-js';\n</script>\n` },
		{ path: 'scripts/d.mjs', text: `${ELYTHIA_HEADER}import * as Misskey from 'misskey-js';\n` },
		{ path: 'packages/frontend/src/ok.ts', text: `${ELYTHIA_HEADER}import type * as Elythia from 'elythia-js';\nimport { x } from 'misskey-js-like';\n` },
		{ path: 'packages/frontend/src/upstream.ts', text: `${UPSTREAM_HEADER}import * as Misskey from 'misskey-js';\n` },
		{ path: 'packages/frontend/src/both.ts', text: `${BOTH_HEADER}import * as Misskey from 'misskey-js';\n` },
		{ path: 'packages/elythia-js/src/bridge.ts', text: `${ELYTHIA_HEADER}export * from 'misskey-js';\n` },
		{ path: 'packages/frontend/src/notes.md', text: `${ELYTHIA_HEADER}import 'misskey-js';\n` },
	]);
	assert.deepEqual(result.violations, [
		{ path: 'packages/frontend/src/a.ts', specifier: 'misskey-js' },
		{ path: 'packages/frontend/src/b.ts', specifier: 'misskey-js/built/acct.js' },
		{ path: 'packages/frontend/src/c.vue', specifier: 'misskey-js' },
		{ path: 'scripts/d.mjs', specifier: 'misskey-js' },
	]);
	assert.equal(result.elythiaFiles, 5);
	assert.deepEqual(result.malformed, []);
});

test('a run is not trusted when it found too little or an Elythia holder is misspelled', () => {
	const files = lowerBoundFiles();
	assert.deepEqual(trustProblems(check(files)), []);

	assert.match(trustProblems(check(files.slice(0, 3))).join('\n'), /Elythia files were found/);

	for (const [i, [path]] of Object.entries(REQUIRED_ELYTHIA_FILES).entries()) {
		const rest = files.filter((_, j) => j !== i);
		assert.match(trustProblems(check(rest)).join('\n'), new RegExp(`${path} was not recognized`));
		const withoutImport = { path, text: elythiaFileImporting(path, 'something-else') };
		assert.match(trustProblems(check([withoutImport, ...rest])).join('\n'), new RegExp(`${path} should import`));
	}

	const misspelled = { path: 'packages/frontend/src/m.ts', text: ELYTHIA_HEADER.replace('Elythia-Network', '2026 Elythia-Network') };
	assert.match(trustProblems(check([...files, misspelled])).join('\n'), /'2026 Elythia-Network' should be exactly 'Elythia-Network'/);
	const lowercase = { path: 'packages/frontend/src/l.ts', text: ELYTHIA_HEADER.replace('Elythia-Network', 'elythia-network') };
	assert.match(trustProblems(check([...files, lowercase])).join('\n'), /'elythia-network' should be exactly/);

	const afterCode = { path: 'packages/frontend/src/u.ts', text: `'use strict';\n${ELYTHIA_HEADER}import 'misskey-js';\n` };
	assert.match(trustProblems(check([...files, afterCode])).join('\n'), /u\.ts: the SPDX header comes after code/);
	const vueAfterTemplate = { path: 'packages/frontend/src/v.vue', text: `<template><div/></template>\n${VUE_ELYTHIA_HEADER}<script setup lang="ts">\nimport 'misskey-js';\n</script>\n` };
	assert.match(trustProblems(check([...files, vueAfterTemplate])).join('\n'), /v\.vue: the SPDX header comes after code/);
	// 本家のファイルの本文にある SPDX の行 (fixture など) では落とさない
	const upstreamFixture = { path: 'packages/frontend/src/fixture.ts', text: 'export const s = `\n * SPDX-FileCopyrightText: syuilo and misskey-project\n`;\n' };
	assert.deepEqual(trustProblems(check([...files, upstreamFixture])), []);
});

/**
 * Creates a throwaway git repository with the given files committed.
 *
 * @param {import('node:test').TestContext} t
 * @param {{ path: string, text: string }[]} files
 * @returns {string}
 */
function fakeRepo(t, files) {
	const root = mkdtempSync(join(tmpdir(), 'check-elythia-imports-'));
	t.after(() => rmSync(root, { recursive: true, force: true }));
	for (const { path, text } of files) {
		mkdirSync(dirname(join(root, path)), { recursive: true });
		writeFileSync(join(root, path), text);
	}
	const git = (/** @type {string[]} */ ...args) => execFileSync('git', ['-c', 'user.name=t', '-c', 'user.email=t@example.com', '-c', 'commit.gpgSign=false', ...args], { cwd: root, stdio: 'pipe' });
	git('init', '-q');
	git('add', '-A');
	git('commit', '-q', '-m', 'init');
	return root;
}

/**
 * @param {string} root
 * @returns {{ code: number, log: string[], error: string[] }}
 */
function runIn(root) {
	/** @type {string[]} */
	const log = [];
	/** @type {string[]} */
	const error = [];
	const code = run(root, { log: line => log.push(line), error: line => error.push(line) });
	return { code, log, error };
}

test('the exit code is 0 for a clean repository, and files deleted from the work tree are skipped', (t) => {
	const root = fakeRepo(t, [...lowerBoundFiles(), { path: 'packages/frontend/src/gone.ts', text: ELYTHIA_HEADER }]);
	unlinkSync(join(root, 'packages/frontend/src/gone.ts'));
	const { code, log, error } = runIn(root);
	assert.deepEqual(error, []);
	assert.equal(code, 0);
	assert.match(log.join('\n'), /none imports misskey-js/);
});

test('the exit code is 1 for a violation anywhere in the repository, with the file named', (t) => {
	const root = fakeRepo(t, [...lowerBoundFiles(), { path: 'scripts/x.mjs', text: `${ELYTHIA_HEADER}import 'misskey-js';\n` }]);
	const { code, error } = runIn(root);
	assert.equal(code, 1);
	assert.match(error.join('\n'), /scripts\/x\.mjs: imports 'misskey-js'/);
});

test('the script runs its check when started through a symlink', (t) => {
	const dir = mkdtempSync(join(tmpdir(), 'check-elythia-imports-link-'));
	t.after(() => rmSync(dir, { recursive: true, force: true }));
	symlinkSync(join(scriptsDir, '..'), join(dir, 'frontend'));
	const result = spawnSync(process.execPath, [join(dir, 'frontend', 'scripts', 'check-elythia-imports.mjs')], { encoding: 'utf8' });
	// 実リポジトリの状態 (違反の有無) に左右されないよう、検査が走って何かを報告したことだけを見る
	assert.match(`${result.stdout}${result.stderr}`, /^check-elythia-imports:|imports 'misskey-js'/m);
});

test('the exit code is 2 when the lower bound fails, and violations are still shown', (t) => {
	const root = fakeRepo(t, [{ path: 'packages/frontend/src/x.ts', text: `${ELYTHIA_HEADER}import 'misskey-js';\n` }]);
	const { code, error } = runIn(root);
	assert.equal(code, 2);
	assert.match(error.join('\n'), /x\.ts: imports 'misskey-js'/);
	assert.match(error.join('\n'), /Elythia files were found/);
});
