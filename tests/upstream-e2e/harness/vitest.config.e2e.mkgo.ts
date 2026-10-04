/*
 * 本家の backend e2e を mk-go に向けて実行するための vitest 設定。
 *
 * include は本家の `test/e2e/**` をそのまま指す。差し替えるのは
 * globalSetup (サーバー起動) と setupFiles (DB リセット) の 2 点だけで、
 * テスト本体には一切手を入れない。上流追従でテストが増えれば自動的に
 * こちらの検証対象も増える。
 */

import { defineConfig, mergeConfig } from 'vitest/config';
import { baseConfig } from './vitest.config.js';

export default mergeConfig(
	baseConfig,
	defineConfig({
		test: {
			include: ['./test/e2e/**/*.ts'],
			// 本家の TypeScript 実装を同じ DB に対して起動してしまうものは、mk-go
			// では構造上成立しないので除外する。いずれも TypeORM がスキーマを張り
			// 直し、mk-go の migration にしか無いテーブル / カラム
			// (relay_observed_user, meta.chunkedUploadEnabled, poll.notifiedAt 等) が
			// 消える。そうなると mk-go は起動できず、後続の全ファイルが道連れになる。
			//
			// - move.ts: initTestDb(false) で TypeORM のエンティティ定義から
			//   スキーマを作り直す (29 ファイル中これだけ)
			// - exports.ts / synalio/*: startJobQueue() で本家の NestJS ジョブキューを
			//   プロセス内に起動する
			exclude: [
				...(baseConfig.test?.exclude ?? []),
				'./test/e2e/move.ts',
				'./test/e2e/exports.ts',
				'./test/e2e/synalio/abuse-report.ts',
				'./test/e2e/synalio/user-create.ts',
			],
			globalSetup: './test-server-mkgo/entry.ts',
			setupFiles: ['./test/setup.e2e.mkgo.ts'],
		},
	}),
);
