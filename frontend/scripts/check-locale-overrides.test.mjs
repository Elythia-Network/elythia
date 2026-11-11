/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { compareLanguage, flatten, parseOverrides, REQUIRED_OVERRIDE, run } from './check-locale-overrides.mjs';

/**
 * Runs the script on a temporary frontend/ with the given upstream words, Elythia's words and list.
 *
 * @param {Record<string, string>} upstream the YAML of each upstream locale file
 * @param {Record<string, string>} own the YAML of each Elythia locale file
 * @param {string} list overrides.tsv
 */
function runOn(upstream, own, list) {
	const root = mkdtempSync(join(tmpdir(), 'locale-overrides-'));
	try {
		mkdirSync(join(root, 'locales'));
		mkdirSync(join(root, 'locales-elythia'));
		for (const [file, text] of Object.entries(upstream)) writeFileSync(join(root, 'locales', file), text);
		for (const [file, text] of Object.entries(own)) writeFileSync(join(root, 'locales-elythia', file), text);
		writeFileSync(join(root, 'locales-elythia', 'overrides.tsv'), list);
		/** @type {string[]} */
		const errors = [];
		/** @type {string[]} */
		const logs = [];
		const code = run(root, { log: line => logs.push(line), error: line => errors.push(line) }, Object.keys(own).sort());
		return { code, errors, logs };
	} finally {
		rmSync(root, { recursive: true, force: true });
	}
}

const upstream = {
	'ja-JP.yml': `${REQUIRED_OVERRIDE}: "ようこそMisskey"\n_s:\n  a: "A"\n  b: "B"\n`,
	'en-US.yml': `${REQUIRED_OVERRIDE}: "Welcome to Misskey"\n_s:\n  a: "A"\n  b: "B"\n`,
};
const own = {
	'ja-JP.yml': `${REQUIRED_OVERRIDE}: "ようこそElythia"\n_s:\n  c: "C"\n_mkgoX:\n  y: "Y"\n`,
	'en-US.yml': `${REQUIRED_OVERRIDE}: "Welcome to Elythia"\n_s:\n  c: "C"\n_mkgoX:\n  y: "Y"\n`,
};
const list = `# comment\n${REQUIRED_OVERRIDE}\tname\n`;

test('flatten lists strings by dotted path', () => {
	assert.deepEqual([...flatten({ a: 'A', _s: { b: 'B', _t: { c: '' } } })], [['a', 'A'], ['_s.b', 'B'], ['_s._t.c', '']]);
});

test('flatten keeps values that are not strings, including empty sections and lists', () => {
	assert.deepEqual([...flatten({ n: null, e: {}, l: [], f: ['x'], _s: { z: 0 } })], [['n', null], ['e', {}], ['l', []], ['f', ['x']], ['_s.z', 0]]);
});

test('parseOverrides reads key and reason, and rejects malformed or repeated lines', () => {
	const parsed = parseOverrides('# c\n\nk1\tr1\nk2\n\tr\nk1\tr2\nk3\tr\textra\n');
	assert.deepEqual([...parsed.entries], [['k1', 'r1']]);
	assert.equal(parsed.malformed.length, 4);
	assert.match(parsed.malformed.join('\n'), /k1 is listed twice/);
});

test('compareLanguage tells rewritten, same, and empty-string words apart', () => {
	const up = flatten({ a: 'A', b: 'B', _s: { x: 'X' }, str: 'S' });
	const result = compareLanguage(up, flatten({ a: 'A2', b: 'B', added: 'N', gone: '', c: '', _s: 'replaced', str: { d: 'D' } }));
	assert.deepEqual(result.overridden.sort(), ['_s', 'a', 'str.d']);
	assert.deepEqual(result.same, ['b']);
	assert.deepEqual(result.removesNothing.sort(), ['c', 'gone']);
});

test('passes when every rewritten upstream word is listed', () => {
	const result = runOn(upstream, own, list);
	assert.equal(result.code, 0, result.errors.join('\n'));
	assert.match(result.logs.join('\n'), /2 files, 1 upstream keys rewritten or removed, all listed/);
});

test('fails when an unlisted upstream word is rewritten', () => {
	const rewritten = { ...own, 'en-US.yml': `${own['en-US.yml']}_s:\n  a: "A2"\n`.replace('_s:\n  c: "C"\n', '') };
	const result = runOn(upstream, rewritten, list);
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /en-US\.yml: _s\.a rewrites or removes an upstream word/);
});

test('fails when an unlisted upstream word is removed with an empty string', () => {
	const removed = { ...own, 'ja-JP.yml': own['ja-JP.yml'].replace('  c: "C"\n', '  c: "C"\n  b: ""\n') };
	const result = runOn(upstream, removed, list);
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /ja-JP\.yml: _s\.b rewrites or removes an upstream word/);
	// 一覧にあれば消してよい
	assert.equal(runOn(upstream, removed, `${list}_s.b\treason\n`).code, 0);
});

test('fails when a word is the same as upstream, or an empty string removes nothing', () => {
	const same = runOn(upstream, { ...own, 'ja-JP.yml': `${own['ja-JP.yml']}other: ""\n`.replace('  c: "C"\n', '  c: "C"\n  a: "A"\n') }, list);
	assert.equal(same.code, 1);
	assert.match(same.errors.join('\n'), /_s\.a is the same as the upstream word/);
	assert.match(same.errors.join('\n'), /other is an empty string, but upstream has no such word/);
});

test('fails when a listed key is not rewritten anywhere', () => {
	const result = runOn(upstream, own, `${list}_s.b\tstale\n`);
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /_s\.b is listed but no language rewrites or removes it/);
});

test('fails when the build does not read the language, even if upstream has the file', () => {
	const result = runOn(upstream, { ...own, 'xx-XX.yml': 'a: "A"\n' }, list);
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /xx-XX\.yml: the build does not read this language/);

	// 本家にファイルはあるが、const.ts の languages に無い (bn-BD など)
	const unread = runOn({ ...upstream, 'bn-BD.yml': upstream['en-US.yml'] }, { ...own, 'bn-BD.yml': own['en-US.yml'] }, list);
	assert.equal(unread.code, 1);
	assert.match(unread.errors.join('\n'), /bn-BD\.yml: the build does not read this language/);
});

test('fails when a value is not a string: an empty section or list replaces an upstream word, or null breaks the build', () => {
	for (const [value, key] of [['{}', 'logout'], ['[]', 'logout'], ['~', 'logout'], ['{}', '_s'], ['~', 'added']]) {
		const result = runOn(upstream, { ...own, 'ja-JP.yml': `${own['ja-JP.yml']}${key}: ${value}\n`.replace(key === '_s' ? /^_s:\n  c: "C"\n/m : /^$^/, '') }, list);
		assert.equal(result.code, 1, `${key}: ${value}`);
		assert.match(result.errors.join('\n'), new RegExp(`ja-JP\\.yml: ${key} is not a string`), `${key}: ${value}`);
	}
	// 本家にある語を空の節で置き換えるのは、一覧に無ければ書き換えとしても落とす
	const replaced = runOn({ ...upstream, 'ja-JP.yml': `${upstream['ja-JP.yml']}logout: "L"\n` }, { ...own, 'ja-JP.yml': `${own['ja-JP.yml']}logout: {}\n` }, list);
	assert.match(replaced.errors.join('\n'), /ja-JP\.yml: logout rewrites or removes an upstream word/);
});

test('cannot be trusted when ja-JP or en-US is missing, the marker is not found, or the list is malformed', () => {
	const noEn = runOn(upstream, { 'ja-JP.yml': own['ja-JP.yml'] }, list);
	assert.equal(noEn.code, 2);
	assert.match(noEn.errors.join('\n'), /locales-elythia\/en-US\.yml was not read/);

	const noMarker = runOn(upstream, {
		'ja-JP.yml': own['ja-JP.yml'].replace(/^introMisskey.*\n/m, ''),
		'en-US.yml': own['en-US.yml'].replace(/^introMisskey.*\n/m, ''),
	}, '');
	assert.equal(noMarker.code, 2);
	assert.match(noMarker.errors.join('\n'), /introMisskey was not found rewritten/);

	const malformed = runOn(upstream, own, 'no-reason\n');
	assert.equal(malformed.code, 2);
});
