/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
/**
 * Fails when a string with Japanese text is written directly in the frontend code, instead of
 * going through i18n (#4070).
 *
 * Usage (from frontend/):
 *   node scripts/check-hardcoded-japanese.mjs
 *
 * Exit codes:
 *   0 = no Japanese string outside the allowlist
 *   1 = a Japanese string is written directly, or the allowlist has entries that are not used
 *   2 = the check could not be trusted (a parser, a malformed allowlist, or the lower bound failed)
 *
 * 英語などの UI でも日本語のまま出るので、画面の文言は i18n (locales-elythia) を通す。
 * Elythia のファイルを SPDX の見出しで見分けると、見出しが本家のままの Elythia のファイルを
 * 取りこぼすので、frontend/packages の全体を見て、本家にもともとある直書きを許可リストで除く。
 * 見るのは template の文字・属性・式と、script の文字列。コメント、検索の語 (SearchMarker の
 * keywords)、テストは見ない。
 */

import { execFileSync } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const frontendRoot = fileURLToPath(new URL('..', import.meta.url));
// typescript と vue は frontend パッケージの依存として入っている (ルートの typescript は版が違う)
const require = createRequire(join(frontendRoot, 'packages', 'frontend', 'package.json'));
/** @type {typeof import('typescript')} */
const ts = require('typescript');
/** @type {typeof import('vue/compiler-sfc')} */
const sfc = require('vue/compiler-sfc');

// かな (拡張と補助を含む)・漢字 (拡張と互換を含む)・和文の約物・囲み文字・全角の英数と記号。
// 半角の矢印など (U+FFE8-FFEE) は日本語ではないので外す
const JAPANESE = /[\u3000-\u30ff\u31f0-\u33ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff\uff00-\uffe6\u{1b000}-\u{1b16f}\u{20000}-\u{3134f}]/u;

/**
 * Reports whether the text has Japanese. 全角の空白だけの文字列 (表の見出しの埋め草など) は文言ではない。
 *
 * @param {string} text
 * @returns {boolean}
 */
function hasJapanese(text) {
	return JAPANESE.test(text.replace(/\s/gu, ''));
}

/**
 * Lower bounds, so that a change in the file layout or the parser does not turn the check into
 * one that reads nothing and passes. 名指しのファイルは、Elythia が足した画面で直書きを
 * 直した実物 (template と script の両方に文言がある)。
 */
export const MIN_FILES = 900;
export const REQUIRED_FILES = [
	'packages/frontend/src/pages/signup-application.vue',
	'packages/frontend/src/pages/admin/moderation.vue',
];

/**
 * @typedef {{ line: number, text: string }} Finding
 */

/**
 * Collapses the whitespace, so that a string can be matched against the allowlist.
 *
 * @param {string} text
 * @returns {string}
 */
export function normalize(text) {
	return text.replace(/\s+/g, ' ').trim();
}

/**
 * Lists the Japanese strings in JavaScript or TypeScript code (string literals, template
 * literals, JSX text). Comments are not part of the syntax tree, so they are not read.
 *
 * @param {string} source
 * @param {number} [lineOffset] the line of the first line of `source` in its file, minus one
 * @param {'ts' | 'tsx' | 'js' | 'jsx'} [kind] TSX は .tsx と lang="tsx" のときだけ。.ts を TSX として
 *   読むと、`<T>(x) =>` や `<string>x` の後ろがタグの中身と読まれ、日本語のコメントを拾う
 * @returns {Finding[]}
 */
export function japaneseInScript(source, lineOffset = 0, kind = 'ts') {
	const scriptKind = { ts: ts.ScriptKind.TS, tsx: ts.ScriptKind.TSX, js: ts.ScriptKind.JS, jsx: ts.ScriptKind.JSX }[kind];
	const file = ts.createSourceFile(`x.${kind}`, source, ts.ScriptTarget.Latest, true, scriptKind);
	/** @type {Finding[]} */
	const found = [];
	/** @param {import('typescript').Node} node */
	const visit = (node) => {
		if (
			ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node) ||
			ts.isTemplateHead(node) || ts.isTemplateMiddle(node) || ts.isTemplateTail(node) || ts.isJsxText(node)
		) {
			const text = node.text;
			if (hasJapanese(text)) {
				found.push({ line: file.getLineAndCharacterOfPosition(node.getStart(file)).line + 1 + lineOffset, text: normalize(text) });
			}
		}
		ts.forEachChild(node, visit);
	};
	visit(file);
	return found;
}

/**
 * Lists the Japanese strings in a Vue single-file component: text, static attributes and
 * expressions in the template, and strings in the scripts. Comments are skipped, and so are
 * the search words of SearchMarker (`:keywords`).
 *
 * @param {string} source
 * @returns {Finding[]}
 */
export function japaneseInVue(source) {
	const { descriptor, errors } = sfc.parse(source, { ignoreEmpty: false });
	if (errors.length > 0) throw new Error(`cannot parse: ${errors[0] instanceof Error ? errors[0].message : String(errors[0])}`);
	/** @type {Finding[]} */
	const found = [];
	/**
	 * @param {string} expression
	 * @param {number} line
	 */
	const fromExpression = (expression, line) => {
		for (const f of japaneseInScript(expression, line - 1)) found.push(f);
	};
	/** @param {any} node */
	const visit = (node) => {
		switch (node.type) {
			case 2: // TEXT
				if (hasJapanese(node.content)) {
					// 部品の前の改行も文字の部品に入るので、文字が始まる行を出す (content は空白を詰めてあるので、元の文字で数える)
					const leading = /^\s*/.exec(node.loc.source)?.[0] ?? '';
					found.push({ line: node.loc.start.line + (leading.match(/\n/g)?.length ?? 0), text: normalize(node.content) });
				}
				break;
			case 5: // INTERPOLATION
				fromExpression(node.content.content, node.content.loc.start.line);
				break;
			case 1: // ELEMENT
				for (const prop of node.props) {
					if (prop.type === 6 && prop.value != null && hasJapanese(prop.value.content)) {
						found.push({ line: prop.loc.start.line, text: normalize(prop.value.content) });
					} else if (prop.type === 7 && prop.exp != null) {
						// SearchMarker の検索の語は画面に出ない (設定の検索の索引に入るだけ)
						if (node.tag === 'SearchMarker' && prop.arg?.content === 'keywords') continue;
						fromExpression(prop.exp.content, prop.exp.loc.start.line);
					}
				}
				break;
		}
		for (const child of node.children ?? []) visit(child);
	};
	if (descriptor.template?.ast != null) visit(descriptor.template.ast);
	for (const script of [descriptor.script, descriptor.scriptSetup]) {
		if (script == null) continue;
		const lang = script.lang ?? 'js';
		const kind = lang === 'tsx' || lang === 'jsx' || lang === 'js' ? lang : 'ts';
		for (const f of japaneseInScript(script.content, script.loc.start.line - 1, kind)) found.push(f);
	}
	return found;
}

/**
 * Parses the allowlist: one `<file>\t<text or *>\t<reason>` per line, `#` comments and blank
 * lines ignored. `*` allows every string in the file.
 *
 * @param {string} text
 * @returns {{ entries: Map<string, string>, malformed: string[] }} keyed by `<file>\t<text>`
 */
export function parseAllowlist(text) {
	/** @type {Map<string, string>} */
	const entries = new Map();
	/** @type {string[]} */
	const malformed = [];
	text.split('\n').forEach((line, i) => {
		if (line.trim() === '' || line.startsWith('#')) return;
		const [file, str, reason, ...rest] = line.split('\t');
		if (!file || !str || reason == null || reason.trim() === '' || rest.length > 0) {
			malformed.push(`line ${i + 1}: expected '<file>\\t<text or *>\\t<reason>'`);
		} else if (entries.has(`${file}\t${str}`)) {
			malformed.push(`line ${i + 1}: ${file} ${str} is listed twice`);
		} else {
			entries.set(`${file}\t${str}`, reason);
		}
	});
	return { entries, malformed };
}

/**
 * Lists the files to read: the tracked code under packages/, without tests.
 *
 * @param {string} root the frontend/ directory
 * @returns {string[]}
 */
export function trackedFiles(root) {
	const out = execFileSync('git', ['ls-files', '-z', '--', 'packages'], { cwd: root, encoding: 'utf8' });
	return out.split('\0').filter((f) => /\.(vue|ts|tsx|js|mjs|cjs)$/.test(f) && !isTest(f)).sort();
}

/**
 * @param {string} file
 * @returns {'ts' | 'tsx' | 'js' | 'jsx'}
 */
export function scriptKindOf(file) {
	if (file.endsWith('.tsx')) return 'tsx';
	if (file.endsWith('.ts')) return 'ts';
	return 'js';
}

/**
 * @param {string} file
 * @returns {boolean}
 */
function isTest(file) {
	return /\/(test|test-d)\//.test(file) || /\.(test|spec)\.[cm]?[jt]sx?$/.test(file) || file.endsWith('.d.ts');
}

/**
 * Runs the check and returns the exit code.
 *
 * @param {string} root the frontend/ directory
 * @param {{ log: (line: string) => void, error: (line: string) => void }} out
 * @param {string[]} [files] the files to read, relative to root (defaults to the tracked ones)
 * @param {string} [allowlistPath]
 * @param {{ minFiles: number, requiredFiles: string[] }} [bounds] the lower bounds (tests pass smaller ones)
 * @returns {number}
 */
export function run(
	root, out, files = trackedFiles(root), allowlistPath = join(root, 'scripts', 'hardcoded-japanese-allowlist.tsv'),
	bounds = { minFiles: MIN_FILES, requiredFiles: REQUIRED_FILES },
) {
	const { entries, malformed } = parseAllowlist(readFileSync(allowlistPath, 'utf8'));
	for (const problem of malformed) out.error(`check-hardcoded-japanese: ${allowlistPath}: ${problem}`);
	if (malformed.length > 0) return 2;

	/** @type {string[]} */
	const problems = [];
	if (files.length < bounds.minFiles) problems.push(`only ${files.length} files were read (expected at least ${bounds.minFiles})`);
	for (const file of bounds.requiredFiles) {
		if (!files.includes(file)) problems.push(`${file} was not read`);
	}
	for (const problem of problems) out.error(`check-hardcoded-japanese: ${problem}`);
	if (problems.length > 0) return 2;

	/** @type {Set<string>} */
	const used = new Set();
	/** @type {string[]} */
	const violations = [];
	for (const file of files) {
		const source = readFileSync(join(root, file), 'utf8');
		/** @type {Finding[]} */
		let found;
		try {
			found = file.endsWith('.vue') ? japaneseInVue(source) : japaneseInScript(source, 0, scriptKindOf(file));
		} catch (error) {
			out.error(`check-hardcoded-japanese: ${file}: ${error instanceof Error ? error.message : String(error)}`);
			return 2;
		}
		for (const f of found) {
			const key = [`${file}\t${f.text}`, `${file}\t*`].find((k) => entries.has(k));
			if (key != null) {
				used.add(key);
			} else {
				violations.push(`${file}:${f.line}: "${f.text}" is written directly. Put it in frontend/locales-elythia and use i18n (developer-facing messages can be in English)`);
			}
		}
	}
	for (const v of violations) out.error(v);
	/** @type {string[]} */
	const unused = [...entries.keys()].filter((k) => !used.has(k));
	for (const k of unused) {
		// 直したか、本家が消した。残すと、同じ文を書き直したときに素通りする
		out.error(`${allowlistPath}: ${k.replace('\t', ' ')} is listed but not found. Remove the line`);
	}
	if (violations.length > 0 || unused.length > 0) return 1;
	out.log(`check-hardcoded-japanese: ${files.length} files, no Japanese string outside the ${entries.size} allowlisted`);
	return 0;
}

// symlink を通して起動しても main を走らせる (import.meta.url は実体のパスになる)
if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
	let code;
	try {
		code = run(frontendRoot, { log: console.log, error: console.error });
	} catch (error) {
		console.error(`check-hardcoded-japanese: ${error instanceof Error ? error.message : String(error)}`);
		code = 2;
	}
	process.exit(code);
}
