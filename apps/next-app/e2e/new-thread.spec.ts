// スレッドを立てる画面。
//
// **ログイン状態が分からないときの扱いが本題**になります。
// この画面にログインの導線は 1 つしか無く (ヘッダには無い)、
// 「決まっていない」を「未ログイン」や「確認中」に丸めると、
// **/me が落ちている間ログインできなくなります。**
//
// PR #54 でその後退を実際に出荷しかけました。**この画面の検査が
// 1 本も無かった**のが理由なので、対になる形で置きます (レビュー指摘)。
import { expect, test } from '@playwright/test';

import { delay, fail, mockMe, ok, plainUser } from './api-mock';

const url = '/threads/new';

test('ログイン中なら画像欄が使える', async ({ page }) => {
  await mockMe(page, plainUser);
  await page.goto(url);

  await expect(page.getByLabel('アイコン (任意)')).toBeVisible();
  await expect(page.getByText('画像を使うにはログインが必要です。')).toHaveCount(0);
});

test('未ログインなら理由とログイン導線を出す', async ({ page }) => {
  await mockMe(page, null);
  await page.goto(url);

  await expect(page.getByText('画像を使うにはログインが必要です。')).toBeVisible();
  await expect(page.getByRole('link', { name: 'Google でログインする' })).toBeVisible();
});

test('決まるまでは「ログインが必要」と言い切らない', async ({ page }) => {
  // `/me` を遅らせて、決まる前の表示を見ます。
  await page.route('**/me', async (route) => {
    await delay(1500);
    return ok(route, plainUser);
  });
  await page.goto(url);

  // **言い切らない。** ログイン中の利用者にも一瞬「使えません」と出て、
  // 応答が返った瞬間に入れ替わるのを避けます。
  await expect(page.getByText('ログイン状態を確認しています...')).toBeVisible();
  await expect(page.getByText('画像を使うにはログインが必要です。')).toHaveCount(0);

  // 決まったら画像欄が使える。
  await expect(page.getByLabel('アイコン (任意)')).toBeVisible();
});

test('ログイン状態が分からないとき、ログイン導線を消さない', async ({ page }) => {
  // **401 ではなく 500。** me.ts は 401 だけを未ログインに翻訳します。
  await page.route('**/me', (route) => fail(route, 500, 'INTERNAL', 'サーバ内部でエラー'));
  await page.goto(url);

  // **待っても変わらないことを出す。**「確認しています...」で固めない。
  await expect(page.getByText('ログイン状態を確認できませんでした。')).toBeVisible();
  await expect(page.getByRole('button', { name: 'もう一度確認する' })).toBeVisible();
  await expect(page.getByText('ログイン状態を確認しています...')).toHaveCount(0);

  // **ログインの導線が残ること。** ここが唯一の入口なので、
  // 消すと /me が落ちている間ログインできなくなります。
  await expect(page.getByRole('link', { name: 'Google でログインする' })).toBeVisible();

  // 画像は使えないが、「ログインが必要」とは断定しない。
  await expect(page.getByText('ログイン状態が分からないため、画像は使えません。')).toBeVisible();
});
