// 管理画面の入口 (AdminGate)。
//
// **これは防御の検査ではありません。** 権限の判定はサーバが行い、
// 画面を隠しても API を直接叩けば通ります (moderation/usecase が持つ領域)。
// ここで見るのは「押せないものを見せないこと」と、
// **401 と 403 で導線が分かれること**になります。
//
// 取り違えると、権限のない利用者がログイン画面に飛ばされ続けます
// (docs/adr/0013-http-defense.md 決定 3)。
import { expect, test } from '@playwright/test';

import { admin, mockMe, moderator, plainUser } from './api-mock';

test.describe('管理画面の入口', () => {
  test('未ログイン (401) にはログインの導線を出す', async ({ page }) => {
    await mockMe(page, null);
    await page.goto('/admin');

    await expect(page.getByText('ログインが必要です')).toBeVisible();
    await expect(page.getByRole('link', { name: 'Google でログインする' })).toBeVisible();
  });

  test('権限不足 (403) にはログインの導線を出さない', async ({ page }) => {
    await mockMe(page, plainUser);
    await page.goto('/admin');

    await expect(page.getByText('この画面を開く権限がありません')).toBeVisible();
    // **ここが本題。** 誰かは分かっている状態なので、
    // ログインし直しても解決しません。
    await expect(page.getByRole('link', { name: 'Google でログインする' })).toHaveCount(0);
    await expect(page.getByText('必要なロール: moderator / 現在のロール: user')).toBeVisible();
  });

  test('moderator には「ロールの変更」を見せない', async ({ page }) => {
    await mockMe(page, moderator);
    await page.route('**/moderation/reports*', (route) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: '{"reports":[],"nextCursor":null}' }),
    );
    await page.goto('/admin');

    await expect(page.getByRole('link', { name: '通報キュー' })).toBeVisible();
    // 開けない画面へのリンクを出すと、403 になるだけでなく
    // **ロールの意味を誤解させます** (決定 1 は権限を分離するためのもの)。
    await expect(page.getByRole('link', { name: 'ロールの変更' })).toHaveCount(0);
  });

  test('admin には「ロールの変更」を見せる', async ({ page }) => {
    await mockMe(page, admin);
    await page.route('**/moderation/reports*', (route) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: '{"reports":[],"nextCursor":null}' }),
    );
    await page.goto('/admin');

    await expect(page.getByRole('link', { name: 'ロールの変更' })).toBeVisible();
  });

  test('moderator がロール変更の画面を開くと 403 表示になる', async ({ page }) => {
    await mockMe(page, moderator);
    await page.goto('/admin/roles');

    await expect(page.getByText('この画面を開く権限がありません')).toBeVisible();
    await expect(page.getByText('必要なロール: admin / 現在のロール: moderator')).toBeVisible();
  });

  test('/me が 500 のときは未ログイン扱いにしない', async ({ page }) => {
    // **401 だけを「未ログイン」に倒します。**
    // 障害中にログイン画面へ送り続けると、原因から遠ざかります。
    await page.route('**/me', (route) =>
      route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: '{"error":{"code":"INTERNAL","message":"想定外のエラー"}}',
      }),
    );
    await page.goto('/admin');

    await expect(page.getByText('状態を確認できませんでした')).toBeVisible();
    await expect(page.getByRole('link', { name: 'Google でログインする' })).toHaveCount(0);
  });
});
