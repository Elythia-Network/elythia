/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Elythia from 'elythia-js';

export type ChunkedStartOptions = {
	name?: string;
	folderId?: string | null;
	isSensitive?: boolean;
	caption?: string | null;
};

/**
 * Builds the request of `drive/files/create-chunked/start` for file.
 */
export function chunkedStartRequest(file: File | Blob, options: ChunkedStartOptions): Elythia.Endpoints['drive/files/create-chunked/start']['req'] {
	return {
		name: options.name ?? (file instanceof File ? file.name : 'untitled'),
		size: file.size,
		force: true,
		isSensitive: options.isSensitive ?? false,
		comment: options.caption ?? null,
		// 単発のアップロードの経路 (`if (options.folderId)`) と同じ真偽で見る。空文字を
		// そのまま送ると、宣言 (misskey:id) の検証で 400 になる (#3369)。
		folderId: options.folderId || null,
	};
}
