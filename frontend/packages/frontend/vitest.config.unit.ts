import path from 'node:path';
import { defineConfig, mergeConfig } from 'vitest/config';
import type { Plugin } from 'vite';
import { getConfig } from './vite.config.js';

const frontendSrc = path.resolve(__dirname, 'src') + path.sep;
const embedSrc = path.resolve(__dirname, '../frontend-embed/src') + path.sep;
const swSrc = path.resolve(__dirname, '../sw/src') + path.sep;

/**
 * Resolves `@/` inside the sources under ownSrc to ownSrc.
 */
function selfAlias(name: string, ownSrc: string): Plugin {
	// Elythia: frontend-embed と sw のモジュールもこの vitest で試す (#3992、#4264。どちらにも
	// vitest の設定が無い)。alias の `@/` は frontend の src を指すので、それぞれのファイルが
	// 書いた `@/` (alias で frontend の src に書き換わった後のもの) だけを自分の src に戻す。
	return {
		name: `elythia:${name}-self-alias`,
		enforce: 'pre',
		async resolveId(source, importer, options) {
			if (importer == null || !importer.startsWith(ownSrc) || !source.startsWith(frontendSrc)) return null;
			const target = ownSrc + source.slice(frontendSrc.length);
			const resolved = await this.resolve(target, importer, { ...options, skipSelf: true });
			// 自分に無いファイルを、frontend の同名のファイルで黙って解決しない (#3992 のレビュー)
			if (resolved == null) throw new Error(`${name} has no module for ${source} (imported from ${importer})`);
			return resolved;
		},
	};
}

export default mergeConfig(getConfig(), defineConfig({
	plugins: [selfAlias('frontend-embed', embedSrc), selfAlias('sw', swSrc)],
	test: {
		include: ['./test/unit/**/*.test.ts'],
		environment: 'happy-dom',
		setupFiles: ['./test/setup-network.unit.ts', './test/setup.unit.ts', './test/setup-theme.unit.ts'],
		deps: {
			optimizer: {
				web: {
					include: [
						// XXX: misskey-dev/browser-image-resizer has no "type": "module"
						'browser-image-resizer',
					],
				},
			},
		},
		includeSource: ['src/**/*.ts'],
	},
}));
