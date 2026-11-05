/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// センシティブなファイルの付いたノートでは、画像が隠れて出て、クリックで表示される
// ことを確かめる (#4043)。
//
// 最初のクリックは表示するだけで、ライトボックスは開かない (MkMediaImage の onClick)。
// 隠れていることは、隠しの覆い (media-image-hidden) と、ファイルが API で
// isSensitive であることの両方で見る。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { tinyPNG } from '../../../fixtures/files';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickOnlyTestId } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: note sensitive media', () => {
  test.setTimeout(90_000);

  test('a sensitive image is hidden until clicked', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwnsfwto'));
    const user = await signupUser(request, randomUsername('pwnsfw'));
    const upload = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: author.token,
        isSensitive: 'true',
        file: { name: `pw-nsfw-${Date.now()}.png`, mimeType: 'image/png', buffer: tinyPNG },
      },
    });
    expect(upload.status()).toBe(200);
    const file = await upload.json();
    const shown = await (await callApi(request, 'drive/files/show', { i: author.token, fileId: file.id })).json();
    expect(shown.isSensitive).toBe(true);
    const text = `pw-nsfw-${Date.now()}`;
    const resp = await callApi(request, 'notes/create', { i: author.token, text, fileIds: [file.id] });
    expect(resp.status()).toBe(200);
    const noteId = (await resp.json()).createdNote.id;
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${noteId}`);
    await expect(page.getByText(text, { exact: true })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('media-image')).toHaveCount(1);
    const hidden = page.getByTestId('media-image-hidden');
    await expect(hidden).toHaveCount(1);
    await expect(hidden).toContainText('Sensitive');

    await clickOnlyTestId(page, 'media-image');
    await expect(hidden).toHaveCount(0);
    await expect(page.getByTestId('lightbox')).toHaveCount(0);
  });
});
