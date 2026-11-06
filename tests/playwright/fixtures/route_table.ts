/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// frontend の全ルートの表 (#4041)。route_table_*.spec.ts が、各ルートを開けること
// (a) と、権限の無い人に画面が出ないこと (c) をこの表で確かめる。
//
// - route は router.definition.ts の path と同じ文字列 (子は親と連結)。全ルートが
//   どこかの spec に宣言されていることは route-coverage.mjs が見る (#4035)。
//   /oauth/authorize だけは載せない (route-coverage-allowlist.tsv に理由がある)
// - **kind は router.definition.ts から作らず、ここに手で書く。** 作ると、ルートの
//   loginRequired や iAmModerator の書き忘れを、表も同じく忘れて拾えない
// - marker は、そのページが mount したときにだけ出るもので書く。ナビの自分の名前、
//   右のウィジェット列の通知、未ログインの左のダッシュボードのように、どのページにも
//   出るもので判定すると not-found でも通る (docs/playwright.md)
//   - title: ページが definePage で付ける title (`<title> | <インスタンス名>` の前半)
//   - text: ページの本体 (RouterView の `._pageContainer` の最も内側) の innerText に
//     含まれる文字列。設定と管理画面の子ページは title が親と共通なので、これで見分ける。
//     **設定の子ページの「本体」には、設定のメニューと案内も入る** (settings/index.vue が
//     自分の `._pageContainer` を持たない)。メニューと同じ文言 (Profile、Privacy など) を
//     子ページの text にすると、どの子ページでも通る。管理画面は index.vue の `.main` が
//     `._pageContainer` なので、メニューは入らない
//   - api: ページが開くときに呼び、2xx が返る API。親が呼ぶもの (管理画面は、どの子を
//     開いても admin/abuse-user-reports を呼ぶ) は子の目印にならない
//   - testId: ページの中の data-testid (data-testid は名前で 1 つに決まるので、本体に
//     絞らない。/verify-email のように RouterView の外に描くページもある)
// - 開いた後のパスが変わらないことも見る (リダイレクトの行を除く)。/settings と /admin は
//   広い画面だと最初の子へ移るので、狭い画面で開く (narrow)

import type { RouteSeed } from './route_seed';

/**
 * How a route treats who opens it.
 * - public: anyone can open it
 * - login: loginRequired; signed out, the not-found page with the sign-in dialog
 * - chat: login, and not-found for a role whose chatAvailability is unavailable
 * - moderator: not-found unless the viewer is a moderator
 * - redirect: renders nothing and moves to another path
 */
export type RouteKind = 'public' | 'login' | 'chat' | 'moderator' | 'redirect';

/** What tells the page apart once it has mounted. Every given field must hold. */
export interface RouteMarker {
  title?: string;
  text?: string;
  api?: string;
  testId?: string;
}

/** The spec file a route is checked in, to keep the files of similar length. */
export type RouteArea = 'content' | 'settings' | 'admin' | 'my' | 'misc';

export interface RouteEntry {
  route: string;
  kind: RouteKind;
  area: RouteArea;
  path: (s: RouteSeed) => string;
  /** For every kind but redirect. */
  marker?: (s: RouteSeed) => RouteMarker;
  /** For redirect: the path it moves to, with the hash if it has one. */
  redirectTo?: (s: RouteSeed) => string;
  /**
   * The viewport to open the route in. /settings and /admin move to their
   * first child on a wide screen and show their own menu only on a narrow one.
   */
  narrow?: boolean;
  /**
   * How a public route differs when opened signed out, with why. Each given
   * difference must happen, so that a stale one is noticed.
   */
  signedOut?: {
    /** The page sets no title. */
    noTitle?: string;
    /** The page raises page errors, all matching `match`, for `reason`. */
    pageError?: { reason: string; match: RegExp };
  };
}

// 未ログインで開くと、ログインの要る API を呼んで失敗を受けないページ (本家から来たもの)
// misskeyApi は失敗を Error でない object で reject するので、page error の文面は "Object" になる
const callsSignedInApi = {
  pageError: { reason: '未ログインでもログインの要る API を呼び、失敗を受けない (本家から来たページ。Elythia の差分は読み込みに関係しない)', match: /^Object$/ },
};

const at = (p: string) => () => p;

export const ROUTE_TABLE: RouteEntry[] = [
  // タイムライン・ノート・利用者
  { route: '/', kind: 'public', area: 'content', path: at('/'), marker: () => ({ title: 'Timeline', api: 'notes/timeline' }), signedOut: { noTitle: '未ログインでは入口のページ (welcome.vue) を出し、title を付けない' } },
  { route: '/timeline', kind: 'public', area: 'content', path: at('/timeline'), marker: () => ({ title: 'Timeline', api: 'notes/timeline' }) },
  { route: '/@:username/pages/:pageName(*)', kind: 'public', area: 'content', path: (s) => `/@${s.root.username}/pages/${s.pageName}`, marker: (s) => ({ title: s.pageTitle, api: 'pages/show' }) },
  { route: '/@:acct/following', kind: 'public', area: 'content', path: (s) => `/@${s.root.username}/following`, marker: (s) => ({ title: `@${s.root.username}`, api: 'users/following' }) },
  { route: '/@:acct/followers', kind: 'public', area: 'content', path: (s) => `/@${s.root.username}/followers`, marker: (s) => ({ title: `@${s.root.username}`, api: 'users/followers' }) },
  { route: '/@:acct/:page?', kind: 'public', area: 'content', path: (s) => `/@${s.root.username}`, marker: (s) => ({ title: `@${s.root.username}`, api: 'users/notes' }) },
  { route: '/notes/:noteId/:initialTab?', kind: 'public', area: 'content', path: (s) => `/notes/${s.noteId}`, marker: (s) => ({ title: 'Note', text: s.noteText }) },
  { route: '/list/:listId', kind: 'public', area: 'content', path: (s) => `/list/${s.listId}`, marker: (s) => ({ title: s.listName, text: s.peer.username }) },
  { route: '/clips/:clipId', kind: 'public', area: 'content', path: (s) => `/clips/${s.clipId}`, marker: (s) => ({ title: s.clipName, api: 'clips/notes' }) },
  { route: '/tags/:tag', kind: 'public', area: 'content', path: at('/tags/rttag'), marker: () => ({ title: 'rttag', text: 'Post to this hashtag' }) },
  { route: '/user-tags/:tag', kind: 'public', area: 'content', path: at('/user-tags/rttag'), marker: () => ({ title: 'rttag', api: 'hashtags/users' }) },
  { route: '/explore', kind: 'public', area: 'content', path: at('/explore'), marker: () => ({ title: 'Explore', text: 'Featured' }) },
  { route: '/search', kind: 'public', area: 'content', path: at('/search'), marker: () => ({ title: 'Search', text: 'Note search' }) },
  { route: '/lookup', kind: 'login', area: 'content', path: at('/lookup'), marker: () => ({ title: 'Lookup' }) },
  { route: '/authorize-follow', kind: 'redirect', area: 'content', path: at('/authorize-follow'), redirectTo: at('/lookup') },
  { route: '/authorize_interaction', kind: 'redirect', area: 'content', path: at('/authorize_interaction'), redirectTo: at('/lookup') },
  { route: '/redirect-test', kind: 'redirect', area: 'content', path: at('/redirect-test'), redirectTo: (s) => `/@${s.root.username}` },
  { route: '/share', kind: 'login', area: 'content', path: at('/share'), marker: () => ({ title: 'Share', testId: 'post-form-text' }) },
  // テストの stack には連合先が無く、どのホストも DB に無い。ページは本体を
  // `v-if="instance"` で描くので、確かめられるのはページが mount して照会することまで
  // (中身は連合の試験の段階で見る)
  { route: '/instance-info/:host', kind: 'public', area: 'content', path: at('/instance-info/remote.example'), marker: () => ({ title: 'remote.example', api: 'federation/show-instance' }) },
  { route: '/announcements', kind: 'public', area: 'content', path: at('/announcements'), marker: () => ({ title: 'Announcements', text: 'Current announcements' }) },
  { route: '/announcements/:announcementId', kind: 'public', area: 'content', path: (s) => `/announcements/${s.announcementId}`, marker: (s) => ({ title: s.announcementTitle, api: 'announcements/show' }) },
  { route: '/roles/:roleId', kind: 'public', area: 'content', path: (s) => `/roles/${s.roleId}`, marker: (s) => ({ title: s.roleName, api: 'roles/show' }) },
  { route: '/channels', kind: 'public', area: 'content', path: at('/channels'), marker: () => ({ title: 'Channels', text: 'Trending' }) },
  { route: '/channels/new', kind: 'login', area: 'content', path: at('/channels/new'), marker: () => ({ title: 'Create channel', text: 'Pinned notes' }) },
  { route: '/channels/:channelId', kind: 'public', area: 'content', path: (s) => `/channels/${s.channelId}`, marker: (s) => ({ title: s.channelName, api: 'channels/show' }) },
  { route: '/channels/:channelId/edit', kind: 'login', area: 'content', path: (s) => `/channels/${s.channelId}/edit`, marker: () => ({ title: 'Edit channel', text: 'Pinned notes' }) },
  { route: '/pages', kind: 'public', area: 'content', path: at('/pages'), marker: () => ({ title: 'Pages', text: 'Liked Pages' }) },
  { route: '/pages/new', kind: 'login', area: 'content', path: at('/pages/new'), marker: () => ({ title: 'Create a new Page' }) },
  { route: '/pages/edit/:initPageId', kind: 'login', area: 'content', path: (s) => `/pages/edit/${s.pageId}`, marker: () => ({ title: 'Edit this Page', api: 'pages/show' }) },
  { route: '/play', kind: 'public', area: 'content', path: at('/play'), marker: () => ({ title: 'Play', text: 'My Plays' }) },
  { route: '/play/new', kind: 'login', area: 'content', path: at('/play/new'), marker: () => ({ title: 'Create Play' }) },
  { route: '/play/:id', kind: 'public', area: 'content', path: (s) => `/play/${s.flashId}`, marker: (s) => ({ title: s.flashTitle, api: 'flash/show' }) },
  { route: '/play/:id/edit', kind: 'login', area: 'content', path: (s) => `/play/${s.flashId}/edit`, marker: (s) => ({ title: `Edit Play: ${s.flashTitle}` }) },
  { route: '/gallery', kind: 'public', area: 'content', path: at('/gallery'), marker: () => ({ title: 'Gallery', text: 'Liked Posts' }) },
  { route: '/gallery/new', kind: 'login', area: 'content', path: at('/gallery/new'), marker: () => ({ title: 'Create new gallery post' }) },
  { route: '/gallery/:postId', kind: 'public', area: 'content', path: (s) => `/gallery/${s.galleryPostId}`, marker: (s) => ({ title: s.galleryTitle, api: 'gallery/posts/show' }) },
  { route: '/gallery/:postId/edit', kind: 'login', area: 'content', path: (s) => `/gallery/${s.galleryPostId}/edit`, marker: (s) => ({ title: 'Edit', text: s.fileName }) },
  { route: '/custom-emojis-manager', kind: 'public', area: 'content', path: at('/custom-emojis-manager'), marker: () => ({ title: 'Custom Emoji', text: 'Select mode' }) },
  { route: '/avatar-decorations', kind: 'public', area: 'content', path: at('/avatar-decorations'), marker: () => ({ title: 'Avatar decorations' }), signedOut: { noTitle: 'ページが ensureSignin() を呼び、未ログインでは title を付ける前に止まる (本家と同じ)' } },

  // チャット
  { route: '/chat', kind: 'chat', area: 'content', path: at('/chat'), marker: () => ({ title: 'Chat with user', text: 'Joined rooms' }) },
  { route: '/chat/user/:userId', kind: 'chat', area: 'content', path: (s) => `/chat/user/${s.peer.id}`, marker: (s) => ({ title: s.peer.username, text: s.chatMessageText }) },
  { route: '/chat/room/:roomId', kind: 'chat', area: 'content', path: (s) => `/chat/room/${s.chatRoomId}`, marker: (s) => ({ title: s.chatRoomName, api: 'chat/rooms/show' }) },
  { route: '/chat/messages/:messageId', kind: 'chat', area: 'content', path: (s) => `/chat/messages/${s.chatMessageId}`, marker: (s) => ({ title: 'Chat with user', text: s.chatMessageText }) },

  // 自分のもの (/my/*)
  { route: '/my/notifications', kind: 'login', area: 'my', path: at('/my/notifications'), marker: () => ({ title: 'Notifications', text: 'Mentions' }) },
  { route: '/my/favorites', kind: 'login', area: 'my', path: at('/my/favorites'), marker: () => ({ title: 'Favorites', api: 'i/favorites' }) },
  { route: '/my/achievements', kind: 'login', area: 'my', path: at('/my/achievements'), marker: () => ({ title: 'Achievements' }) },
  { route: '/my/drive', kind: 'login', area: 'my', path: at('/my/drive'), marker: (s) => ({ title: 'Drive', text: s.folderName }) },
  // drive.vue は :folder を読まない (本家と同じ)。中身は /my/drive と同じになる
  { route: '/my/drive/folder/:folder', kind: 'login', area: 'my', path: (s) => `/my/drive/folder/${s.folderId}`, marker: (s) => ({ title: 'Drive', text: s.folderName }) },
  { route: '/my/drive/file/:fileId', kind: 'login', area: 'my', path: (s) => `/my/drive/file/${s.fileId}`, marker: (s) => ({ title: 'File details', text: s.fileName }) },
  { route: '/my/follow-requests', kind: 'login', area: 'my', path: at('/my/follow-requests'), marker: () => ({ title: 'Follow requests', text: 'Received request' }) },
  { route: '/my/lists', kind: 'login', area: 'my', path: at('/my/lists'), marker: (s) => ({ title: 'Manage lists', text: s.listName }) },
  { route: '/my/lists/:listId', kind: 'login', area: 'my', path: (s) => `/my/lists/${s.listId}`, marker: (s) => ({ title: s.listName, text: s.peer.username }) },
  { route: '/my/clips', kind: 'login', area: 'my', path: at('/my/clips'), marker: (s) => ({ title: 'Clip', text: s.clipName }) },
  { route: '/my/antennas', kind: 'login', area: 'my', path: at('/my/antennas'), marker: (s) => ({ title: 'Manage Antennas', text: s.antennaName }) },
  { route: '/my/antennas/create', kind: 'login', area: 'my', path: at('/my/antennas/create'), marker: () => ({ title: 'Create an antenna' }) },
  { route: '/my/antennas/:antennaId', kind: 'login', area: 'my', path: (s) => `/my/antennas/${s.antennaId}`, marker: () => ({ title: 'Edit antenna', api: 'antennas/show' }) },
  { route: '/timeline/list/:listId', kind: 'login', area: 'my', path: (s) => `/timeline/list/${s.listId}`, marker: (s) => ({ title: s.listName, api: 'notes/user-list-timeline' }) },
  { route: '/timeline/antenna/:antennaId', kind: 'login', area: 'my', path: (s) => `/timeline/antenna/${s.antennaId}`, marker: (s) => ({ title: s.antennaName, api: 'antennas/notes' }) },
  { route: '/registry', kind: 'public', area: 'my', path: at('/registry'), marker: (s) => ({ title: 'Registry', text: s.registryScope }), signedOut: callsSignedInApi },
  { route: '/registry/keys/:domain/:path(*)?', kind: 'public', area: 'my', path: (s) => `/registry/keys/@/${s.registryScope}`, marker: (s) => ({ title: 'Registry', text: s.registryKey, api: 'i/registry/keys-with-type' }), signedOut: callsSignedInApi },
  { route: '/registry/value/:domain/:path(*)?', kind: 'public', area: 'my', path: (s) => `/registry/value/@/${s.registryScope}/${s.registryKey}`, marker: (s) => ({ title: 'Registry', text: s.registryKey, api: 'i/registry/get-detail' }), signedOut: callsSignedInApi },

  // 設定 (title は親の Settings なので、本文で見分ける)
  { route: '/settings', kind: 'login', area: 'settings', path: at('/settings'), narrow: true, marker: () => ({ title: 'Settings', text: 'Privacy' }) },
  { route: '/settings/profile', kind: 'login', area: 'settings', path: at('/settings/profile'), marker: () => ({ title: 'Settings', text: 'Change banner' }) },
  { route: '/settings/avatar-decoration', kind: 'login', area: 'settings', path: at('/settings/avatar-decoration'), marker: () => ({ title: 'Settings', text: 'You can add up to' }) },
  { route: '/settings/privacy', kind: 'login', area: 'settings', path: at('/settings/privacy'), marker: () => ({ title: 'Settings', text: 'account privacy' }) },
  { route: '/settings/emoji-palette', kind: 'login', area: 'settings', path: at('/settings/emoji-palette'), marker: () => ({ title: 'Settings', text: 'You can register presets' }) },
  { route: '/settings/drive', kind: 'login', area: 'settings', path: at('/settings/drive'), marker: () => ({ title: 'Settings', text: 'You can manage and configure' }) },
  { route: '/settings/drive/cleaner', kind: 'login', area: 'settings', path: at('/settings/drive/cleaner'), marker: () => ({ title: 'Settings', text: 'Filesizes' }) },
  { route: '/settings/notifications', kind: 'login', area: 'settings', path: at('/settings/notifications'), marker: () => ({ title: 'Settings', text: 'configure the types' }) },
  { route: '/settings/email', kind: 'login', area: 'settings', path: at('/settings/email'), marker: () => ({ title: 'Settings', text: 'does not support sending' }) },
  { route: '/settings/security', kind: 'login', area: 'settings', path: at('/settings/security'), marker: () => ({ title: 'Settings', text: 'account security' }) },
  { route: '/settings/preferences', kind: 'login', area: 'settings', path: at('/settings/preferences'), marker: () => ({ title: 'Settings', text: 'configure the overall behavior' }) },
  { route: '/settings/theme', kind: 'login', area: 'settings', path: at('/settings/theme'), marker: () => ({ title: 'Settings', text: 'Sync Dark Mode with your device settings' }) },
  { route: '/settings/theme/install', kind: 'login', area: 'settings', path: at('/settings/theme/install'), marker: () => ({ title: 'Settings', text: 'Theme code' }) },
  { route: '/settings/theme/manage', kind: 'login', area: 'settings', path: at('/settings/theme/manage'), marker: () => ({ title: 'Settings', testId: 'settings-theme-manage' }) },
  { route: '/settings/navbar', kind: 'login', area: 'settings', path: at('/settings/navbar'), marker: () => ({ title: 'Settings', text: 'Navigation bar' }) },
  { route: '/settings/statusbar', kind: 'login', area: 'settings', path: at('/settings/statusbar'), marker: () => ({ title: 'Settings', testId: 'settings-statusbar' }) },
  { route: '/settings/sounds', kind: 'login', area: 'settings', path: at('/settings/sounds'), marker: () => ({ title: 'Settings', text: 'configure the sound settings' }) },
  { route: '/settings/plugin', kind: 'login', area: 'settings', path: at('/settings/plugin'), marker: () => ({ title: 'Settings', text: 'You can extend client features' }) },
  { route: '/settings/plugin/install', kind: 'login', area: 'settings', path: at('/settings/plugin/install'), marker: () => ({ title: 'Settings', text: 'Please do not install' }) },
  { route: '/settings/account-data', kind: 'login', area: 'settings', path: at('/settings/account-data'), marker: () => ({ title: 'Settings', text: 'Export and import' }) },
  { route: '/settings/mute-block', kind: 'login', area: 'settings', path: at('/settings/mute-block'), marker: () => ({ title: 'Settings', text: 'settings to hide' }) },
  { route: '/settings/connect', kind: 'login', area: 'settings', path: at('/settings/connect'), marker: () => ({ title: 'Settings', text: 'Manage and configure access' }) },
  { route: '/settings/apps', kind: 'login', area: 'settings', path: at('/settings/apps'), marker: () => ({ title: 'Settings', api: 'i/apps' }) },
  { route: '/settings/webhook/new', kind: 'login', area: 'settings', path: at('/settings/webhook/new'), marker: () => ({ title: 'Settings', text: 'When being mentioned' }) },
  { route: '/settings/webhook/edit/:webhookId', kind: 'login', area: 'settings', path: (s) => `/settings/webhook/edit/${s.webhookId}`, marker: () => ({ title: 'Settings', api: 'i/webhooks/show' }) },
  { route: '/settings/deck', kind: 'login', area: 'settings', path: at('/settings/deck'), marker: () => ({ title: 'Settings', text: 'Enable profile information sync' }) },
  { route: '/settings/custom-css', kind: 'login', area: 'settings', path: at('/settings/custom-css'), marker: () => ({ title: 'Settings', text: 'This setting should only be' }) },
  { route: '/settings/profiles', kind: 'login', area: 'settings', path: at('/settings/profiles'), marker: () => ({ title: 'Settings', testId: 'settings-profiles' }) },
  { route: '/settings/accounts', kind: 'login', area: 'settings', path: at('/settings/accounts'), marker: () => ({ title: 'Settings', text: 'Add account' }) },
  { route: '/settings/other', kind: 'login', area: 'settings', path: at('/settings/other'), marker: () => ({ title: 'Settings', text: 'Account Info' }) },

  // 管理画面 (title は親の Control Panel なので、本文で見分ける)
  { route: '/admin', kind: 'moderator', area: 'admin', path: at('/admin'), narrow: true, marker: () => ({ title: 'Control Panel', text: 'Quick actions' }) },
  { route: '/admin/overview', kind: 'moderator', area: 'admin', path: at('/admin/overview'), marker: () => ({ title: 'Control Panel', text: 'Goroutines' }) },
  { route: '/admin/users', kind: 'moderator', area: 'admin', path: at('/admin/users'), marker: () => ({ title: 'Control Panel', api: 'admin/show-users' }) },
  { route: '/admin/emojis', kind: 'moderator', area: 'admin', path: at('/admin/emojis'), marker: () => ({ title: 'Control Panel', text: 'Select mode' }) },
  { route: '/admin/emojis2', kind: 'moderator', area: 'admin', path: at('/admin/emojis2'), marker: () => ({ title: 'Control Panel', text: 'Emoji registration' }) },
  { route: '/admin/avatar-decorations', kind: 'moderator', area: 'admin', path: at('/admin/avatar-decorations'), marker: () => ({ title: 'Control Panel', api: 'admin/avatar-decorations/list' }) },
  { route: '/admin/federation-job-queue', kind: 'moderator', area: 'admin', path: at('/admin/federation-job-queue'), marker: () => ({ title: 'Control Panel', text: 'Federation Jobs' }) },
  { route: '/admin/job-queue', kind: 'moderator', area: 'admin', path: at('/admin/job-queue'), marker: () => ({ title: 'Control Panel', text: 'Job Queue' }) },
  { route: '/admin/files', kind: 'moderator', area: 'admin', path: at('/admin/files'), marker: () => ({ title: 'Control Panel', api: 'admin/drive/files' }) },
  { route: '/admin/federation', kind: 'moderator', area: 'admin', path: at('/admin/federation'), marker: () => ({ title: 'Control Panel', api: 'federation/instances' }) },
  { route: '/admin/announcements', kind: 'moderator', area: 'admin', path: at('/admin/announcements'), marker: (s) => ({ title: 'Control Panel', text: s.announcementTitle }) },
  { route: '/admin/ads', kind: 'moderator', area: 'admin', path: at('/admin/ads'), marker: () => ({ title: 'Control Panel', api: 'admin/ad/list' }) },
  { route: '/admin/roles', kind: 'moderator', area: 'admin', path: at('/admin/roles'), marker: (s) => ({ title: 'Control Panel', text: s.roleName }) },
  { route: '/admin/roles/new', kind: 'moderator', area: 'admin', path: at('/admin/roles/new'), marker: () => ({ title: 'Control Panel', text: 'New role' }) },
  { route: '/admin/roles/:id', kind: 'moderator', area: 'admin', path: (s) => `/admin/roles/${s.roleId}`, marker: (s) => ({ title: 'Control Panel', text: `Role: ${s.roleName}` }) },
  { route: '/admin/roles/:id/edit', kind: 'moderator', area: 'admin', path: (s) => `/admin/roles/${s.roleId}/edit`, marker: (s) => ({ title: 'Control Panel', text: `Edit role: ${s.roleName}` }) },
  { route: '/admin/database', kind: 'moderator', area: 'admin', path: at('/admin/database'), marker: () => ({ title: 'Control Panel', api: 'admin/get-table-stats' }) },
  { route: '/admin/backups', kind: 'moderator', area: 'admin', path: at('/admin/backups'), marker: () => ({ title: 'Control Panel', text: 'Shows the database' }) },
  { route: '/admin/signup-applications', kind: 'moderator', area: 'admin', path: at('/admin/signup-applications'), marker: () => ({ title: 'Control Panel', text: '登録申請' }) },
  { route: '/admin/server-plugins', kind: 'moderator', area: 'admin', path: at('/admin/server-plugins'), marker: () => ({ title: 'Control Panel', api: 'admin/server-plugins' }) },
  { route: '/admin/abuses', kind: 'moderator', area: 'admin', path: at('/admin/abuses'), marker: () => ({ title: 'Control Panel', text: 'Notification settings' }) },
  { route: '/admin/ip-search', kind: 'moderator', area: 'admin', path: at('/admin/ip-search'), marker: () => ({ title: 'Control Panel', text: 'Search by IP address' }) },
  { route: '/admin/ip-lookup-log', kind: 'redirect', area: 'admin', path: at('/admin/ip-lookup-log'), redirectTo: at('/admin/modlog#ipLookup') },
  { route: '/admin/modlog', kind: 'moderator', area: 'admin', path: at('/admin/modlog'), marker: () => ({ title: 'Control Panel', api: 'admin/show-moderation-logs' }) },
  { route: '/admin/settings', kind: 'moderator', area: 'admin', path: at('/admin/settings'), marker: () => ({ title: 'Control Panel', text: 'Instance name' }) },
  { route: '/admin/branding', kind: 'moderator', area: 'admin', path: at('/admin/branding'), marker: () => ({ title: 'Control Panel', text: 'Entrance page style' }) },
  { route: '/admin/moderation', kind: 'moderator', area: 'admin', path: at('/admin/moderation'), marker: () => ({ title: 'Control Panel', text: 'How registrations are accepted' }) },
  { route: '/admin/email-settings', kind: 'moderator', area: 'admin', path: at('/admin/email-settings'), marker: () => ({ title: 'Control Panel', text: 'Enable email distribution' }) },
  { route: '/admin/object-storage', kind: 'moderator', area: 'admin', path: at('/admin/object-storage'), marker: () => ({ title: 'Control Panel', text: 'Use object storage' }) },
  { route: '/admin/security', kind: 'moderator', area: 'admin', path: at('/admin/security'), marker: () => ({ title: 'Control Panel', text: 'Bot Protection' }) },
  { route: '/admin/relays', kind: 'moderator', area: 'admin', path: at('/admin/relays'), marker: () => ({ title: 'Control Panel', api: 'admin/relays/list' }) },
  { route: '/admin/external-services', kind: 'moderator', area: 'admin', path: at('/admin/external-services'), marker: () => ({ title: 'Control Panel', text: 'DeepL' }) },
  { route: '/admin/performance', kind: 'moderator', area: 'admin', path: at('/admin/performance'), marker: () => ({ title: 'Control Panel', text: 'Publish server hardware stats' }) },
  { route: '/admin/invites', kind: 'moderator', area: 'admin', path: at('/admin/invites'), marker: () => ({ title: 'Control Panel', api: 'admin/invite/list' }) },
  { route: '/admin/abuse-report-notification-recipient', kind: 'moderator', area: 'admin', path: at('/admin/abuse-report-notification-recipient'), marker: () => ({ title: 'Control Panel', api: 'admin/abuse-report/notification-recipient/list' }) },
  { route: '/admin/system-webhook', kind: 'moderator', area: 'admin', path: at('/admin/system-webhook'), marker: () => ({ title: 'Control Panel', api: 'admin/system-webhook/list' }) },
  { route: '/admin/user/:userId', kind: 'moderator', area: 'admin', path: (s) => `/admin/user/${s.peer.id}`, marker: (s) => ({ title: s.peer.username, api: 'admin/show-user' }) },
  { route: '/admin/file/:fileId', kind: 'moderator', area: 'admin', path: (s) => `/admin/file/${s.fileId}`, marker: (s) => ({ title: `File: ${s.fileName}`, api: 'admin/drive/show-file' }) },

  // そのほか
  { route: '/about', kind: 'public', area: 'misc', path: at('/about'), marker: () => ({ title: 'Instance Information', text: 'Overview' }) },
  { route: '/about-misskey', kind: 'public', area: 'misc', path: at('/about-misskey'), marker: () => ({ title: 'About Misskey' }) },
  { route: '/about-elythia', kind: 'public', area: 'misc', path: at('/about-elythia'), marker: () => ({ title: 'About Elythia', testId: 'about-elythia-server-source' }) },
  { route: '/contact', kind: 'public', area: 'misc', path: at('/contact'), marker: () => ({ title: 'Contact', text: 'Device information' }) },
  { route: '/ads', kind: 'public', area: 'misc', path: at('/ads'), marker: () => ({ title: 'Advertisements' }) },
  { route: '/invite', kind: 'public', area: 'misc', path: at('/invite'), marker: () => ({ title: 'Invite', api: 'invite/limit' }), signedOut: callsSignedInApi },
  { route: '/signup-application', kind: 'public', area: 'misc', path: at('/signup-application'), marker: () => ({ title: '登録の申請' }) },
  { route: '/emoji-request', kind: 'login', area: 'misc', path: at('/emoji-request'), marker: () => ({ title: 'Request a custom emoji' }) },
  { route: '/reset-password/:token?', kind: 'public', area: 'misc', path: at('/reset-password/rt-token'), marker: () => ({ title: 'Reset password', text: 'New password' }) },
  { route: '/signup-complete/:code', kind: 'public', area: 'misc', path: at('/signup-complete/rt-code'), marker: () => ({ text: 'to complete email verification', testId: 'signup-complete' }) },
  { route: '/verify-email/:code', kind: 'public', area: 'misc', path: at('/verify-email/rt-code'), marker: () => ({ text: 'to complete email verification', testId: 'verify-email-ok' }) },
  { route: '/theme-editor', kind: 'login', area: 'misc', path: at('/theme-editor'), marker: () => ({ title: 'Theme editor', text: 'Background color' }) },
  { route: '/api-console', kind: 'login', area: 'misc', path: at('/api-console'), marker: () => ({ title: 'API console', text: 'Endpoint' }) },
  { route: '/scratchpad', kind: 'public', area: 'misc', path: at('/scratchpad'), marker: () => ({ title: 'Scratchpad', text: 'UI inspector' }) },
  { route: '/preview', kind: 'public', area: 'misc', path: at('/preview'), marker: () => ({ title: 'Preview' }) },
  { route: '/auth/:token', kind: 'public', area: 'misc', path: (s) => `/auth/${s.authToken}`, marker: (s) => ({ title: 'Granting application permissions', text: s.appName }) },
  { route: '/miauth/:session', kind: 'public', area: 'misc', path: at('/miauth/00000000-0000-4000-8000-000000000000?name=rt-miauth'), marker: () => ({ title: 'MiAuth', text: 'Select account' }) },
  { route: '/install-extentions', kind: 'redirect', area: 'misc', path: at('/install-extentions'), redirectTo: at('/install-extensions') },
  { route: '/install-extensions', kind: 'login', area: 'misc', path: at('/install-extensions'), marker: () => ({ title: 'Install from external site', text: 'Invalid parameters' }) },
  { route: '/games', kind: 'public', area: 'misc', path: at('/games'), marker: () => ({ title: 'Elythia Games' }) },
  { route: '/clicker', kind: 'login', area: 'misc', path: at('/clicker'), marker: () => ({ title: '🍪👈', testId: 'count' }) },
  { route: '/bubble-game', kind: 'login', area: 'misc', path: at('/bubble-game'), marker: () => ({ title: 'Bubble Game' }) },
  { route: '/bubble-game/versus/history/:userId?', kind: 'login', area: 'misc', path: at('/bubble-game/versus/history'), marker: () => ({ title: 'Match history', api: 'bubble-game/versus/history' }) },
  // リプレイは終わった対局の記録しか出さない (終わっていなければ「見つかりません」)。
  // 相手の名前は記録が読めたときにだけ出る
  { route: '/bubble-game/versus/replay/:matchId', kind: 'login', area: 'misc', path: (s) => `/bubble-game/versus/replay/${s.versusEndedMatchId}`, marker: (s) => ({ title: 'Match replay', text: s.peer.username, api: 'bubble-game/versus/record' }) },
  { route: '/bubble-game/versus/:matchId', kind: 'login', area: 'misc', path: (s) => `/bubble-game/versus/${s.versusMatchId}`, marker: (s) => ({ title: 'Bubble Game Versus', text: s.peer.username, api: 'bubble-game/versus/show' }) },
  { route: '/reversi', kind: 'public', area: 'misc', path: at('/reversi'), marker: () => ({ title: 'Reversi', text: 'Free Match' }), signedOut: callsSignedInApi },
  { route: '/reversi/g/:gameId', kind: 'public', area: 'misc', path: (s) => `/reversi/g/${s.reversiGameId}`, marker: (s) => ({ title: 'Reversi', text: `${s.root.username} vs ${s.peer.username}` }) },
  { route: '/qr', kind: 'login', area: 'misc', path: at('/qr'), marker: () => ({ title: 'QR Code', text: 'Scan' }) },
  { route: '/debug', kind: 'public', area: 'misc', path: at('/debug'), marker: () => ({ title: 'DEBUG ROOM' }) },
  { route: '/:(*)', kind: 'public', area: 'misc', path: at('/no-such-route-rt'), marker: () => ({ title: 'Not found' }) },
];
