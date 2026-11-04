/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import * as fs from 'fs/promises';
import * as path from 'node:path';
import MagicString from 'magic-string';
import { collectModifications } from './locale-inliner/collect-modifications.js';
import { applyWithLocale } from './locale-inliner/apply-with-locale.js';
import { blankLogger } from './logger.js';
import { detectI18nFacadeChunk } from './locale-inliner/facade-chunk-detection.js';
import { collectStaticImports, staticImportClosure } from './locale-inliner/static-imports.js';
import type { Logger } from './logger.js';
import type { Locale } from 'i18n';
import type { Manifest as ViteManifest } from 'vite';

export class LocaleInliner {
	outputDir: string;
	scriptsDir: string;
	i18nFile: string;
	i18nFileName: string;
	i18nSymbol: string;
	logger: Logger;
	chunks: ScriptChunk[];
	manifest: ViteManifest;

	static async create(options: {
		outputDir: string,
		scriptsDir: string,
		i18nFile: string,
		logger: Logger,
	}): Promise<LocaleInliner> {
		const manifest: ViteManifest = JSON.parse(await fs.readFile(`${options.outputDir}/manifest.json`, 'utf-8'));
		return new LocaleInliner({ ...options, manifest });
	}

	constructor(options: {
		outputDir: string,
		scriptsDir: string,
		i18nFile: string,
		manifest: ViteManifest,
		logger: Logger,
	}) {
		this.outputDir = options.outputDir;
		this.scriptsDir = options.scriptsDir;
		this.i18nFile = options.i18nFile;
		this.i18nFileName = this.stripScriptDir(options.manifest[this.i18nFile].file);
		this.logger = options.logger;
		this.i18nSymbol = 'i18n';
		this.manifest = options.manifest;
		this.chunks = Object.values(options.manifest).filter(chunk => this.isScriptFile(chunk.file)).map(chunk => ({
			fileName: this.stripScriptDir(chunk.file),
			src: chunk.src,
			chunkName: chunk.name,
			isEntry: chunk.isEntry === true,
		}));
	}

	async loadFiles() {
		await Promise.all(this.chunks.map(async chunk => {
			const filePath = path.join(this.outputDir, this.scriptsDir, chunk.fileName);
			chunk.sourceCode = await fs.readFile(filePath, 'utf-8');
		}));
	}

	collectsModifications() {
		this.#detectI18nFacadeChunk();

		for (const chunk of this.chunks) {
			if (chunk.sourceCode == null) {
				throw new Error(`Source code for ${chunk.fileName} is not loaded.`);
			}
			if (chunk.isFacadeOfI18n) {
				chunk.modifications = [];
				continue;
			}
			const fileLogger = this.logger.prefixed(`${chunk.fileName} (${chunk.chunkName}): `);
			chunk.modifications = collectModifications(chunk.sourceCode, chunk.fileName, fileLogger, this);
		}

		if (!this.chunks.flatMap(x => x.modifications ?? []).some(x => x.type === 'localized')) {
			throw new Error('No localizations are inlined! this should mean locale inliner is not working well!');
		}
	}

	#detectI18nFacadeChunk() {
		// For some reason, even with `preserveEntrySignatures: 'allow-extension'`, rolldown may generate facade chunk
		// This method detects facade chunk and replace i18nFile / i18nFileName with correct file name
		const chunk = this.chunks.find(x => x.fileName === this.i18nFileName);
		if (chunk == null) throw new Error(`i18n script file '${this.i18nFile}' not found`);
		if (chunk.sourceCode == null) throw new Error(`Source code for '${this.i18nFile}' not loaded`);
		const fileLogger = this.logger.prefixed(`${chunk.fileName} (${chunk.chunkName}): `);
		const facadeInfo = detectI18nFacadeChunk(chunk.sourceCode, chunk.fileName, fileLogger);
		if (facadeInfo != null) {
			const i18nSymbol = facadeInfo.nameMap[this.i18nSymbol];
			if (i18nSymbol == null) throw new Error(`Facade module for i18n file does not map ${this.i18nSymbol}. mapping: ${JSON.stringify(facadeInfo.nameMap)}`);
			this.logger.info(`We detected ${this.i18nFileName} is facade chunk maps ${facadeInfo.fileName} with ${i18nSymbol} as ${this.i18nSymbol}`);
			chunk.isFacadeOfI18n = true;
			this.i18nFileName = facadeInfo.fileName;
			this.i18nSymbol = i18nSymbol;
		}
	}

	async saveAllLocales(locales: Record<string, Locale>) {
		const localeNames = Object.keys(locales);
		for (const localeName of localeNames) {
			this.logger.info(`Creating bundle for ${localeName}`);
			await this.saveLocale(localeName, locales[localeName]);
		}
		this.logger.info('Done');
	}

	async saveLocale(localeName: string, localeJson: Locale) {
		// create directory
		await fs.mkdir(path.join(this.outputDir, localeName), { recursive: true });
		const localeLogger = localeName === 'ja-JP' ? this.logger : blankLogger; // we want to log for single locale only
		for (const chunk of this.chunks) {
			if (chunk.sourceCode == null || !chunk.modifications) {
				throw new Error(`Source code or modifications for ${chunk.fileName} is not available.`);
			}
			const fileLogger = localeLogger.prefixed(`${chunk.fileName} (${chunk.chunkName}): `);
			const magicString = new MagicString(chunk.sourceCode);
			applyWithLocale(magicString, chunk.modifications, localeName, localeJson, fileLogger);

			await fs.writeFile(path.join(this.outputDir, localeName, chunk.fileName), magicString.toString());
		}
	}

	/**
	 * Writes `preloads.json`: for each entry chunk, the chunks it imports
	 * statically after inlining (`{ "<src>": ["scripts/<file>", ...] }`).
	 *
	 * `dynamicImports` maps an entry to the modules it imports dynamically as
	 * soon as it starts, named by their manifest key or by a path the key ends
	 * with (for files outside the package, such as `icons-subsetter/built/...`).
	 * Their chunks and static imports, and their stylesheets
	 * (`assets/<file>.css`), are appended to the entry's list.
	 */
	async savePreloads(options: { dynamicImports?: Record<string, string[]> } = {}) {
		await fs.writeFile(path.join(this.outputDir, 'preloads.json'), JSON.stringify(this.collectPreloads(options)));
	}

	collectPreloads(options: { dynamicImports?: Record<string, string[]> } = {}): Record<string, string[]> {
		// Elythia: 起動の HTML から entry の静的 import を modulepreload で並べるための一覧 (#3613)。
		// manifest.json の imports は inline 化の前の形で、ブラウザが取りに行かない i18n の chunk
		// (ロケールの全文、ja-JP で約 290KB) を含む。inline 化で消した import を除いて数える。
		const importsByFile = new Map<string, string[]>();
		for (const chunk of this.chunks) {
			if (chunk.sourceCode == null || !chunk.modifications) {
				throw new Error(`Source code or modifications for ${chunk.fileName} is not available.`);
			}
			const removed = chunk.modifications.filter(x => x.type === 'delete');
			importsByFile.set(chunk.fileName, collectStaticImports(chunk.sourceCode, removed));
		}
		const closureOf = (fileName: string) => staticImportClosure(fileName, f => importsByFile.get(f));
		const preloads: Record<string, string[]> = {};
		for (const chunk of this.chunks) {
			if (!chunk.isEntry || chunk.src == null) continue;
			const files = closureOf(chunk.fileName).map(f => `${this.scriptsDir}/${f}`);
			// Elythia: entry が動き出してすぐ動的に読むもの (embed のアイコンの CSS と ui.vue) も
			// 並べる (#3992)。動的 import は manifest にも chunk の中にも静的な形で残らないので、
			// 呼び出し側が manifest のキー (か、その末尾のパス) で名指しする。名指しが外れたらビルドを落とす
			// (黙って一覧が短くなると、先読みが消えたことに気付けない)。
			for (const key of options.dynamicImports?.[chunk.src] ?? []) {
				for (const file of this.#dynamicImportFiles(key, closureOf)) {
					if (!files.includes(file)) files.push(file);
				}
			}
			preloads[chunk.src] = files;
		}
		for (const src of Object.keys(options.dynamicImports ?? {})) {
			if (!Object.hasOwn(preloads, src)) throw new Error(`preloads: entry '${src}' not found`);
		}
		return preloads;
	}

	#resolveManifestKey(name: string): string {
		if (Object.hasOwn(this.manifest, name)) return name;
		// パッケージの外のファイル (アイコンの CSS など) のキーは、vite の root からの相対パスで、
		// 依存の置き方 (symlink の解決先) で変わる。末尾のパスで 1 つに決まるときだけ採る。
		const matches = Object.keys(this.manifest).filter(k => k.endsWith('/' + name));
		if (matches.length === 1) return matches[0];
		if (matches.length === 0) throw new Error(`preloads: '${name}' not found in manifest.json`);
		throw new Error(`preloads: '${name}' matches more than one manifest key: ${matches.join(', ')}`);
	}

	#dynamicImportFiles(name: string, closureOf: (fileName: string) => string[]): string[] {
		const key = this.#resolveManifestKey(name);
		const target = this.manifest[key];
		if (target.file.endsWith('.css')) return [target.file];
		if (!this.isScriptFile(target.file)) throw new Error(`preloads: '${key}' is neither a script chunk nor a stylesheet (${target.file})`);

		const fileName = this.stripScriptDir(target.file);
		const files = [fileName, ...closureOf(fileName)].map(f => `${this.scriptsDir}/${f}`);
		// CSS は manifest の imports を辿って集める (vite は chunk ごとに CSS を切り出す)。
		// 辿る先には inline 化で消える i18n の chunk も入るが、CSS を持たないので影響しない。
		const seen = new Set<string>();
		const walk = (k: string) => {
			if (seen.has(k)) return;
			seen.add(k);
			const chunk = this.manifest[k] as ViteManifest[string] | undefined;
			if (chunk == null) return;
			for (const css of chunk.css ?? []) {
				if (!files.includes(css)) files.push(css);
			}
			for (const next of chunk.imports ?? []) walk(next);
		};
		walk(key);
		return files;
	}

	isScriptFile(fileName: string) {
		return fileName.startsWith(this.scriptsDir + '/') && fileName.endsWith('.js');
	}

	stripScriptDir(fileName: string) {
		if (!fileName.startsWith(this.scriptsDir + '/')) {
			throw new Error(`${fileName} does not start with ${this.scriptsDir}/`);
		}
		return fileName.slice(this.scriptsDir.length + 1);
	}
}

interface ScriptChunk {
	fileName: string;
	chunkName?: string;
	src?: string;
	isEntry?: boolean;
	sourceCode?: string;
	isFacadeOfI18n?: true;
	modifications?: TextModification[];
}

export type TextModification = {
	type: 'delete';
	begin: number;
	end: number;
	localizedOnly: boolean;
} | {
	// can be used later to insert '../scripts' for common files
	type: 'insert';
	begin: number;
	text: string;
	localizedOnly: boolean;
} | {
	type: 'replace';
	begin: number;
	end: number;
	text: string;
	localizedOnly: boolean;
} | {
	type: 'localized';
	begin: number;
	end: number;
	localizationKey: string[];
	localizedOnly: true;
} | {
	type: 'parameterized-function';
	begin: number;
	end: number;
	localizationKey: string[];
	localizedOnly: true;
} | {
	type: 'locale-name';
	begin: number;
	end: number;
	literal: boolean;
	localizedOnly: true;
} | {
	type: 'locale-json';
	begin: number;
	end: number;
	localizedOnly: true;
};
