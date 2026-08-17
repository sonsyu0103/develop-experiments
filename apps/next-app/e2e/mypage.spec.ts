// マイページとログイン状態の共有 (ADR 0005)。
//
// 【この検査の主題は「1 か所で持っていること」】
// ログイン状態は `app/lib/me.ts` が控えていて、ヘッダのナビと画面本体が
// **同じものを見ています。** 部品ごとに `GET /me` を持つと、
// ログアウトしたのにヘッダだけログイン中のまま、という状態が作れます ——
// 型検査もビルドも通り、画面も一見正常に見える壊れ方になります。
import { expect, test } from '@playwright/test';

import {
  fail,
  mockMe,
  mockMyComments,
  mockMyThreads,
  moderator,
  myComment,
  myComments,
  noContent,
  ok,
  thread,
  threads,
} from './api-mock';

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

});

// 自分の投稿一覧 (GET /me/threads / GET /me/comments)。
//
// 【この一覧に固有の落とし穴】
// 一覧そのものの状態遷移は ADR 0020 がスレッド詳細で洗ってあるので、
// ここで見るのは**この画面でしか起きない**ものに絞ります。
//
//   - 開いていないタブまで取りに行かないか (マイページを開くだけで 2 往復しない)
//   - 削除済みスレッドへのコメントを、消さずに・リンクにせず出せるか
//   - 匿名の投稿が出ないことを、画面に書いてあるか
test.describe('自分の投稿', () => {
  test('既定でスレッドを出し、コメントは開くまで取りに行かない', async ({ page }) => {
    let threadCalls = 0;
    let commentCalls = 0;

    await mockMe(page, moderator);
    await mockMyThreads(page, (route) => {
      threadCalls += 1;
      return ok(route, threads([thread({ id: 7, title: '自分で立てたスレッド' })]));
    });
    await mockMyComments(page, (route) => {
      commentCalls += 1;
      return ok(route, myComments([myComment()]));
    });

    await page.goto('/mypage');
    await expect(page.getByRole('link', { name: '自分で立てたスレッド' })).toBeVisible();

    expect(threadCalls).toBe(1);
    // **ここが本題。** 両方を先に取ると、マイページを開くだけで 2 往復します。
    // コメント側は 8 区画すべてを走る側なので、無駄撃ちの代償が大きくなります。
    expect(commentCalls).toBe(0);

    await page.getByRole('button', { name: '書いたコメント' }).click();
    await expect(page.getByText('テスト用のコメント')).toBeVisible();
    expect(commentCalls).toBe(1);
  });

  test('一度取ったタブは、戻っても取り直さない', async ({ page }) => {
    let threadCalls = 0;

    await mockMe(page, moderator);
    await mockMyThreads(page, (route) => {
      threadCalls += 1;
      return ok(route, threads([thread({ id: 7, title: '自分で立てたスレッド' })]));
    });
    await mockMyComments(page, (route) => ok(route, myComments([myComment()])));

    await page.goto('/mypage');
    await expect(page.getByRole('link', { name: '自分で立てたスレッド' })).toBeVisible();
    expect(threadCalls).toBe(1);

    await page.getByRole('button', { name: '書いたコメント' }).click();
    await expect(page.getByText('テスト用のコメント')).toBeVisible();
    await page.getByRole('button', { name: '立てたスレッド' }).click();

    // **読み進めた位置を捨てないため。** 戻すたびに取り直すと、
    // 「もっと読む」で開いた続きが毎回先頭に戻ります。
    await expect(page.getByRole('link', { name: '自分で立てたスレッド' })).toBeVisible();
    expect(threadCalls).toBe(1);
  });

  test('削除済みスレッドへのコメントは、消さずにリンクにしない', async ({ page }) => {
    await mockMe(page, moderator);
    await mockMyThreads(page, (route) => ok(route, threads([])));
    await mockMyComments(page, (route) =>
      ok(
        route,
        myComments([
          myComment({ id: 1, threadId: 100, threadTitle: '消されたスレッド', threadDeleted: true }),
          myComment({ id: 2, threadId: 101, threadTitle: '生きているスレッド' }),
        ]),
      ),
    );

    await page.goto('/mypage');
    await page.getByRole('button', { name: '書いたコメント' }).click();

    // **落としません。** 落とすと、自分の投稿が「消えた」のか
    // 「元から無い」のかを本人が区別できなくなります。
    await expect(page.getByText('消されたスレッド')).toBeVisible();
    await expect(page.getByText('このスレッドは削除されています')).toBeVisible();

    // **リンクにはしません。** 押した先は 404 です。
    await expect(page.getByRole('link', { name: '消されたスレッド' })).toHaveCount(0);
    // 生きているほうは辿れること (両方ともリンクを外していないか)。
    await expect(page.getByRole('link', { name: '生きているスレッド' })).toBeVisible();
  });

  test('取得に失敗したら「まだありません」とは書かない', async ({ page }) => {
    await mockMe(page, moderator);
    await mockMyThreads(page, (route) => fail(route, 500, 'INTERNAL', 'DB がダウンしています'));
    await mockMyComments(page, (route) => ok(route, myComments([])));

    await page.goto('/mypage');

    await expect(page.getByText('投稿を取得できませんでした')).toBeVisible();
    // **ここを取り違えると、投稿が消えたように見えます** (ADR 0020 ④)。
    await expect(page.getByText('まだスレッドを立てていません')).toHaveCount(0);
    await expect(page.getByRole('button', { name: '再試行する' })).toBeVisible();
  });

  test('本当に 0 件なら「まだありません」を出す', async ({ page }) => {
    await mockMe(page, moderator);
    await mockMyThreads(page, (route) => ok(route, threads([])));

    await page.goto('/mypage');

    await expect(page.getByText('まだスレッドを立てていません')).toBeVisible();
  });

  test('匿名の投稿が出ないことを画面に書く', async ({ page }) => {
    await mockMe(page, moderator);
    await mockMyThreads(page, (route) => ok(route, threads([])));

    await page.goto('/mypage');

    // 出ないのは仕様ですが、書かないと「消えた」と読まれます (ADR 0005 決定 2)。
    await expect(page.getByText('ログインせずに投稿したものは')).toBeVisible();
  });

  test('続きは受け取ったカーソルをそのまま渡す', async ({ page }) => {
    const seen: Array<string | null> = [];

    await mockMe(page, moderator);
    await mockMyThreads(page, (route, cursor) => {
      seen.push(cursor);
      return cursor === null
        ? ok(route, threads([thread({ id: 7, title: '1 ページ目' })], 'CURSOR-FROM-API'))
        : ok(route, threads([thread({ id: 6, title: '2 ページ目' })]));
    });

    await page.goto('/mypage');
    await expect(page.getByRole('link', { name: '1 ページ目' })).toBeVisible();

    await page.getByRole('button', { name: 'もっと読む' }).click();
    await expect(page.getByRole('link', { name: '2 ページ目' })).toBeVisible();

    // **カーソルは不透明トークンです** (ADR 0018)。
    // 解釈も生成もせず、受け取った値をそのまま返します。
    expect(seen).toEqual([null, 'CURSOR-FROM-API']);
    // 返しきったので、ボタンは消えます。
    await expect(page.getByRole('button', { name: 'もっと読む' })).toHaveCount(0);
    // 前のページも残っていること (追記であって置き換えではない)。
    await expect(page.getByRole('link', { name: '1 ページ目' })).toBeVisible();
  });
});
