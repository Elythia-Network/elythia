/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { compareFiles, MIN_FILES, REQUIRED_FILES, run } from './check-locale-upstream.mjs';

/** @returns {Map<string, Buffer>} enough files to satisfy the lower bound */
function lowerBound() {
	/** @type {Map<string, Buffer>} */
	const files = new Map(REQUIRED_FILES.map(name => [name, Buffer.from(`${name}\n`)]));
	for (let i = files.size; i < MIN_FILES; i++) files.set(`l${i}.yml`, Buffer.from(`${i}\n`));
	return files;
}

/**
 * Runs the script against a temporary upstream directory.
 *
 * @param {Map<string, Buffer>} ours
 * @param {Map<string, Buffer> | null} upstream null to pass no directory
 */
function runOn(ours, upstream) {
	const dir = mkdtempSync(join(tmpdir(), 'locale-upstream-'));
	try {
		for (const [name, content] of upstream ?? []) writeFileSync(join(dir, name), content);
		// 本家の locales/ にディレクトリがあっても数えない
		mkdirSync(join(dir, 'subdir'));
		/** @type {string[]} */
		const errors = [];
		/** @type {string[]} */
		const logs = [];
		const code = run(dir, upstream == null ? [] : [dir], { log: line => logs.push(line), error: line => errors.push(line) }, ours);
		return { code, errors, logs };
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
}

test('compareFiles finds changed files and files on only one side', () => {
	const result = compareFiles(
		new Map([['same', Buffer.from('a')], ['changed', Buffer.from('ours')], ['ours', Buffer.from('x')]]),
		new Map([['same', Buffer.from('a')], ['changed', Buffer.from('theirs')], ['theirs', Buffer.from('y')]]),
	);
	assert.deepEqual(result, { differ: ['changed'], onlyOurs: ['ours'], onlyUpstream: ['theirs'] });
});

test('passes when every file is the same as upstream', () => {
	const result = runOn(lowerBound(), lowerBound());
	assert.equal(result.code, 0, result.errors.join('\n'));
	assert.match(result.logs.join('\n'), new RegExp(`${MIN_FILES} files, all the same as upstream`));
});

test('fails when a word is added to an upstream file', () => {
	const ours = lowerBound();
	ours.set('ja-JP.yml', Buffer.from('ja-JP.yml\n_mkgoX: "Elythia"\n'));
	const result = runOn(ours, lowerBound());
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /frontend\/locales\/ja-JP\.yml differs from upstream/);
});

test('fails when a file exists on only one side', () => {
	const ours = lowerBound();
	ours.set('xx-XX.yml', Buffer.from('a: "A"\n'));
	const upstream = lowerBound();
	upstream.set('yy-YY.yml', Buffer.from('a: "A"\n'));
	const result = runOn(ours, upstream);
	assert.equal(result.code, 1);
	assert.match(result.errors.join('\n'), /xx-XX\.yml is not in upstream/);
	assert.match(result.errors.join('\n'), /yy-YY\.yml is missing/);
});

test('cannot be trusted without a directory, with too few files, or without ja-JP', () => {
	assert.equal(runOn(lowerBound(), null).code, 2);
	/** @type {string[]} */
	const errors = [];
	assert.equal(run('.', [join(tmpdir(), 'no-such-locales-dir')], { log: () => {}, error: line => errors.push(line) }, lowerBound()), 2);
	assert.match(errors.join('\n'), /pass the upstream locales directory/);

	// 本家の側が空振りしても落とす (抽出が壊れて本家を読めていない)
	const fewUpstream = runOn(lowerBound(), new Map([...lowerBound()].slice(0, MIN_FILES - 1)));
	assert.equal(fewUpstream.code, 2);
	assert.match(fewUpstream.errors.join('\n'), /only 39 files were read from \//);

	const few = new Map([...lowerBound()].slice(0, MIN_FILES - 1));
	const tooFew = runOn(few, few);
	assert.equal(tooFew.code, 2);
	assert.match(tooFew.errors.join('\n'), /only 39 files were read from frontend\/locales/);

	const noJa = lowerBound();
	noJa.delete('ja-JP.yml');
	noJa.set('extra.yml', Buffer.from('e\n'));
	const missing = runOn(noJa, noJa);
	assert.equal(missing.code, 2);
	assert.match(missing.errors.join('\n'), /ja-JP\.yml was not read from frontend\/locales/);
});
