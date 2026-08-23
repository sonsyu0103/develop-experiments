// ロール変更の画面。
//
// **エラーの出し分けが本題**になります。仕様書は 403 / 404 / 422 を
// 別々の意味で返しており、文言をまとめると
// 「次に何をすればよいか」が伝わらなくなります。
import { expect, test } from '@playwright/test';

import { admin, fail, mockMe, ok } from './api-mock';

const targetPublicID = '01920000-0000-7000-8000-0000000000bb';

test.beforeEach(async ({ page }) => {
  await mockMe(page, admin);
});

test('公開 ID とロールを送り、応答の公開 ID を出す', async ({ page }) => {
  let sent: Record<string, unknown> | null = null;
  await page.route('**/users/*/role', (route) => {
    sent = route.request().postDataJSON() as Record<string, unknown>;
    return ok(route, {
      id: 9,
      action: 'change_role',
      targetType: 'user',
      // **記録は内部 ID だが、API は公開 ID へ詰め替える** (ADR 0011 の 8)。
      // 画面はこれをそのまま他の API へ渡せる必要があります。
      targetId: targetPublicID,
      reason: null,
      createdAt: '2026-08-16T00:00:00Z',
    });
  });

  await page.goto('/admin/roles');
  await page.getByLabel('対象の公開 ID').fill(targetPublicID);
  await page.getByLabel('新しいロール').selectOption('moderator');
  await page.getByRole('button', { name: '変更する' }).click();

  await expect(page.getByText(`${targetPublicID} を moderator にしました`)).toBeVisible();
  expect(sent).toEqual({ role: 'moderator' });
});

test('自分自身は送る前に止める', async ({ page }) => {
  let called = false;
  await page.route('**/users/*/role', (route) => {
    called = true;
    return fail(route, 403, 'PERMISSION_DENIED');
  });

  await page.goto('/admin/roles');
  await page.getByLabel('対象の公開 ID').fill(admin.publicId);

  await expect(page.getByText('自分のロールは変更できません')).toBeVisible();
  await expect(page.getByRole('button', { name: '変更する' })).toBeDisabled();
  expect(called).toBe(false);
});

// **大文字で貼られた UUID でも止めること** (レビュー指摘)。
//
// UUID は 16 進なので、コピー元によっては大文字で来ます。区別して比べると
// isSelf が false のままになり、**「変更する」が押せてしまいます** ——
// サーバは 403 で止めますが、この画面の役目 (送る前に気づかせる) が
// casing だけで消えます。既存の検査は publicId をそのまま入れるので、
// `.toLowerCase()` が有っても無くても通っていました。
test('大文字で貼った自分の公開 ID も送る前に止める', async ({ page }) => {
  // **`called` は見ません** (レビュー指摘)。一度もクリックしない検査では
  // ボタンが有効でも false のままで、**構造的に失敗しえない assert** に
  // なります —— この PR が潰そうとしている「守っているつもり」そのもの。
  // 実際に守っているのは下の toBeDisabled() だけなので、そこだけ見ます。
  await page.goto('/admin/roles');
  await page.getByLabel('対象の公開 ID').fill(admin.publicId.toUpperCase());

  await expect(page.getByText('自分のロールは変更できません')).toBeVisible();
  await expect(page.getByRole('button', { name: '変更する' })).toBeDisabled();
});

test('422 は「最後の admin」だと伝える', async ({ page }) => {
  await page.route('**/users/*/role', (route) => fail(route, 422, 'FAILED_PRECONDITION'));

  await page.goto('/admin/roles');
  await page.getByLabel('対象の公開 ID').fill(targetPublicID);
  await page.getByLabel('新しいロール').selectOption('user');
  await page.getByRole('button', { name: '変更する' }).click();

  // **再試行しても解決しません。** 次にやること (先に別の admin を作る) を出します。
  await expect(
    page.getByText('最後の admin は降格させられません。先に別の利用者を admin にしてください。'),
  ).toBeVisible();
});

test('404 と 403 を取り違えない', async ({ page }) => {
  await page.route('**/users/*/role', (route) => fail(route, 404, 'NOT_FOUND'));

  await page.goto('/admin/roles');
  await page.getByLabel('対象の公開 ID').fill(targetPublicID);
  await page.getByRole('button', { name: '変更する' }).click();

  await expect(
    page.getByText('利用者が見つかりません (公開 ID が違うか、退会しています)。'),
  ).toBeVisible();
});
