import * as fs from 'fs/promises';
import url from 'node:url';
import path from 'node:path';
import { execa } from 'execa';
import locales from 'i18n';
import { LocaleInliner } from '../frontend-builder/locale-inliner.js';
import { createLogger } from '../frontend-builder/logger';
import { minifyLoaderAssets } from '../frontend-builder/minify-loader.js';

// requires node 21 or later
const __dirname = path.dirname(url.fileURLToPath(import.meta.url));
const outputDir = __dirname + '/../../built/_frontend_embed_vite_';

/**
 * @return {Promise<void>}
 */
async function viteBuild() {
	await execa('vite', ['build'], {
		cwd: __dirname,
		stdout: process.stdout,
		stderr: process.stderr,
	});
}


async function buildAllLocale() {
	const logger = createLogger()
	const inliner = await LocaleInliner.create({
		outputDir,
		logger,
		scriptsDir: 'scripts',
		i18nFile: 'src/i18n.ts',
	})

	await inliner.loadFiles();

	inliner.collectsModifications();

	await inliner.saveAllLocales(locales);
	// Elythia: 埋め込みの HTML から先読みする一覧 (#3992)。boot.ts は動き出してすぐ、
	// アイコンの CSS を `await import` し、ui.vue を `defineAsyncComponent` で読む。
	await inliner.savePreloads({
		dynamicImports: {
			'src/boot.ts': ['icons-subsetter/built/tabler-icons-frontendEmbed.css', 'src/ui.vue'],
		},
	});

	if (logger.errorCount > 0) {
		throw new Error(`Build failed with ${logger.errorCount} errors and ${logger.warningCount} warnings.`);
	}
}

async function build() {
	await fs.rm(outputDir, { recursive: true, force: true });
	await viteBuild();
	await minifyLoaderAssets(outputDir);
	await buildAllLocale();
}

await build();
