/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
/**
 * Fails when frontend/locales/ differs from the upstream locales/ of the pinned version (#4071).
 *
 * Usage (from the repository root):
 *   node frontend/scripts/check-locale-upstream.mjs <upstream locales dir>
 *
 * Exit codes:
 *   0 = every file is byte for byte the same as upstream, and no file is missing on either side
 *   1 = a file differs, or exists on only one side
 *   2 = the check could not be trusted (no directory given, or too few files were compared)
 *
 * Elythia の語は locales-elythia/ に置き、本家のファイルは本家のまま持つ。本家のファイルに
 * 書き足すと、本家の取り込みのたびに衝突を手で解くことに戻る。本家のソースが要るので、
 * CI では本家を取得している apicompat (make upstream-check) で回す。
 */

import { execFileSync } from 'node:child_process';
import { existsSync, readdirSync, readFileSync, realpathSync, statSync } from 'node:fs';
import { basename, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const frontendRoot = fileURLToPath(new URL('..', import.meta.url));

/** 本家の locales/ は 42 の言語と README.md を持つ。抽出が空振りしたら落とす */
export const MIN_FILES = 40;
export const REQUIRED_FILES = ['ja-JP.yml', 'en-US.yml'];

/**
 * Compares two sets of files by name and content.
 *
 * @param {Map<string, Buffer>} ours
 * @param {Map<string, Buffer>} upstream
 * @returns {{ differ: string[], onlyOurs: string[], onlyUpstream: string[] }}
 */
export function compareFiles(ours, upstream) {
	return {
		differ: [...ours.keys()].filter(name => upstream.has(name) && !ours.get(name)?.equals(/** @type {Buffer} */ (upstream.get(name)))).sort(),
		onlyOurs: [...ours.keys()].filter(name => !upstream.has(name)).sort(),
		onlyUpstream: [...upstream.keys()].filter(name => !ours.has(name)).sort(),
	};
}

/**
 * Reads the locale files that git tracks (an untracked file would pass here and fail in CI).
 *
 * @param {string} root the frontend/ directory
 * @returns {Map<string, Buffer>}
 */
function readTracked(root) {
	const out = execFileSync('git', ['ls-files', '-z', '--', 'locales'], { cwd: root, encoding: 'utf8' });
	/** @type {Map<string, Buffer>} */
	const files = new Map();
	for (const path of out.split('\0').filter(f => f !== '')) {
		files.set(path.slice('locales/'.length), readFileSync(join(root, path)));
	}
	return files;
}

/**
 * @param {string} dir
 * @returns {Map<string, Buffer>}
 */
function readDir(dir) {
	/** @type {Map<string, Buffer>} */
	const files = new Map();
	for (const name of readdirSync(dir)) {
		const path = join(dir, name);
		if (statSync(path).isFile()) files.set(name, readFileSync(path));
	}
	return files;
}

/**
 * Runs the check and returns the exit code.
 *
 * @param {string} root the frontend/ directory
 * @param {string[]} args
 * @param {{ log: (line: string) => void, error: (line: string) => void }} out
 * @param {Map<string, Buffer>} [ours] the files of frontend/locales (defaults to the tracked ones)
 * @returns {number}
 */
export function run(root, args, out, ours = readTracked(root)) {
	if (args.length !== 1 || !existsSync(args[0])) {
		out.error('check-locale-upstream: pass the upstream locales directory (.cache/misskey/<version>/locales)');
		return 2;
	}
	const upstream = readDir(args[0]);
	/** @type {string[]} */
	const problems = [];
	for (const [label, files] of /** @type {const} */ ([['frontend/locales', ours], [args[0], upstream]])) {
		if (files.size < MIN_FILES) problems.push(`only ${files.size} files were read from ${label} (expected at least ${MIN_FILES})`);
		for (const name of REQUIRED_FILES) {
			if (!files.has(name)) problems.push(`${name} was not read from ${label}`);
		}
	}
	for (const problem of problems) out.error(`check-locale-upstream: ${problem}`);
	if (problems.length > 0) return 2;

	const result = compareFiles(ours, upstream);
	for (const name of result.differ) {
		out.error(`frontend/locales/${name} differs from upstream. Put Elythia's words in frontend/locales-elythia/${name} and keep this file as upstream has it`);
	}
	for (const name of result.onlyOurs) {
		out.error(`frontend/locales/${name} is not in upstream. Put Elythia's words in frontend/locales-elythia/`);
	}
	for (const name of result.onlyUpstream) {
		out.error(`frontend/locales/${name} is missing. Copy it from upstream (${basename(args[0])}/${name})`);
	}
	if (result.differ.length + result.onlyOurs.length + result.onlyUpstream.length > 0) return 1;
	out.log(`check-locale-upstream: ${ours.size} files, all the same as upstream`);
	return 0;
}

// symlink を通して起動しても main を走らせる (import.meta.url は実体のパスになる)
if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
	let code;
	try {
		code = run(frontendRoot, process.argv.slice(2), { log: console.log, error: console.error });
	} catch (error) {
		console.error(`check-locale-upstream: ${error instanceof Error ? error.message : String(error)}`);
		code = 2;
	}
	process.exit(code);
}
