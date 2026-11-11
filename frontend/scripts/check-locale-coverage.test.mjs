/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import {
	buildAllowlist, check, ELYTHIA_MARKERS, hasString, leafKeys, MIN_KEYS, parseAllowlist, REQUIRED_KEYS, run, trustProblems,
	UPSTREAM_UNTRANSLATED_REASON,
} from './check-locale-coverage.mjs';

/**
 * Builds a nested locale from dotted keys.
 *
 * @param {Record<string, string>} flat
 */
function locale(flat) {
	/** @type {Record<string, any>} */
	const root = {};
	for (const [path, value] of Object.entries(flat)) {
		const keys = path.split('.');
		let node = root;
		for (const key of keys.slice(0, -1)) node = node[key] ??= {};
		node[keys.at(-1)] = value;
	}
	return root;
}

/** @returns {Record<string, string>} strings that satisfy the lower bound */
function lowerBound() {
	/** @type {Record<string, string>} */
	const flat = Object.fromEntries(REQUIRED_KEYS.map(key => [key, key]));
	for (let i = 0; i < MIN_KEYS; i++) flat[`filler.k${i}`] = 'x';
	return flat;
}

/**
 * @param {Record<string, string>} flat
 * @returns {string} the locale as YAML (one quoted string per line, as the locales are written)
 */
function toYaml(flat) {
	/** @type {string[]} */
	const lines = [];
	/** @param {Record<string, any>} node @param {number} depth */
	const emit = (node, depth) => {
		for (const [key, value] of Object.entries(node)) {
			if (typeof value === 'object') {
				lines.push(`${'  '.repeat(depth)}${key}:`);
				emit(value, depth + 1);
			} else {
				lines.push(`${'  '.repeat(depth)}${key}: ${JSON.stringify(value)}`);
			}
		}
	};
	emit(locale(flat), 0);
	return lines.join('\n') + '\n';
}

/**
 * Runs the script on a temporary frontend/ with the given locales and allowlist.
 *
 * @param {Record<string, string>} ja
 * @param {Record<string, string>} en
 * @param {string} allowlist
 * @param {string[]} [args]
 * @param {{ ja: Record<string, string>, en: Record<string, string> }} [upstream]
 * @param {{ ja?: Record<string, string>, en?: Record<string, string> }} [own] Elythia's words (locales-elythia/)
 */
function runOn(ja, en, allowlist, args = [], upstream, own) {
	const root = mkdtempSync(join(tmpdir(), 'locale-coverage-'));
	try {
		mkdirSync(join(root, 'locales'));
		mkdirSync(join(root, 'scripts'));
		writeFileSync(join(root, 'locales', 'ja-JP.yml'), toYaml(ja));
		writeFileSync(join(root, 'locales', 'en-US.yml'), toYaml(en));
		if (own != null) {
			mkdirSync(join(root, 'locales-elythia'));
			if (own.ja != null) writeFileSync(join(root, 'locales-elythia', 'ja-JP.yml'), toYaml(own.ja));
			if (own.en != null) writeFileSync(join(root, 'locales-elythia', 'en-US.yml'), toYaml(own.en));
		}
		writeFileSync(join(root, 'scripts', 'locale-coverage-allowlist.tsv'), allowlist);
		const fullArgs = [...args];
		if (upstream != null) {
			mkdirSync(join(root, 'upstream'));
			writeFileSync(join(root, 'upstream', 'ja-JP.yml'), toYaml(upstream.ja));
			writeFileSync(join(root, 'upstream', 'en-US.yml'), toYaml(upstream.en));
			fullArgs.push(join(root, 'upstream'));
		}
		/** @type {string[]} */
		const errors = [];
		/** @type {string[]} */
		const logs = [];
		const code = run(root, fullArgs, { log: line => logs.push(line), error: line => errors.push(line) });
		return { code, errors, logs, allowlist: readFileSync(join(root, 'scripts', 'locale-coverage-allowlist.tsv'), 'utf8') };
	} finally {
		rmSync(root, { recursive: true, force: true });
	}
}

test('leafKeys lists nested strings as dotted paths in order', () => {
	assert.deepEqual(leafKeys(locale({ a: '1', '_s.b': '2', '_s._t.c': '3' })), ['a', '_s.b', '_s._t.c']);
});

test('hasString treats a missing key, an empty string and a section as missing', () => {
	const en = locale({ a: 'A', b: '', '_s.c': 'C' });
	assert.equal(hasString(en, 'a'), true);
	assert.equal(hasString(en, '_s.c'), true);
	assert.equal(hasString(en, 'b'), false);
	assert.equal(hasString(en, 'missing'), false);
	assert.equal(hasString(en, '_s'), false);
	assert.equal(hasString(en, '_s.c.d'), false);
});

test('a ja-JP string missing from en-US is a violation unless allowlisted', () => {
	const ja = locale({ a: 'あ', '_s.b': 'い', '_s.c': 'う' });
	const en = locale({ a: 'A', '_s.c': '' });
	assert.deepEqual(check(ja, en, new Map()).violations, ['_s.b', '_s.c']);
	assert.deepEqual(check(ja, en, new Map([['_s.b', 'r'], ['_s.c', 'r']])).violations, []);
});

test('an allowlist entry that en-US has, or that ja-JP no longer has, is unused', () => {
	const ja = locale({ a: 'あ', b: 'い' });
	const en = locale({ a: 'A' });
	const result = check(ja, en, new Map([['a', 'r'], ['b', 'r'], ['gone', 'r']]));
	assert.deepEqual(result.violations, []);
	assert.deepEqual(result.unused, ['a', 'gone']);
});

test('parseAllowlist reads key and reason, and reports malformed lines', () => {
	const { entries, malformed } = parseAllowlist('# comment\n\na\treason a\nb\n\tno key\nc\t \nd\tr\textra\na\tagain\n');
	assert.deepEqual([...entries], [['a', 'reason a']]);
	assert.equal(malformed.length, 5);
});

test('parseAllowlist refuses Elythia strings, which must be translated instead', () => {
	const { entries, malformed } = parseAllowlist(`_mkgoBubbleGame.mode\tr\n${ELYTHIA_MARKERS[1]}\tr\nup\tr\n`);
	assert.deepEqual([...entries.keys()], ['up']);
	assert.equal(malformed.length, 2);
	assert.match(malformed.join('\n'), /_mkgoBubbleGame\.mode is an Elythia string/);
});

test('the lower bound fails when too few strings are read or a marker is missing', () => {
	assert.deepEqual(trustProblems(check(locale(lowerBound()), {}, new Map())), []);
	const few = locale(Object.fromEntries(REQUIRED_KEYS.map(key => [key, key])));
	assert.match(trustProblems(check(few, {}, new Map())).join('\n'), /only \d+ strings were read/);
	const flat = lowerBound();
	delete flat[REQUIRED_KEYS[0]];
	assert.deepEqual(trustProblems(check(locale(flat), {}, new Map())), [`${REQUIRED_KEYS[0]} was not read from ja-JP`]);
});

test('buildAllowlist keeps only strings that upstream has in ja-JP but not in en-US', () => {
	const upstreamJa = locale({ up: 'う', upDone: 'え' });
	const upstreamEn = locale({ upDone: 'E' });
	const built = buildAllowlist(['up', 'upDone', 'elythia'], upstreamJa, upstreamEn, new Map([['up', 'kept reason']]));
	assert.match(built.text, /^up\tkept reason$/m);
	assert.doesNotMatch(built.text, /^upDone\t/m);
	assert.doesNotMatch(built.text, /^elythia\t/m);
	assert.equal(built.errors.length, 2);
	assert.match(built.errors.join('\n'), /elythia: not in the upstream ja-JP/);
	assert.match(built.errors.join('\n'), /upDone: the upstream en-US has it/);
});

test('run passes when every string is in en-US or allowlisted', () => {
	const ja = { ...lowerBound(), pending: 'ま' };
	const en = lowerBound();
	const result = runOn(ja, en, 'pending\treason\n');
	assert.equal(result.code, 0, result.errors.join('\n'));
});

test('run fails on a missing string and on an unused allowlist entry', () => {
	const ja = { ...lowerBound(), '_mkgoX.missing': 'ま' };
	const missing = runOn(ja, lowerBound(), '');
	assert.equal(missing.code, 1);
	assert.match(missing.errors.join('\n'), /_mkgoX\.missing is missing/);

	const unused = runOn(lowerBound(), lowerBound(), 'filler.k0\treason\n');
	assert.equal(unused.code, 1);
	assert.match(unused.errors.join('\n'), /filler\.k0 is listed but en-US has it/);
});

test('run cannot be trusted with a malformed allowlist or below the lower bound', () => {
	assert.equal(runOn(lowerBound(), lowerBound(), 'no reason\n').code, 2);
	const flat = lowerBound();
	delete flat[REQUIRED_KEYS.at(-1)];
	assert.equal(runOn(flat, flat, '').code, 2);
});

test('--write-allowlist writes the upstream untranslated strings and refuses Elythia strings', () => {
	const ja = { ...lowerBound(), up: 'う' };
	const upstream = { ja: { up: 'う' }, en: { other: 'O' } };
	const written = runOn(ja, lowerBound(), '', ['--write-allowlist'], upstream);
	assert.equal(written.code, 0, written.errors.join('\n'));
	assert.match(written.allowlist, new RegExp(`^up\\t${UPSTREAM_UNTRANSLATED_REASON.replace(/[()]/g, '\\$&')}$`, 'm'));
	assert.equal(runOn(ja, lowerBound(), written.allowlist).code, 0);

	// Elythia 自身の locales/ を渡すと、抜けた Elythia の語が許可リストへ逃げる
	// 見分けは `_mkgo` の節 1 つで足りる
	const own = runOn({ ...ja, elythia: 'え' }, lowerBound(), 'kept\tr\n', ['--write-allowlist'], { ja: { up: 'う', elythia: 'え', '_mkgoX.y': 'お' }, en: { other: 'O' } });
	assert.equal(own.code, 2);
	assert.match(own.errors.join('\n'), /has Elythia sections \(_mkgoX\)/);
	assert.equal(own.allowlist, 'kept\tr\n');

	const refused = runOn({ ...ja, elythia: 'え' }, lowerBound(), 'kept\tr\n', ['--write-allowlist'], upstream);
	assert.equal(refused.code, 1);
	assert.equal(refused.allowlist, 'kept\tr\n', 'the allowlist must not be rewritten when an Elythia string is missing');
});

test('run reads Elythia\'s words (locales-elythia/) laid over the upstream words (#4071)', () => {
	const upstreamJa = Object.fromEntries(Object.entries(lowerBound()).filter(([key]) => !ELYTHIA_MARKERS.includes(key)));
	const ownJa = { ...Object.fromEntries(ELYTHIA_MARKERS.map(key => [key, key])), '_mkgoX.y': 'お' };

	// 目印の語は Elythia のファイルにだけある。重ねて読まないと下限で落ちる
	const ok = runOn(upstreamJa, upstreamJa, '', [], undefined, { ja: ownJa, en: ownJa });
	assert.equal(ok.code, 0, ok.errors.join('\n'));

	// Elythia の ja-JP に足した語が、Elythia の en-US に無い
	const missing = runOn(upstreamJa, upstreamJa, '', [], undefined, { ja: ownJa, en: Object.fromEntries(ELYTHIA_MARKERS.map(key => [key, key])) });
	assert.equal(missing.code, 1);
	assert.match(missing.errors.join('\n'), /locales-elythia\/en-US\.yml: _mkgoX\.y is missing/);

	// Elythia の en-US が本家の英語の語を空文字で消すと、英語の UI に日本語が出る
	const removed = runOn(upstreamJa, upstreamJa, '', [], undefined, { ja: ownJa, en: { ...ownJa, 'filler.k0': '' } });
	assert.equal(removed.code, 1);
	assert.match(removed.errors.join('\n'), /filler\.k0 is missing/);
});

test('run cannot be trusted with unknown arguments or --write-allowlist without a directory', () => {
	const unknown = runOn(lowerBound(), lowerBound(), '', ['--bogus']);
	assert.equal(unknown.code, 2);
	assert.match(unknown.errors.join('\n'), /unknown arguments: --bogus/);
	const bare = runOn(lowerBound(), lowerBound(), '', ['--write-allowlist']);
	assert.equal(bare.code, 2);
	assert.match(bare.errors.join('\n'), /--write-allowlist takes the upstream locales directory/);
});
