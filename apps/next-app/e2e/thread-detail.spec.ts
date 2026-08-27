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

  test('更新したら「もっと読む」を押せる状態に戻す', async ({ page }) => {
    await mockMe(page, null);
    await mockThreadOk(page);

    await mockComments(page, async (route, cursor) => {
      if (cursor !== null) {
        // 続きは返ってこないまま (この間に更新する)。
        await delay(2000);
        return ok(route, comments([]));
      }
      return ok(route, comments([comment({ id: 1, seq: 1, body: '1 件目' })], 'CURSOR'));
    });

    await page.goto(url);
    await page.getByRole('button', { name: 'もっと読む' }).click();
    await expect(page.getByRole('button', { name: '取得しています...' })).toBeVisible();

    await page.getByRole('button', { name: '最新の状態にする' }).click();

    // **進行中の印も戻します。** 戻さないと、更新後の一覧に続きがあっても
    // 遅い応答が届くまでボタンが押せません。
    await expect(page.getByRole('button', { name: 'もっと読む' })).toBeEnabled();
  });

  test('削除が 404 なら、その行を残さない', async ({ page }) => {
    await mockMe(page, plainUser);
    await mockThreadOk(page);
    await mockComments(page, (route) =>
      ok(route, comments([byUser(plainUser, { id: 1, seq: 1, body: '先に消された投稿' })])),
    );
    // 別の端末やモデレーターが先に消した場合がこれに当たります。
    await page.route(/\/threads\/\d+\/comments\/\d+$/, (route) =>
      fail(route, 404, 'NOT_FOUND', '見つかりません'),
    );

    await page.goto(url);
    await page.getByRole('button', { name: '削除' }).click();
    await page.getByRole('button', { name: '削除する' }).click();

    // **残すと、何度押しても 404 になる行が居座ります。**
    await expect(page.getByText('先に消された投稿')).toHaveCount(0);
  });

  test('ログイン状態が決まるまで、投稿ごとの操作を出さない', async ({ page }) => {
    // `/me` だけを遅らせます。コメントは先に届きます。
    await page.route('**/me', async (route) => {
      await delay(1500);
      return ok(route, plainUser);
    });
    await mockThreadOk(page);
    await mockComments(page, (route) =>
      ok(route, comments([byUser(plainUser, { id: 1, seq: 1, body: '本人の書き込み' })])),
    );

    await page.goto(url);
    // **コメントの読み取りは待たせません。** 遅れるのは投稿ごとの操作だけです。
    await expect(page.getByText('本人の書き込み')).toBeVisible();

    // **この時点で「通報する」を出してはいけません。** 自分の投稿なので、
    // ログイン状態が決まった瞬間に「削除」へ化けます ——
    // 押そうとした先が変わるのは事故のもとになります。
    await expect(page.getByRole('button', { name: '通報する' })).toHaveCount(0);

    await expect(page.getByRole('button', { name: '削除' })).toBeVisible();
  });

  test('ログイン状態が分からないとき、匿名の画面を出さない', async ({ page }) => {
    // **401 ではなく 500。** me.ts は 401 だけを「未ログイン」に翻訳し、
    // それ以外は `error` = 「分からない」として持ちます (ADR 0013 決定 3)。
    //
    // ここを「解決済み＝未ログイン」に数えると、**ログイン中の利用者に
    // 匿名用の画面が出ます** ——
    //
    //   - 名前欄に書いて投稿しても、Cookie が生きているのでサーバは
    //     authorName を捨て、実名の表示名とアバターで公開される
    //   - 自分の投稿に「削除」ではなく「通報する」が出て、実際に成立する
    await page.route('**/me', (route) => fail(route, 500, 'INTERNAL', 'サーバ内部でエラー'));
    await mockThreadOk(page);
    await mockComments(page, (route) =>
      ok(route, comments([byUser(plainUser, { id: 1, seq: 1, body: '本人の書き込み' })])),
    );

    await page.goto(url);
    await expect(page.getByText('本人の書き込み')).toBeVisible();

    // **先に「確定した」印を待ちます** (レビュー指摘)。
    // 否定の assert は、要素が「まだ出ていない」だけでも通ります ——
    // /me の 500 が届く前に走ると全部空振りで緑になり、
    // 「警告は出るが名前欄も一緒に出る」形の後退を見逃します。
    await expect(page.getByText('ログイン状態を確認できませんでした。')).toBeVisible();

    // **匿名向けの案内を出さない。**
    await expect(page.getByLabel('名前 (任意)')).toHaveCount(0);
    await expect(page.getByText('ログインしなくても投稿できます。')).toHaveCount(0);

    // **投稿ごとの操作も出さない** (自分のものか判定できないため)。
    await expect(page.getByRole('button', { name: '通報する' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: '削除' })).toHaveCount(0);

    // **「確認しています...」で固めない。** 待っても変わらないので、
    // やり直せることを出す。
    await expect(page.getByRole('button', { name: 'もう一度確認する' })).toBeVisible();

    // **投稿そのものは塞がない。** Cookie があれば通るので、
    // /me の一時的な失敗で投稿できなくなるほうが害が大きい。
    await expect(page.getByRole('button', { name: '投稿する' })).toBeVisible();
  });

  test('「もう一度確認する」で書きかけのコメントが消えない', async ({ page }) => {
    // **フォームを 2 つ置くと、ここで消えます** (レビュー指摘)。
    // 不明時と確定時で別の要素にすると、React の位置ベースの照合で
    // 別物として作り直され、body の state が捨てられます ——
    // 「もう一度確認する」はフォームのすぐ上にあるので、
    // 長文を書いてから押した利用者がちょうどそれを踏みます。
    let failMe = true;
    await page.route('**/me', (route) =>
      failMe ? fail(route, 500, 'INTERNAL', 'サーバ内部でエラー') : ok(route, plainUser),
    );
    await mockThreadOk(page);
    await mockComments(page, (route) => ok(route, comments([])));

    await page.goto(url);
    await expect(page.getByText('ログイン状態を確認できませんでした。')).toBeVisible();

    const draft = 'ここまで書いたところで確認を押した';
    await page.getByLabel('コメント').fill(draft);

    failMe = false;
    await page.getByRole('button', { name: 'もう一度確認する' }).click();

    // **`ready` になったことを先に待ちます** (レビュー指摘)。
    //
    // reload は同期で `loading` に戻すので、クリック直後は警告が消えます ——
    // そこで値を見ると、**再取得が起きなくても緑になります**
    // (実測: invalidateMe を外す変異が通り抜けた)。
    // ログイン中にしか出ない文言を目印にして、確定を観測してから確かめます。
    await expect(page.getByText('ログイン中の表示名で投稿されます。')).toBeVisible();
    await expect(page.getByText('ログイン状態を確認できませんでした。')).toHaveCount(0);
    // **本文は残る。**
    await expect(page.getByLabel('コメント')).toHaveValue(draft);
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
