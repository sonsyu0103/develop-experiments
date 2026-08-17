// マイページとログイン状態の共有 (ADR 0005)。
//
// 【この検査の主題は「1 か所で持っていること」】
// ログイン状態は `app/lib/me.ts` が控えていて、ヘッダのナビと画面本体が
// **同じものを見ています。** 部品ごとに `GET /me` を持つと、
// ログアウトしたのにヘッダだけログイン中のまま、という状態が作れます ——
// 型検査もビルドも通り、画面も一見正常に見える壊れ方になります。
import { expect, test } from '@playwright/test';

import { fail, mockMe, moderator, noContent, ok } from './api-mock';

test.describe('マイページ', () => {
  test('未ログインならログインの導線を出す', async ({ page }) => {
    await mockMe(page, null);
    await page.goto('/mypage');

    await expect(page.getByText('ログインしていません')).toBeVisible();
    await expect(page.getByRole('link', { name: 'Google でログインする' })).toBeVisible();
  });

  test('ログイン中はロールと公開 ID を出す', async ({ page }) => {
    await mockMe(page, moderator);
    await page.goto('/mypage');

    await expect(page.getByRole('heading', { name: moderator.displayName })).toBeVisible();
    // **内部 ID は API が返しません。** 外に出る識別子は公開 ID だけです
    // (ADR 0003 未決 #11)。
    await expect(page.getByText(moderator.publicId)).toBeVisible();
    await expect(page.getByText('他人・匿名の投稿も削除できます')).toBeVisible();
  });

  test('ログアウトすると、ヘッダの管理リンクも同時に消える', async ({ page }) => {
    let loggedOut = false;

    // ログアウト後は 401 に変わります (セッションの実体はサーバ側にあり、
    // ログアウトは即座に効きます。ADR 0005 決定 1)。
    await page.route('**/me', (route) =>
      loggedOut
        ? fail(route, 401, 'UNAUTHENTICATED', 'ログインが必要です')
        : ok(route, moderator),
    );
    await page.route('**/auth/logout', (route) => {
      loggedOut = true;
      return noContent(route);
    });

    await page.goto('/mypage');
    await expect(page.getByRole('link', { name: '管理' })).toBeVisible();

    await page.getByRole('button', { name: 'ログアウト' }).click();

    await expect(page.getByText('ログインしていません')).toBeVisible();
    // **ここが本題。** ヘッダは別の部品ですが、同じ控えを見ているので
    // 引き直され、押しても 403 になるリンクは消えます。
    await expect(page.getByRole('link', { name: '管理' })).toHaveCount(0);
  });

  test('プロフィール画像を外しても、/me を取り直さない', async ({ page }) => {
    let meCalls = 0;
    await page.route('**/me', (route) => {
      meCalls += 1;
      return ok(route, moderator);
    });
    // `PUT /me/avatar` は**新しい `Me` をそのまま返します。**
    // 返ってきたものを控えに置けば、もう一往復する必要はありません。
    await page.route('**/me/avatar', (route) =>
      ok(route, { ...moderator, displayName: '画像を外した人' }),
    );

    await page.goto('/mypage');
    await expect(page.getByRole('heading', { name: moderator.displayName })).toBeVisible();
    const before = meCalls;

    await page.getByRole('button', { name: '設定を外す' }).click();

    await expect(page.getByText('プロフィール画像を外しました')).toBeVisible();
    // 応答の内容が画面に反映されていること (= 控えが置き換わったこと)。
    await expect(page.getByRole('heading', { name: '画像を外した人' })).toBeVisible();
    expect(meCalls).toBe(before);
  });

  test('自分の投稿一覧は、無い理由ごと出す', async ({ page }) => {
    await mockMe(page, moderator);
    await page.goto('/mypage');

    // **「準備中」と書きません。** 投稿者で絞る API が無いという
    // こちらの都合なので、そのまま書きます。
    await expect(page.getByText('一覧は用意していません')).toBeVisible();
  });
});
