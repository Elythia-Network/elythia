/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Elythia-specific endpoints that misskey-js does not know about.
 *
 * 型を足したら pending-endpoints.txt から外す。キーと Go のルートは
 * internal/entitycompat の TestElythiaJS_EndpointsMatchRouter が突き合わせるので、
 * キーは `'<path>': {` の形で 1 行に書く。
 */
// eslint-disable-next-line @typescript-eslint/no-empty-object-type
export type ElythiaEndpoints = {
};
