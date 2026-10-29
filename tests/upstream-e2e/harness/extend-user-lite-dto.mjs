import { readFileSync, writeFileSync } from 'node:fs';

const usersPath = process.argv[2];
if (!usersPath) {
	throw new Error('usage: node extend-user-lite-dto.mjs <upstream test/e2e/users.ts>');
}

const source = readFileSync(usersPath, 'utf8');
const original = '\t\t\tonlineStatus: user.onlineStatus,\n\t\t\tbadgeRoles: user.badgeRoles,\n';
const extended = '\t\t\tonlineStatus: user.onlineStatus,\n'
	+ '\t\t\t// Elythia のローカル登録 fixture は federationStatus が "on"。期待値を固定する。\n'
	+ '\t\t\tfederationStatus: user.host === null ? \'on\' : undefined,\n'
	+ '\t\t\tbadgeRoles: user.badgeRoles,\n';

if (source.includes(extended)) {
	process.exit(0);
}

const occurrences = source.split(original).length - 1;
if (occurrences !== 1) {
	throw new Error(`expected one userLite DTO field block in ${usersPath}, found ${occurrences}`);
}

writeFileSync(usersPath, source.replace(original, extended));
