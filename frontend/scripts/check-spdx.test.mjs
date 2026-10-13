/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const scriptsDir = fileURLToPath(new URL('.', import.meta.url));

/**
 * Creates a throwaway git repository with frontend/scripts/check-spdx.mjs and
 * one committed file that already has the upstream header.
 *
 * @returns {string} the frontend/ directory of the repository
 */
function fakeRepo() {
	const root = mkdtempSync(join(tmpdir(), 'check-spdx-'));
	const frontend = join(root, 'frontend');
	mkdirSync(join(frontend, 'scripts', 'lib'), { recursive: true });
	copyFileSync(join(scriptsDir, 'check-spdx.mjs'), join(frontend, 'scripts', 'check-spdx.mjs'));
	copyFileSync(join(scriptsDir, 'lib', 'git.mjs'), join(frontend, 'scripts', 'lib', 'git.mjs'));
	const src = join(frontend, 'packages', 'frontend', 'src');
	mkdirSync(src, { recursive: true });
	writeFileSync(join(src, 'upstream.ts'), '/*\n * SPDX-FileCopyrightText: syuilo and misskey-project\n * SPDX-License-Identifier: AGPL-3.0-only\n */\n\nexport {};\n');
	const git = (/** @type {string[]} */ ...args) => execFileSync('git', ['-c', 'user.name=t', '-c', 'user.email=t@example.com', '-c', 'commit.gpgSign=false', ...args], { cwd: root, stdio: 'pipe' });
	git('init', '-q');
	git('add', '-A');
	git('commit', '-q', '-m', 'init');
	return frontend;
}

test('--fix gives new files the Elythia-Network header, not the upstream one', (t) => {
	const frontend = fakeRepo();
	t.after(() => rmSync(join(frontend, '..'), { recursive: true, force: true }));
	const src = join(frontend, 'packages', 'frontend', 'src');
	writeFileSync(join(src, 'new.ts'), 'export const a = 1;\n');
	writeFileSync(join(src, 'new.vue'), '<template><div></div></template>\n');

	const run = spawnSync(process.execPath, ['scripts/check-spdx.mjs', '--fix', '--base', 'HEAD'], { cwd: frontend, encoding: 'utf8' });
	assert.equal(run.status, 0, run.stdout + run.stderr);

	assert.equal(
		readFileSync(join(src, 'new.ts'), 'utf8'),
		'/*\n * SPDX-FileCopyrightText: Elythia-Network\n * SPDX-License-Identifier: AGPL-3.0-only\n */\n\nexport const a = 1;\n',
	);
	assert.equal(
		readFileSync(join(src, 'new.vue'), 'utf8'),
		'<!--\nSPDX-FileCopyrightText: Elythia-Network\nSPDX-License-Identifier: AGPL-3.0-only\n-->\n\n<template><div></div></template>\n',
	);
	// 上流のヘッダーを持つ既存のファイルには触らない
	assert.match(readFileSync(join(src, 'upstream.ts'), 'utf8'), /SPDX-FileCopyrightText: syuilo and misskey-project/);
});

test('the check passes either the license line or the upstream copyright line, but not the Elythia line alone', (t) => {
	const frontend = fakeRepo();
	t.after(() => rmSync(join(frontend, '..'), { recursive: true, force: true }));
	const src = join(frontend, 'packages', 'frontend', 'src');
	writeFileSync(join(src, 'licenseonly.ts'), '/*\n * SPDX-License-Identifier: AGPL-3.0-only\n */\n');
	writeFileSync(join(src, 'upstreamonly.ts'), '/*\n * SPDX-FileCopyrightText: syuilo and misskey-project\n */\n');
	writeFileSync(join(src, 'elythiaonly.ts'), '/*\n * SPDX-FileCopyrightText: Elythia-Network\n */\n');

	const run = spawnSync(process.execPath, ['scripts/check-spdx.mjs', '--check'], { cwd: frontend, encoding: 'utf8' });
	assert.equal(run.status, 1, run.stdout + run.stderr);
	// 判定は #3533 の前と同じ: ライセンスの行か上流の著作権の行のどちらかがあれば通す
	assert.match(run.stdout, /missing: {4}packages\/frontend\/src\/elythiaonly\.ts/);
	assert.doesNotMatch(run.stdout, /licenseonly\.ts/);
	assert.doesNotMatch(run.stdout, /upstreamonly\.ts/);
	assert.match(run.stdout, /CI 判定上の欠落 1 件/);
});
