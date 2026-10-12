/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

let call: <T>(endpoint: string, params?: Record<string, unknown>) => Promise<T>;

/** Called once from the plugin's setup. */
export function initApi(fn: typeof call): void {
	call = fn;
}

export function api<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
	return call<T>(`plugin/trustlevel/${path}`, params);
}

export type Overview = {
	config: {
		roleId: string;
		actorId: string;
		minAccountAgeDays: number;
		minNotes: number;
		cron: string;
	};
	counts: {
		total: number;
		granted: number;
		held: number;
		failing: number;
	};
	runs: {
		startedAt: string;
		finishedAt: string;
		elapsedMs: number;
		scanned: number;
		granted: number;
		failed: number;
		error: string;
	}[];
};

export type Subject = {
	userId: string;
	granted: boolean;
	held: boolean;
	reason: string;
	lastError: string;
	evaluatedAt: string | null;
};
