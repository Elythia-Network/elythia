import { defineConfig, mergeConfig } from 'vitest/config';
import { getConfig } from './vite.config.js';

export default mergeConfig(getConfig(), defineConfig({
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

