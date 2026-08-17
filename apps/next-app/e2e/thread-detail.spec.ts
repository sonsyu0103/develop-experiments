// スレッド詳細の状態遷移 (ADR 0020)。
//
// **ここで見るのは、型検査もビルドも通るのに壊れている領域**になります。
//
//   - 遅れて届いた応答が、更新後の一覧に混ざる
//   - 削除の進行中の印を 1 つしか持たず、行をまたぐと二重送信が通る
//   - 取得に失敗しただけなのに「コメントはありません」と出る
//   - 404 に再試行の導線を出す (何度押しても同じ結果になる)
//
// 応答は `page.route` で差し替えます。**実バックエンドは使いません** ——
// 「A の応答が B より後に届く」は実 API では作れないためです。
import { expect, test, type Page } from '@playwright/test';

import {
  byUser,
  comment,
  comments,
  delay,
  fail,
  mockComments,
  mockMe,
  mockThread,
  noContent,
  ok,
  plainUser,
  thread,
} from './api-mock';

/** どの検査でも同じスレッドを見ます。 */
const threadId = 100;
const url = `/threads/${threadId}`;

/** スレッド本体だけを差し替えます (コメントは各検査が決めます)。 */
async function mockThreadOk(page: Page, over = {}) {
  await mockThread(page, (route) => ok(route, thread(over)));
}

test.describe('スレッド詳細', () => {
  test('存在しないスレッドには再試行を出さない', async ({ page }) => {
    await mockMe(page, null);
    await mockThread(page, (route) => fail(route, 404, 'NOT_FOUND', '見つかりません'));
    await mockComments(page, (route) => ok(route, comments([])));
    await page.goto(url);

    await expect(page.getByText('スレッドが見つかりません')).toBeVisible();
    // **何度押しても同じ結果になるものを押させません。**
    // 404 は消えたということなので、再試行ではなく一覧へ戻す導線を出します。
    await expect(page.getByRole('button', { name: '再試行する' })).toHaveCount(0);
    await expect(page.getByRole('link', { name: 'スレッド一覧へ' })).toBeVisible();
  });

  test('コメントの取得に失敗したら「ありません」とは書かない', async ({ page }) => {
    await mockMe(page, null);
    await mockThreadOk(page);
    await mockComments(page, (route) => fail(route, 500, 'INTERNAL', '想定外のエラー'));
    await page.goto(url);

    await expect(page.getByText('コメントを取得できませんでした')).toBeVisible();
    // **ここが本題。** 取得に失敗しただけなのに「ありません」と出すと、
    // 投稿が消えたように見えます (ADR 0020 ④)。
    await expect(page.getByText('まだコメントはありません')).toHaveCount(0);
    await expect(page.getByRole('button', { name: '再試行する' })).toBeVisible();
  });

  test('遅れて届いた「もっと読む」は、更新後の一覧に混ざらない', async ({ page }) => {
    await mockMe(page, null);
    await mockThreadOk(page);

    let firstPageCount = 0;
    await mockComments(page, async (route, cursor) => {
      if (cursor !== null) {
        // **続きの応答をわざと遅らせます。** この間に「最新の状態にする」を
        // 押すと、古い続きが新しい一覧へ追記されるかどうかが見えます。
        await delay(1500);
        return ok(route, comments([comment({ id: 9, seq: 9, body: '古い続き' })]));
      }
      firstPageCount += 1;
      return firstPageCount === 1
        ? ok(route, comments([comment({ id: 1, seq: 1, body: '最初の 1 件' })], 'CURSOR'))
        : ok(route, comments([comment({ id: 2, seq: 2, body: '更新後の 1 件' })]));
    });

    await page.goto(url);
    await expect(page.getByText('最初の 1 件')).toBeVisible();

    await page.getByRole('button', { name: 'もっと読む' }).click();
    await page.getByRole('button', { name: '最新の状態にする' }).click();

    await expect(page.getByText('更新後の 1 件')).toBeVisible();
    // 遅れて届く応答を待ってから確かめます (待たないと、
    // まだ届いていないだけの状態を「混ざっていない」と読んでしまいます)。
    await page.waitForTimeout(2000);
    await expect(page.getByText('古い続き')).toHaveCount(0);
    await expect(page.getByText('最初の 1 件')).toHaveCount(0);
  });

  test('自分のコメントにだけ削除を出す', async ({ page }) => {
    await mockMe(page, plainUser);
    await mockThreadOk(page);
    await mockComments(page, (route) =>
      ok(
        route,
        comments([
          byUser(plainUser, { id: 1, seq: 1, body: '自分の投稿' }),
          comment({ id: 2, seq: 2, body: '匿名の投稿' }),
        ]),
      ),
    );
    await page.goto(url);

    const rows = page.getByRole('listitem');
    await expect(rows.filter({ hasText: '自分の投稿' }).getByRole('button', { name: '削除' })).toBeVisible();
    // **匿名の投稿は誰も自分のものだと示せません** (author_id が NULL)。
    // ここに削除を出すと、押しても 403 になるものを見せることになります。
    await expect(rows.filter({ hasText: '匿名の投稿' }).getByRole('button', { name: '削除' })).toHaveCount(0);
    await expect(rows.filter({ hasText: '匿名の投稿' }).getByRole('button', { name: '通報する' })).toBeVisible();
  });

  test('削除の進行中は、行をまたいでも押せる状態に戻らない', async ({ page }) => {
    await mockMe(page, plainUser);
    await mockThreadOk(page);
    await mockComments(page, (route) =>
      ok(
        route,
        comments([
          byUser(plainUser, { id: 1, seq: 1, body: '遅いほう' }),
          byUser(plainUser, { id: 2, seq: 2, body: '速いほう' }),
        ]),
      ),
    );

    // **#1 の削除だけを遅らせます。** 進行中の印を 1 つの ID で持っていると、
    // 先に終わった #2 の後片付けが #1 のボタンを押せる状態に戻します
    // (ADR 0020 ②。管理画面で実際に出た指摘と同じ形)。
    await page.route(/\/threads\/\d+\/comments\/(\d+)$/, async (route) => {
      const slow = route.request().url().endsWith('/1');
      if (slow) await delay(1500);
      return noContent(route);
    });

    await page.goto(url);
    const rows = page.getByRole('listitem');

    await rows.filter({ hasText: '遅いほう' }).getByRole('button', { name: '削除' }).click();
    await rows.filter({ hasText: '遅いほう' }).getByRole('button', { name: '削除する' }).click();

    await rows.filter({ hasText: '速いほう' }).getByRole('button', { name: '削除' }).click();
    await rows.filter({ hasText: '速いほう' }).getByRole('button', { name: '削除する' }).click();

    // 速いほうは消えます。
    await expect(page.getByText('速いほう')).toHaveCount(0);
    // 遅いほうは**まだ進行中**なので、押せる状態に戻っていてはいけません。
    await expect(
      rows.filter({ hasText: '遅いほう' }).getByRole('button', { name: '削除しています...' }),
    ).toBeDisabled();
  });

  test('匿名で立てたスレッドには削除を出さない', async ({ page }) => {
    await mockMe(page, plainUser);
    // `author` が null = 匿名。**ログインしていても消せません**
    // (投稿者を特定する情報が無いため。ADR 0005 決定 2)。
    await mockThreadOk(page, { author: null });
    await mockComments(page, (route) => ok(route, comments([])));
    await page.goto(url);

    await expect(page.getByRole('button', { name: 'このスレッドを削除' })).toHaveCount(0);
  });

  test('消えた添付画像は「表示できません」と出す', async ({ page }) => {
    await mockMe(page, null);
    await mockThreadOk(page);
    await mockComments(page, (route) =>
      ok(
        route,
        comments([
          comment({
            id: 1,
            seq: 1,
            image: {
              id: '018f2c00-0000-7000-8000-000000000001',
              url: '/images/deleted.jpg',
              width: 800,
              height: 600,
            },
          }),
        ]),
      ),
    );
    // モデレーターに削除された画像は、**行は残るが URL が 404** になります
    // (「削除された」と「元から画像なし」を区別するため。ADR 0016 問題 3)。
    await page.route('**/images/deleted.jpg', (route) => route.fulfill({ status: 404, body: '' }));

    await page.goto(url);
    await expect(page.getByText('この画像は表示できません')).toBeVisible();
  });
});
