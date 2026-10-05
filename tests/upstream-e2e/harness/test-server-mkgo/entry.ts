/*
 * mk-go 版の vitest globalSetup。
 *
 * 本家の `test-server/entry.ts` は NestJS アプリをプロセス内で起動するが、
 * ここでは代わりに mk-go のバイナリを子プロセスとして起動する。`test/e2e/**`
 * は HTTP でしかサーバーに触らないので、この 1 ファイルを差し替えるだけで
 * 本家のテストをそのまま mk-go に向けられる。
 */

import { spawn, type ChildProcess } from 'node:child_process';
import { openSync } from 'node:fs';
import { setTimeout as sleep } from 'node:timers/promises';
import Fastify from 'fastify';
import { loadConfig } from '@/config.js';

const config = loadConfig();

// 既定値は持たない (#3378)。手元のパスを既定値にすると、別の環境では黙って
// 存在しないバイナリを起動しに行く。Makefile の upstream-e2e-test が必ず渡す。
function requireEnv(name: string): string {
	const v = process.env[name];
	if (!v) {
		throw new Error(`${name} is not set (run via \`make upstream-e2e-test\`)`);
	}
	return v;
}

const MKGO_BIN = requireEnv('MKGO_BIN');
const MKGO_CONFIG = requireEnv('MKGO_CONFIG');
const MKGO_CWD = requireEnv('MKGO_CWD');
const MKGO_LOG = process.env.MKGO_LOG ?? '/tmp/mkgo-upstream-e2e.log';

let proc: ChildProcess | null = null;

/**
 * `/env` で積まれた環境変数。
 *
 * 本家は `process.env` を書き換えるだけで済むが、mk-go は起動時に設定を
 * 読み切るので、ここに溜めて再起動時に渡す。
 */
let extraEnv: Record<string, string> = {};

async function waitForReady(timeoutMs = 60_000): Promise<void> {
	const deadline = Date.now() + timeoutMs;
	let lastError: unknown = null;

	while (Date.now() < deadline) {
		if (proc?.exitCode != null) {
			throw new Error(`mk-go exited early with code ${proc.exitCode}. See ${MKGO_LOG}`);
		}
		try {
			const res = await fetch(`http://127.0.0.1:${config.port}/api/meta`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: '{}',
			});
			if (res.ok) return;
			lastError = new Error(`status ${res.status}`);
		} catch (e) {
			lastError = e;
		}
		await sleep(300);
	}

	throw new Error(`mk-go did not become ready within ${timeoutMs}ms: ${lastError}. See ${MKGO_LOG}`);
}

async function startMkGo(): Promise<void> {
	const out = openSync(MKGO_LOG, 'a');

	proc = spawn(MKGO_BIN, ['serve', '-config', MKGO_CONFIG], {
		cwd: MKGO_CWD,
		env: { ...process.env, ...extraEnv },
		stdio: ['ignore', out, out],
	});

	proc.on('error', (e) => {
		console.error('failed to spawn mk-go:', e);
	});

	await waitForReady();
}

async function stopMkGo(): Promise<void> {
	if (proc == null || proc.exitCode != null) {
		proc = null;
		return;
	}

	const p = proc;
	proc = null;

	await new Promise<void>((resolve) => {
		const killTimer = setTimeout(() => {
			// SIGTERM で落ちなければ強制終了する。次の起動でポートが掴まれたままに
			// なると、以降のテストが全部落ちる。
			p.kill('SIGKILL');
		}, 5_000);

		p.once('exit', () => {
			clearTimeout(killTimer);
			resolve();
		});

		p.kill('SIGTERM');
	});

	// ポートが解放されるまでわずかに待つ。
	await sleep(200);
}

async function restartMkGo(): Promise<void> {
	await stopMkGo();
	await startMkGo();
}

/**
 * 本家 `test-server/entry.ts` の `startControllerEndpoints` 相当。
 *
 * `/env` は本家では `process.env` の書き換えだけで完結するが、mk-go は起動時に
 * 設定を読むため、ここでは値を溜めた上でプロセスごと再起動する。
 */
async function startControllerEndpoints(port = config.port + 1000): Promise<void> {
	const fastify = Fastify();

	fastify.post<{ Body: { key?: string, value?: string } }>('/env', async (req, res) => {
		const key = req.body['key'];
		if (!key) {
			res.code(400).send({ success: false });
			return;
		}

		if (req.body['value'] == null || req.body['value'] === '') {
			delete extraEnv[key];
		} else {
			extraEnv[key] = req.body['value'];
		}

		await restartMkGo();
		res.code(200).send({ success: true });
	});

	fastify.post('/env-reset', async (req, res) => {
		extraEnv = {};
		await restartMkGo();
		res.code(200).send({ success: true });
	});

	await fastify.listen({ port, host: 'localhost' });
}

export async function setup(): Promise<void> {
	console.log(`starting mk-go (${MKGO_BIN})...`);
	await startMkGo();
	await startControllerEndpoints();
	console.log('mk-go ready.');
}

export async function teardown(): Promise<void> {
	await stopMkGo();
}
