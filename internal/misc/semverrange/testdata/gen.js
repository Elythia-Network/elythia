// Generates satisfies.json, the table that semverrange_test.go compares
// against. Run from the repository root:
//
//   node internal/misc/semverrange/testdata/gen.js \
//     frontend/node_modules/.pnpm/semver@7.8.5/node_modules/semver \
//     > internal/misc/semverrange/testdata/satisfies.json
//
// The argument is the directory of node-semver 7.8.5. The random cases use a
// fixed seed, so the output is stable.
'use strict'

const path = require('path')
const dir = path.resolve(process.argv[2] || '')
const pkg = require(path.join(dir, 'package.json'))
if (pkg.name !== 'semver' || pkg.version !== '7.8.5') {
  throw new Error(`expected semver 7.8.5 at ${dir}, got ${pkg.name}@${pkg.version}`)
}
const satisfies = require(path.join(dir, 'functions/satisfies.js'))

const long = (ch, n) => ch.repeat(n)

// Versions that ActivityPub servers report, and edge cases of the parser.
const versions = [
  '2025.4.0', '2024.11.0', '2024.11.0-beta.3', '2024.11.1', '2024.10.9',
  '4.3.2+glitch', '4.4.0-beta.1+glitch', '4.0.0', '4.2.9', '4.3.0', '4.3.0-rc.1',
  '2.7.0-1234-gabc', '2024.11.1-cherrypick-4.14.0', '13.14.2', '1.0', '1',
  'v1.2.3', '=1.2.3', '?', '', ' ', ' 1.2.3 ', '\t1.2.3\n', ' 1.2.3　',
  '1.2.3', '1.2.4', '1.2.3-alpha', '1.2.3-0', '1.3.0-0', '2.0.0-0', '0.0.0', '0.0.0-0',
  '0.2.3', '0.2.4', '0.3.0', '0.0.1', '0.0.2', '0.1.0', '2.3.4', '2.3.5', '2.4.0-0',
  '01.2.3', '1.2.3-01', '1.2.3-a.01', '1.2.3-a..b', '1.2.3+', '1.2.3-', 'V1.2.3',
  '9007199254740991.0.0', '9007199254740992.0.0', '1.2.3-9007199254740991',
  '1.2.3-9007199254740990', '1.2.3-9007199254740993', '1.2.3-9007199254740992',
  '1.2.3 1.2.4', '1.2.3-beta.10', '1.2.3-beta.9', '1.2.3-beta', '1.2.3-beta.a',
  '1.2.3-x.7.z.92', '2024.11.0-io.1', '2024.11.0-io.1a', 'x.2.3', '1.x.3',
  '12345678901234567.0.0', '1.2.3-12345678901234567',
  '2.3.4-beta', '2.3.4-alpha', '1.2.0-0', '1.2.0-alpha', '0.2.0-0', '0.0.1-0',
]

const ranges = [
  '*', '', ' ', 'x', 'X', '>=0.0.0', '>=0.0.0-0', '<=2024.11.0', '<2025', '<2025.0.0',
  '>=4.0.0 <4.3.0', '>=4.0.0  <4.3.0', '  >=4.0.0 <4.3.0  ', '1.2.3 - 2.3.4',
  '1.2 - 2.3', '1.2.3 - 2', '* - 2.3.4', '1.2.3 - *', '1.2.3-alpha - 2.3.4-beta',
  'v1.2.3 - v2.3.4', '=1.2.3 - 2', '1.2.3  - 2.3.4', '1.2.3 -  2.3.4', '1.2.3 +x - 2.3.4',
  '~1.2', '~1.2.3', '~1', '~0.0.1', '~1.2.3-beta', '~> 1.2', '~>1.2', '~ 1.2', '~',
  '^0.2.3', '^0.0.1', '^0.0', '^1.2.3', '^1.2', '^1', '^0', '^1.2.3-beta', '^0.0.1-beta',
  '^0.2.3-beta', '^ 1.2', '^', '^x', '~x',
  '1.x', '1.2.*', '1.2.x', '1.*.3', 'x.2', '>1', '>1.2', '>=1.2', '<1.2', '<=1.2', '<=1',
  '=1.x', '>x', '<x', '>=x', '<=*', '>*', '=*',
  '>=1.2.3 || <0.1', '1.2.3 || 2.3.4', '||', ' || ', '1.2.3 ||', '|| 1.2.3', '<0.0.0-0 || 1.2.3',
  '<0.0.0-0', '<0.0.0', '* || 1.2.3', 'abc || 1.2.3',
  '=1.2.3', '1.2.3', 'v1.2.3', '=v1.2.3', '== 1.2.3', '> 1.2.3', '>= 1.2.3', '<  1.2.3',
  '>=1.2.3+build', '1.2.3+build - 2.0.0+x', '^1.2.3+build', '1.2.3+',
  '>=', '<', '>', 'abc', '1.2.3 -', '- 1.2.3', '>=1.2.3 <', '1.2.3.4', '01.2.3', '1.2.03',
  '>=4.0.0\t<4.3.0', '>=4.0.0 <4.3.0', '>=4.0.0　<4.3.0', '>=4.0.0\v<4.3.0',
  '>=2024.11.0-0', '>2024.11.0-beta.2', '<2024.11.0-beta.4', '>=2.7.0-0 <2.8.0',
  '<=4.3.2', '4.3.2', '>=9007199254740991.0.0', '^9007199254740991.0.0', '~9007199254740991',
  '<=9007199254740990.x', '1.2.3-9007199254740993', '>1.2.3-9007199254740992',
  '>=1.2.3 <=1.2.3', '>=1.2.3 >=1.2.3', '1.2.3 1.2.3', '>=1.0.0 * <2.0.0',
  '<0.0.0-0 || >x', '<x || >x', '>=1.2.3-beta.1', '<1.2.3-beta.0',
  '**', '<**', '>=1.2.3 **', '*x*',
]

// Long inputs near MAX_LENGTH and the regexp repetition limits. They are paired
// with a few partners only, to keep the table small.
const longVersions = [
  '1.2.3+' + long('a', 250), '1.2.3+' + long('a', 251),
  '1.2.3-' + long('a', 249), '1.2.3-' + long('a', 250),
  ' 1.2.3+' + long('a', 250), '\u30001.2.3+' + long('a', 249), '\u3000\u30001.2.3+' + long('a', 249),
]
const longVersionPartners = ['*', '1.2.3', '>=1.2.3-0', '<1.2.3', '1.2.3 - 1.2.3', '^1.2.3-a']
const longRanges = [
  '<1.2.3-' + long('a', 250), '>=1.2.3-' + long('a', 249), '1.2.' + long('1', 257),
  '1.2.3+' + long('a', 251), '>=1.0.0+' + long('b', 300) + ' <2', '~1.2.3-' + long('a', 260),
  '1.2.3-' + long('a', 251) + ' - 2', '>=1.2.3 ' + long('1', 300),
]
const longRangePartners = ['1.2.3', '1.2.4', '1.2.3-a', '1.5.0', '2.0.0', '0.1.0']

const versionsUsed = versions
const cases = []
const seen = new Set()
const add = (v, r) => {
  const k = JSON.stringify([v, r])
  if (seen.has(k)) return
  seen.add(k)
  cases.push({ v, r, ok: satisfies(v, r, { includePrerelease: true }) })
}

for (const v of versionsUsed) {
  for (const r of ranges) {
    add(v, r)
  }
}

for (const v of longVersions) {
  for (const r of longVersionPartners) {
    add(v, r)
  }
}
for (const r of longRanges) {
  for (const v of longRangePartners) {
    add(v, r)
  }
}

// JS の空白 (String.prototype.trim と \s が扱う文字) を、バージョンの前後と範囲の
// いろいろな位置に置く (#3558 のレビュー)。U+0085 と U+180E は JS の空白ではない。
const jsSpaces = ['\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2000', '\u2005', '\u200a',
  '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff', '\u0085', '\u180e']
for (const ws of jsSpaces) {
  for (const r of ['*', '1.2.3', '>=1.0.0']) {
    add(ws + '1.2.3' + ws, r)
  }
  const spacedRanges = [
    ws + '1.2.3' + ws,
    '>=1.0.0' + ws + '<2.0.0',
    '1.2.3' + ws + '||' + ws + '2.0.0',
    '^' + ws + '1.2.3',
    '>=' + ws + '1.2.3',
    '~' + ws + '1.2',
    '1.2.3' + ws + '-' + ws + '1.2.5',
  ]
  for (const r of spacedRanges) {
    for (const v of ['1.2.3', '1.2.4', '2.0.0']) {
      add(v, r)
    }
  }
}

// mulberry32 で固定の乱数列を作る。
let seed = 3558
const rand = () => {
  seed = (seed + 0x6D2B79F5) | 0
  let t = seed
  t = Math.imul(t ^ (t >>> 15), t | 1)
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296
}
const pick = (a) => a[Math.floor(rand() * a.length)]

const nums = ['0', '1', '2', '3', '10', '2024', '01', 'x', 'X', '*', '9007199254740991', '9007199254740992']
const pres = ['', '', '', '-0', '-1', '-beta', '-beta.1', '-rc.0.a', '-01', '-a-b', '-']
const builds = ['', '', '', '+b', '+build.1', '+', '+a.b']
const prefixes = ['', '', '', 'v', '=', ' ', 'v=']
const ops = ['', '', '<', '>', '<=', '>=', '=', '~', '^', '~>', '< ', '>= ', '~ ', '^ ', '==', '<>']
const seps = [' ', ' ', '  ', ' - ', ' || ', '||', ' -', '- ', '\t', ' +x ', ' ', ' | ']

const randVersionish = () => {
  const n = 1 + Math.floor(rand() * 3)
  const parts = []
  for (let i = 0; i < n; i++) parts.push(pick(nums))
  if (rand() < 0.2) parts.push(pick(nums))
  return pick(prefixes) + parts.join('.') + pick(pres) + pick(builds)
}

const randRange = () => {
  const n = 1 + Math.floor(rand() * 3)
  let s = ''
  for (let i = 0; i < n; i++) {
    if (i > 0) s += pick(seps)
    s += pick(ops) + randVersionish()
  }
  if (rand() < 0.1) s = ' ' + s
  if (rand() < 0.1) s = s + ' '
  return s
}

const concreteNums = ['0', '1', '2', '3', '10', '2024']
const randVersion = () => {
  if (rand() < 0.15) return randVersionish()
  return [pick(concreteNums), pick(concreteNums), pick(concreteNums)].join('.') + pick(pres) + pick(builds)
}

for (let i = 0; i < 1500; i++) {
  add(randVersion(), randRange())
}

process.stdout.write(JSON.stringify(cases, null, 0).replace(/\},\{/g, '},\n{') + '\n')
