/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
/**
 * Fails when Elythia's words (locales-elythia/) rewrite or remove an upstream word that is not
 * listed in locales-elythia/overrides.tsv (#4071).
 *
 * Usage (from frontend/):
 *   node scripts/check-locale-overrides.mjs
 *
 * Exit codes:
 *   0 = every rewritten or removed upstream word is listed
 *   1 = an unlisted upstream word is rewritten or removed, a word is the same as upstream, an
 *       empty string removes nothing, a value is not a string, the build does not read the
 *       language, or a listed key is unused
 *   2 = the check could not be trusted (a parser, a malformed list, or the lower bound failed)
 *
 * Elythia の語は本家の語に重なる (packages/i18n/src/elythia.ts) ので、本家と同じキーを書くと
 * 気付かないうちに本家の文言を上書きする。書き換えてよいキーは一覧に理由付きで書かせる。
 * 一覧はキーの単位で全言語に効く (言語ごとに書くと 400 行を超え、読まれなくなる)。
 */

import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { basename, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { languages } from '../packages/i18n/src/const.ts';
import { ELYTHIA_LOCALES_DIR } from '../packages/i18n/src/elythia.ts';

const frontendRoot = fileURLToPath(new URL('..', import.meta.url));
// js-yaml はロケールを読む i18n パッケージの依存として入っている (ルートには無い)
const require = createRequire(join(frontendRoot, 'packages', 'i18n', 'package.json'));
/** @type {typeof import('js-yaml')} */
const yaml = require('js-yaml');

/**
 * Lower bounds, so that a change in the file layout or the parser does not turn the check into
 * one that reads nothing and passes. ja-JP と en-US は Elythia の語の大半を持つので必ずある。
 * introMisskey はほぼ全言語で書き換えている本家の語。
 */
export const REQUIRED_FILES = ['ja-JP.yml', 'en-US.yml'];
export const REQUIRED_OVERRIDE = 'introMisskey';

/**
 * @typedef {{ [key: string]: string | LocaleRecord }} LocaleRecord
 */

/**
 * Flattens the locale into dotted paths of its values.
 *
 * 文字列以外の値 (null、数、配列、空の節) もそのまま 1 つの値として残す。空の節を
 * 読み飛ばすと、本家の語を `{}` で置き換えても比べる対象に入らず、素通りする。
 *
 * @param {unknown} locale
 * @param {string} [prefix]
 * @returns {Map<string, unknown>}
 */
export function flatten(locale, prefix = '') {
	/** @type {Map<string, unknown>} */
	const out = new Map();
	if (locale === null || typeof locale !== 'object') return out;
	for (const [key, value] of Object.entries(locale)) {
		const path = prefix === '' ? key : `${prefix}.${key}`;
		if (value !== null && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length > 0) {
			for (const [k, v] of flatten(value, path)) out.set(k, v);
		} else {
			out.set(path, value);
		}
	}
	return out;
}

/**
 * Parses the list: one `<dotted key>\t<reason>` per line, `#` comments and blank lines ignored.
 *
 * @param {string} text
 * @returns {{ entries: Map<string, string>, malformed: string[] }}
 */
export function parseOverrides(text) {
	/** @type {Map<string, string>} */
	const entries = new Map();
	/** @type {string[]} */
	const malformed = [];
	text.split('\n').forEach((line, i) => {
		if (line.trim() === '' || line.startsWith('#')) return;
		const [key, reason, ...rest] = line.split('\t');
		if (key === '' || reason == null || reason.trim() === '' || rest.length > 0) {
			malformed.push(`line ${i + 1}: expected '<key>\\t<reason>'`);
		} else if (entries.has(key)) {
			malformed.push(`line ${i + 1}: ${key} is listed twice`);
		} else {
			entries.set(key, reason);
		}
	});
	return { entries, malformed };
}

/**
 * Finds where Elythia's words for one language touch the upstream words.
 *
 * 節と語の取り違え (本家の節を Elythia が文字列で置き換える、またはその逆) も、本家の
 * 語を消すので書き換えとして数える。
 *
 * @param {Map<string, unknown>} upstream
 * @param {Map<string, unknown>} own
 * @returns {{ overridden: string[], same: string[], removesNothing: string[] }}
 */
export function compareLanguage(upstream, own) {
	/** @type {string[]} */
	const overridden = [];
	/** @type {string[]} */
	const same = [];
	/** @type {string[]} */
	const removesNothing = [];
	const upstreamKeys = [...upstream.keys()];
	for (const [key, value] of own) {
		if (upstream.has(key)) {
			if (upstream.get(key) === value) same.push(key);
			else overridden.push(key);
			continue;
		}
		const clashes = upstreamKeys.some(k => k.startsWith(`${key}.`) || key.startsWith(`${k}.`));
		if (clashes) {
			overridden.push(key);
		} else if (value === '') {
			removesNothing.push(key);
		}
	}
	return { overridden, same, removesNothing };
}

/**
 * @param {string} path
 * @returns {unknown}
 */
function loadYaml(path) {
	return yaml.load(readFileSync(path, 'utf8'));
}

/**
 * Lists Elythia's locale files that git tracks (a file not yet added would pass here and fail in CI).
 *
 * @param {string} root the frontend/ directory
 * @returns {string[]}
 */
function trackedOwnFiles(root) {
	const out = execFileSync('git', ['ls-files', '-z', '--', `${ELYTHIA_LOCALES_DIR}/*.yml`], { cwd: root, encoding: 'utf8' });
	return out.split('\0').filter(f => f !== '').map(f => basename(f)).sort();
}

/**
 * Runs the check and returns the exit code.
 *
 * @param {string} root the frontend/ directory
 * @param {{ log: (line: string) => void, error: (line: string) => void }} out
 * @param {string[]} [files] Elythia's locale files to read (defaults to the tracked ones)
 * @param {readonly string[]} [builtLanguages] the languages that build() reads
 * @returns {number}
 */
export function run(root, out, files = trackedOwnFiles(root), builtLanguages = languages) {
	const listPath = join(root, ELYTHIA_LOCALES_DIR, 'overrides.tsv');
	const { entries, malformed } = parseOverrides(readFileSync(listPath, 'utf8'));
	for (const problem of malformed) out.error(`check-locale-overrides: ${listPath}: ${problem}`);
	if (malformed.length > 0) return 2;

	/** @type {string[]} */
	const problems = [];
	for (const file of REQUIRED_FILES) {
		if (!files.includes(file)) problems.push(`${ELYTHIA_LOCALES_DIR}/${file} was not read`);
	}

	/** @type {string[]} */
	const errors = [];
	/** @type {Set<string>} */
	const used = new Set();
	for (const file of files) {
		const upstreamPath = join(root, 'locales', file);
		// build() が読むのは const.ts の languages (本家のもの) だけ。本家にファイルがあっても、
		// languages に無い言語の語は画面に出ない
		if (!builtLanguages.includes(file.replace(/\.yml$/, '')) || !existsSync(upstreamPath)) {
			errors.push(`${ELYTHIA_LOCALES_DIR}/${file}: the build does not read this language (packages/i18n/src/const.ts), so these words are never used`);
			continue;
		}
		const own = flatten(loadYaml(join(root, ELYTHIA_LOCALES_DIR, file)));
		for (const [key, value] of own) {
			// null (`~`) はビルドが落ち、空の節や配列は本家の語を壊す
			if (typeof value !== 'string') errors.push(`${ELYTHIA_LOCALES_DIR}/${file}: ${key} is not a string. Write a string, or "" to remove the upstream word`);
		}
		const result = compareLanguage(flatten(loadYaml(upstreamPath)), own);
		for (const key of result.overridden) {
			used.add(key);
			if (!entries.has(key)) {
				errors.push(`${ELYTHIA_LOCALES_DIR}/${file}: ${key} rewrites or removes an upstream word. Use a new key, or list it with the reason in ${ELYTHIA_LOCALES_DIR}/overrides.tsv`);
			}
		}
		for (const key of result.same) {
			errors.push(`${ELYTHIA_LOCALES_DIR}/${file}: ${key} is the same as the upstream word. Remove the line`);
		}
		for (const key of result.removesNothing) {
			errors.push(`${ELYTHIA_LOCALES_DIR}/${file}: ${key} is an empty string, but upstream has no such word to remove. Remove the line`);
		}
	}
	if (!used.has(REQUIRED_OVERRIDE)) problems.push(`${REQUIRED_OVERRIDE} was not found rewritten in any language`);
	for (const problem of problems) out.error(`check-locale-overrides: ${problem}`);
	if (problems.length > 0) return 2;

	for (const key of entries.keys()) {
		// 本家に追従して本家の文が同じになった、または書き換えをやめた。残すと次の書き換えが素通りする
		if (!used.has(key)) errors.push(`${listPath}: ${key} is listed but no language rewrites or removes it. Remove the line`);
	}
	for (const error of errors) out.error(error);
	if (errors.length > 0) return 1;
	out.log(`check-locale-overrides: ${files.length} files, ${used.size} upstream keys rewritten or removed, all listed`);
	return 0;
}

// symlink を通して起動しても main を走らせる (import.meta.url は実体のパスになる)
if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
	let code;
	try {
		code = run(frontendRoot, { log: console.log, error: console.error });
	} catch (error) {
		console.error(`check-locale-overrides: ${error instanceof Error ? error.message : String(error)}`);
		code = 2;
	}
	process.exit(code);
}
