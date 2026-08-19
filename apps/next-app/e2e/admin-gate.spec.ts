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

import { admin, mockMe, moderator, ok, plainUser } from './api-mock';

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

  test('管理画面に入るときは、ロールを引き直す', async ({ page }) => {
    // **控えはタブが開いているあいだ残ります。** クライアント遷移では
    // セッション切れやロール剥奪が反映されないので、管理画面に入る時点で
    // 一度引き直します —— この部品の役目は「押しても失敗する UI を
    // 見せない」ことなので、古い控えのまま通すと役目を果たしません。
    let calls = 0;
    await page.route('**/me', (route) => {
      calls += 1;
      return ok(route, moderator);
    });
    await page.route('**/moderation/reports*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: '{"reports":[],"nextCursor":null}',
      }),
    );

    await page.goto('/mypage');
    await expect(page.getByRole('heading', { name: moderator.displayName })).toBeVisible();
    const beforeNavigation = calls;

    await page.getByRole('link', { name: '管理' }).click();
    await expect(page.getByRole('heading', { name: '通報キュー' })).toBeVisible();

    // **expect.poll で待つ。** 見出しの表示と /me の引き直しは別々の
    // 取得なので、見出しが出た時点で引き直しが終わっている保証がない。
    // 素の expect だと、遅い実行環境でだけ「まだ 1 回」で落ちる
    // (CI の Playwright で実際に落ちた。手元では 57 件緑だった)。
    await expect.poll(() => calls).toBeGreaterThan(beforeNavigation);
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
