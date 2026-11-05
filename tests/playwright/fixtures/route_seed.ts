/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 全ルートの表 (route_table.ts) のパラメータを埋めるために作るデータ (#4041)。
//
// どれも root が作り、root が開いて中身を確かめられるものにする。チャットと
// 対局は相手が要るので、相手 (peer) を作って root と相互フォローにする
// (チャットは相互フォローでないと送れない設定がある)。名前には実行ごとに
// 変わる接尾辞を付け、同じ stack で何度流しても衝突しないようにする。

import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { type APIRequestContext, expect } from '@playwright/test';
import { callApi } from './api';
import { DEFAULT_TEST_PASSWORD, type Principal, randomUsername, signupUser } from './auth';
import { uploadTinyPNG } from './files';
import { awaitSubscribeBuffer, openStream, subscribeChannel } from './streaming';
import type { RootFixture } from './ui_auth';

/**
 * The shape of RouteSeed. Bump it when a field is added, so that a seed
 * cached by an older spec is made again.
 */
export const ROUTE_SEED_VERSION = 2;

/** The values route_table.ts fills the route parameters with. */
export interface RouteSeed {
  version: number;
  suffix: string;
  root: RootFixture;
  peer: Principal;
  /** A user with no role: opens no moderator route. */
  outsider: Principal;
  /** A user whose role makes chatAvailability unavailable: opens no chat route. */
  noChat: Principal;
  noteId: string;
  noteText: string;
  pageName: string;
  pageTitle: string;
  pageId: string;
  listId: string;
  listName: string;
  clipId: string;
  clipName: string;
  chatRoomId: string;
  chatRoomName: string;
  chatMessageId: string;
  chatMessageText: string;
  webhookId: string;
  webhookName: string;
  announcementId: string;
  announcementTitle: string;
  roleId: string;
  roleName: string;
  authToken: string;
  appName: string;
  flashId: string;
  flashTitle: string;
  galleryPostId: string;
  galleryTitle: string;
  channelId: string;
  channelName: string;
  registryScope: string;
  registryKey: string;
  fileId: string;
  fileName: string;
  folderId: string;
  folderName: string;
  antennaId: string;
  antennaName: string;
  /** A bubble game match that has been accepted and not started. */
  versusMatchId: string;
  /** A bubble game match that has ended, so that its record can be replayed. */
  versusEndedMatchId: string;
  reversiGameId: string;
}

async function ok<T>(resp: Promise<import('@playwright/test').APIResponse>, what: string, status = 200): Promise<T> {
  const r = await resp;
  expect(r.status(), `${what}: ${await r.text()}`).toBe(status);
  return (status === 204 ? undefined : await r.json()) as T;
}

/**
 * Plays a bubble game match between root and peer to its end and returns its id:
 * root invites, peer accepts, both say ready on the match channel (the REST API
 * has no ready), then root reports game over and peer reports the end.
 */
async function playVersusToEnd(request: APIRequestContext, root: RootFixture, peer: Principal): Promise<string> {
  const invited = await ok<{ id: string }>(callApi(request, 'bubble-game/versus/invite', { i: root.token, userId: peer.id, gameMode: 'normal' }), 'bubble-game/versus/invite (to end)');
  await ok(callApi(request, 'bubble-game/versus/accept', { i: peer.token, matchId: invited.id }), 'bubble-game/versus/accept (to end)');
  const sockets = await Promise.all([openStream(root.token), openStream(peer.token)]);
  try {
    const subs = sockets.map((ws) => subscribeChannel(ws, 'bubbleVersusMatch', { matchId: invited.id }));
    await awaitSubscribeBuffer();
    sockets.forEach((ws, n) => ws.send(JSON.stringify({ type: 'ch', body: { id: subs[n], type: 'ready', body: true } })));
    await expect.poll(async () => {
      const r = await callApi(request, 'bubble-game/versus/show', { i: root.token, matchId: invited.id });
      return r.status() === 200 ? ((await r.json()) as { status: string }).status : `HTTP ${r.status()}`;
    }, { message: 'the match starts when both are ready' }).toBe('playing');
  } finally {
    for (const ws of sockets) ws.close();
  }
  await ok(callApi(request, 'bubble-game/versus/report', {
    i: root.token, matchId: invited.id, score: 0, frame: 0, reason: 'gameOver', logs: [],
  }), 'bubble-game/versus/report (game over)');
  await ok(callApi(request, 'bubble-game/versus/report', {
    i: peer.token, matchId: invited.id, score: 0, frame: 0, reason: 'opponentEnded', logs: [],
  }), 'bubble-game/versus/report (opponent ended)');
  return invited.id;
}

/** Where seedRoutesOnce keeps the seed of the run, next to the root credentials of globalSetup. */
export const ROUTE_SEED_PATH = '.auth/route-seed.json';

/** Where seedRoutesOnce records that making the seed failed in this run. */
export const ROUTE_SEED_FAILED_PATH = '.auth/route-seed.failed';

/**
 * Returns the seed of this run, creating it on the first call.
 *
 * spec のファイルごとに作ると、webhook のように数の上限があるものが足りなくなる
 * (実際に TOO_MANY_WEBHOOKS で落ちた)。実行ごとに 1 回だけ作り、globalSetup の
 * root.json と同じ場所に残す。globalSetup は実行の始めに root のアンテナ・webhook・
 * クリップ・リストを消すので、同じ所でこのファイルも消す (global-setup.ts)。念のため、
 * root の id か RouteSeed の形 (ROUTE_SEED_VERSION) が違えば作り直す。
 */
export async function seedRoutesOnce(request: APIRequestContext, baseURL: string, root: RootFixture): Promise<RouteSeed> {
  // 一度失敗したら、同じ実行の中では作り直さずに落とす。ファイルごと・retry ごとに
  // 作り直すと、作りかけの webhook やアンテナが残って上限を食い、同じ shard の
  // 無関係な spec (webhook_create_form など) まで TOO_MANY_* で落ちる
  if (existsSync(ROUTE_SEED_FAILED_PATH)) {
    throw new Error(`making the route seed failed earlier in this run: ${readFileSync(ROUTE_SEED_FAILED_PATH, 'utf-8')}`);
  }
  if (existsSync(ROUTE_SEED_PATH)) {
    const cached = JSON.parse(readFileSync(ROUTE_SEED_PATH, 'utf-8')) as RouteSeed;
    if (cached.version === ROUTE_SEED_VERSION && cached.root.id === root.id) return { ...cached, root };
  }
  try {
    const seed = await seedRoutes(request, baseURL, root);
    writeFileSync(ROUTE_SEED_PATH, JSON.stringify(seed));
    return seed;
  } catch (e) {
    writeFileSync(ROUTE_SEED_FAILED_PATH, e instanceof Error ? e.message : String(e));
    throw e;
  }
}

/** Creates the data every parameterized route of route_table.ts opens. */
export async function seedRoutes(request: APIRequestContext, baseURL: string, root: RootFixture): Promise<RouteSeed> {
  const suffix = Date.now().toString(36);
  const i = root.token;
  const peer = await signupUser(request, randomUsername('rtpeer'), DEFAULT_TEST_PASSWORD);
  await ok(callApi(request, 'following/create', { i, userId: peer.id }), 'following/create');
  await ok(callApi(request, 'following/create', { i: peer.token, userId: root.id }), 'following/create (peer)');

  const outsider = await signupUser(request, randomUsername('rtout'), DEFAULT_TEST_PASSWORD);
  const noChat = await signupUser(request, randomUsername('rtnochat'), DEFAULT_TEST_PASSWORD);
  const noChatRole = await ok<{ id: string }>(callApi(request, 'admin/roles/create', {
    i, name: `rt-nochat-${suffix}`, description: 'route table fixture: chat unavailable', color: null, iconUrl: null,
    target: 'manual', condFormula: {}, isPublic: false, isExplorable: false,
    isAdministrator: false, isModerator: false, asBadge: false,
    canEditMembersByModerator: false, displayOrder: 0,
    policies: { chatAvailability: { useDefault: false, priority: 0, value: 'unavailable' } },
  }), 'admin/roles/create (no chat)');
  await ok(callApi(request, 'admin/roles/assign', { i, roleId: noChatRole.id, userId: noChat.id }), 'admin/roles/assign', 204);

  const noteText = `rt-note-${suffix}`;
  const note = await ok<{ createdNote: { id: string } }>(callApi(request, 'notes/create', { i, text: noteText, visibility: 'public' }), 'notes/create');

  const pageName = `rt-page-${suffix}`;
  const pageTitle = `RT page ${suffix}`;
  const pg = await ok<{ id: string }>(callApi(request, 'pages/create', {
    i, title: pageTitle, name: pageName, content: [], variables: [], script: '',
  }), 'pages/create');

  const listName = `rt-list-${suffix}`;
  const list = await ok<{ id: string }>(callApi(request, 'users/lists/create', { i, name: listName }), 'users/lists/create');
  await ok(callApi(request, 'users/lists/push', { i, listId: list.id, userId: peer.id }), 'users/lists/push', 204);
  await ok(callApi(request, 'users/lists/update', { i, listId: list.id, isPublic: true }), 'users/lists/update');

  const clipName = `rt-clip-${suffix}`;
  const clip = await ok<{ id: string }>(callApi(request, 'clips/create', { i, name: clipName, isPublic: true }), 'clips/create');

  const chatRoomName = `rt-room-${suffix}`;
  const room = await ok<{ id: string }>(callApi(request, 'chat/rooms/create', { i, name: chatRoomName }), 'chat/rooms/create');
  const chatMessageText = `rt-chat-${suffix}`;
  const message = await ok<{ id: string }>(callApi(request, 'chat/messages/create-to-user', { i, toUserId: peer.id, text: chatMessageText }), 'chat/messages/create-to-user');

  const webhookName = `rt-hook-${suffix}`;
  const webhook = await ok<{ id: string }>(callApi(request, 'i/webhooks/create', {
    i, name: webhookName, url: 'https://example.test/webhook', secret: 'rt-secret', on: ['follow'],
  }), 'i/webhooks/create');

  const announcementTitle = `rt-announce-${suffix}`;
  const announcement = await ok<{ id: string }>(callApi(request, 'admin/announcements/create', {
    i, title: announcementTitle, text: `rt announcement ${suffix}`, imageUrl: null,
  }), 'admin/announcements/create');

  const roleName = `rt-role-${suffix}`;
  const role = await ok<{ id: string }>(callApi(request, 'admin/roles/create', {
    i, name: roleName, description: 'route table fixture', color: null, iconUrl: null,
    target: 'manual', condFormula: {}, isPublic: true, isExplorable: true,
    isAdministrator: false, isModerator: false, asBadge: false,
    canEditMembersByModerator: false, displayOrder: 0, policies: {},
  }), 'admin/roles/create');

  const appName = `rt-app-${suffix}`;
  const app = await ok<{ secret: string }>(callApi(request, 'app/create', {
    name: appName, description: 'route table fixture', permission: ['read:account'],
  }), 'app/create');
  const session = await ok<{ token: string }>(callApi(request, 'auth/session/generate', { appSecret: app.secret }), 'auth/session/generate');

  const flashTitle = `rt-play-${suffix}`;
  const flash = await ok<{ id: string }>(callApi(request, 'flash/create', {
    i, title: flashTitle, summary: '', script: '/// @ 1.0.0', permissions: [],
  }), 'flash/create');

  // 同じ中身のファイルは、drive が前に上げたもの (前の名前) を返す。名前は返った方を使う
  const file = await uploadTinyPNG(request, baseURL, i, `rt-file-${suffix}.png`);
  const fileName = file.name;
  const galleryTitle = `rt-gallery-${suffix}`;
  const gallery = await ok<{ id: string }>(callApi(request, 'gallery/posts/create', {
    i, title: galleryTitle, description: 'route table fixture', fileIds: [file.id], isSensitive: false,
  }), 'gallery/posts/create');

  const channelName = `rt-channel-${suffix}`;
  const channel = await ok<{ id: string }>(callApi(request, 'channels/create', { i, name: channelName }), 'channels/create');

  const registryScope = `rt_scope_${suffix}`;
  const registryKey = 'rt_key';
  await ok(callApi(request, 'i/registry/set', { i, scope: [registryScope], key: registryKey, value: 'rt-value' }), 'i/registry/set', 204);

  const folderName = `rt-folder-${suffix}`;
  const folder = await ok<{ id: string }>(callApi(request, 'drive/folders/create', { i, name: folderName }), 'drive/folders/create');

  const antennaName = `rt-antenna-${suffix}`;
  const antenna = await ok<{ id: string }>(callApi(request, 'antennas/create', {
    i, name: antennaName, src: 'all', keywords: [['route-table']], excludeKeywords: [[]], users: [],
    caseSensitive: false, withReplies: false, withFile: false,
  }), 'antennas/create');

  // バブルゲームの対戦 (Elythia 独自)。リプレイは終わった対局の記録しか出さないので
  // (終わっていない対局には NO_SUCH_MATCH)、1 つ目は終局まで進める。2 つ目は受けた
  // ところで止め、対戦の画面で開く。同じ相手への招待は同時に 1 つなので、この順に作る
  const versusEndedMatchId = await playVersusToEnd(request, root, peer);
  const invited = await ok<{ id: string }>(callApi(request, 'bubble-game/versus/invite', { i, userId: peer.id, gameMode: 'normal' }), 'bubble-game/versus/invite');
  await ok(callApi(request, 'bubble-game/versus/accept', { i: peer.token, matchId: invited.id }), 'bubble-game/versus/accept');

  // リバーシは、互いに相手を指定して呼ぶと対局が成立する
  await ok(callApi(request, 'reversi/match', { i, userId: peer.id }), 'reversi/match', 204);
  const game = await ok<{ id: string }>(callApi(request, 'reversi/match', { i: peer.token, userId: root.id }), 'reversi/match (peer)');

  return {
    version: ROUTE_SEED_VERSION,
    suffix, root, peer, outsider, noChat,
    noteId: note.createdNote.id, noteText,
    pageName, pageTitle, pageId: pg.id,
    listId: list.id, listName,
    clipId: clip.id, clipName,
    chatRoomId: room.id, chatRoomName, chatMessageId: message.id, chatMessageText,
    webhookId: webhook.id, webhookName,
    announcementId: announcement.id, announcementTitle,
    roleId: role.id, roleName,
    authToken: session.token, appName,
    flashId: flash.id, flashTitle,
    galleryPostId: gallery.id, galleryTitle,
    channelId: channel.id, channelName,
    registryScope, registryKey,
    fileId: file.id, fileName,
    folderId: folder.id, folderName,
    antennaId: antenna.id, antennaName,
    versusMatchId: invited.id,
    versusEndedMatchId,
    reversiGameId: game.id,
  };
}
