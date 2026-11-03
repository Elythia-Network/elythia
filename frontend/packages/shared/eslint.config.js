import globals from 'globals';
import { createRequire } from 'node:module';
import pluginMisskey from '@misskey-dev/eslint-plugin';

// espree は eslint 本体の依存で、workspace の package からは直接 import できない
const espree = createRequire(createRequire(import.meta.url).resolve('eslint'))('espree');

/**
 * Lints the splash loader (public/loader/*.js), which the server inlines into the
 * HTML as a classic script. Put it at the end of a package config.
 * @param {Record<string, 'readonly'>} injected constants the server defines before the loader
 */
export function loaderScriptConfig(injected) {
	return {
		// 起動画面のスクリプト (public/loader/boot.js) は、サーバーが HTML に <script> で
		// 埋め込む。構文の誤りが 1 つあるだけで起動全体が止まるので、必須の CI で拾う (#3954)。
		// TypeScript のパーサーは sourceType: 'script' でも import や型注釈を通すので、
		// 素の JavaScript だけを受ける espree で読む。後の設定 (eslint-plugin-vue の
		// vue/base/setup) が sourceType を module に戻すので、各 package の設定の末尾に置く
		files: ['public/loader/*.js'],
		languageOptions: {
			parser: espree,
			// boot.js はビルドで esbuild の minify を通る (#3968) が、esbuild が下げられない構文
			// (正規表現の修飾子など) はそのまま配られるので、vite の build.target
			// (chrome130 / firefox132 / safari18.2) が解釈できる年に留める。ES2025 の
			// 正規表現の修飾子 ((?i:...)) は safari18.2 で解釈できない
			ecmaVersion: 2024,
			sourceType: 'script',
			parserOptions: {
				sourceType: 'script',
			},
			globals: {
				...globals.browser,
				...injected,
			},
		},
	};
}

export default [
	...pluginMisskey.configs['recommended'],
	{
		files: ['**/*.cjs'],
		languageOptions: {
			sourceType: 'commonjs',
			parserOptions: {
				sourceType: 'commonjs',
			},
		},
	},
	{
		files: ['**/*.js', '**/*.jsx'],
		languageOptions: {
			parserOptions: {
				sourceType: 'module',
			},
		},
	},
	{
		files: ['build.js'],
		languageOptions: {
			globals: globals.node,
		},
	},
	{
		files: ['**/*.js', '**/*.cjs'],
		rules: {
			'@typescript-eslint/no-var-requires': 'off',
		},
	},
	{
		rules: {
			'no-restricted-imports': ['error', {
				paths: [{ name: 'punycode' }],
			}],
			// 型の情報を利用してlintする必要があるため無効化
			// TODO: 有効化検討
			'@typescript-eslint/no-misused-promises': 'off',
			'no-async-promise-executor': 'error',
		},
	},
	{
		// typescript
		files: ['**/*.ts', '**/*.tsx'],
		rules: {
			'@typescript-eslint/no-unused-vars': ['warn', {
				'args': 'all',
				'argsIgnorePattern': '^_',
				'caughtErrors': 'all',
				'caughtErrorsIgnorePattern': '^_',
				'destructuredArrayIgnorePattern': '^_',
				'varsIgnorePattern': '^_',
				'ignoreRestSiblings': true,
			}],
		},
	},
];
