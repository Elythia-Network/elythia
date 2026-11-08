/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 埋め込み (/embed/notes/:id) が、添付の画像・引用したノート・本文のカスタム絵文字を描くことを
// 確かめる (#4072)。embed.spec.ts は本文だけを見ている。
//
// 埋め込みは別のアプリ (frontend-embed) で、data-testid を持たないので、添付は画像の title
// (ファイルの名前)、絵文字は title (絵文字の名前)、引用は本文で見る。実際の使い方に合わせて
// iframe の中で開く (embed.spec.ts と同じ)。埋め込みは SPA のルートの表の外なので、ルートの宣言は無い。

import { expect, test } from '@playwright/test';
import { cleanupApi, expectApiOk, rootFixture } from '../../../../fixtures/admin_table';
import { uploadTinyPNG } from '../../../../fixtures/files';
import { freshUser, postNote } from '../../../../fixtures/ui_timeline';

test.describe('embed: media, quote and custom emoji', () => {
  test.setTimeout(60_000);

  test('an embedded note shows its attachment, quote and custom emoji', async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const me = await freshUser(request, 'pwembm');
    const emojiFile = await uploadTinyPNG(request, baseURL!, root.token, 'pw-embed-emoji.png');
    const emoji = `pwemb${Date.now().toString(36)}`;
    const added = await expectApiOk(request, 'admin/emoji/add', {
      i: root.token, name: emoji, fileId: emojiFile.id, category: null, aliases: [], license: null,
      isSensitive: false, localOnly: false, roleIdsThatCanBeUsedThisEmojiAsReaction: [],
    }) as { id: string };
    try {
      const fileName = `pw-embed-${Date.now().toString(36)}.png`;
      const file = await uploadTinyPNG(request, baseURL!, me.token, fileName);
      const quotedText = `pw quoted ${Date.now().toString(36)}`;
      const quotedId = await postNote(request, me, quotedText);
      const noteId = await postNote(request, me, `pw embed :${emoji}:`, { fileIds: [file.id], renoteId: quotedId });

      await page.setContent(
        `<iframe id="mk" src="${baseURL}/embed/notes/${noteId}" width="600" height="600"></iframe>`,
        { waitUntil: 'domcontentloaded' },
      );
      const frame = page.frameLocator('#mk');
      await expect(frame.getByText('pw embed').first(), '本文が出ない (前提)').toBeVisible({ timeout: 30_000 });
      await expect(frame.locator(`[title="${fileName}"]`).first(), '添付の画像が出ない').toBeAttached();
      await expect(frame.getByText(quotedText).first(), '引用したノートが出ない').toBeVisible();
      await expect(frame.locator(`img[title*="${emoji}"]`).first(), '本文のカスタム絵文字が出ない').toBeVisible();
    } finally {
      await cleanupApi(request, 'admin/emoji/delete', { i: root.token, id: added.id });
    }
  });
});
