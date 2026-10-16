/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { parseAst } from 'rolldown/parseAst';

/**
 * Returns the sibling chunks a built chunk imports statically
 * (`import ... from "./x.js"`, `import "./x.js"`, `export ... from "./x.js"`),
 * leaving out the import statements that lie inside one of `removedRanges`.
 *
 * The locale inliner deletes the import of the i18n chunk from every chunk it
 * inlines, so the browser never fetches that chunk. Passing those deletions as
 * `removedRanges` gives the imports of the localized output.
 */
export function collectStaticImports(sourceCode: string, removedRanges: { begin: number; end: number }[] = []): string[] {
	if (sourceCode === '') return [];
	const program = parseAst(sourceCode);
	const imports: string[] = [];
	for (const node of program.body) {
		if (node.type !== 'ImportDeclaration' && node.type !== 'ExportNamedDeclaration' && node.type !== 'ExportAllDeclaration') continue;
		const source = node.source;
		if (source == null || typeof source.value !== 'string') continue;
		if (!source.value.startsWith('./')) continue;
		if (removedRanges.some(r => r.begin <= node.start && node.end <= r.end)) continue;
		const fileName = source.value.slice(2);
		if (!imports.includes(fileName)) imports.push(fileName);
	}
	return imports;
}

/**
 * Walks `importsOf` from `entry` and returns every chunk the entry needs
 * before it can run, in breadth-first order and without the entry itself.
 */
export function staticImportClosure(entry: string, importsOf: (fileName: string) => string[] | undefined): string[] {
	const seen = new Set<string>([entry]);
	const order: string[] = [];
	const queue = [entry];
	for (let current = queue.shift(); current != null; current = queue.shift()) {
		for (const next of importsOf(current) ?? []) {
			if (seen.has(next)) continue;
			seen.add(next);
			order.push(next);
			queue.push(next);
		}
	}
	return order;
}
