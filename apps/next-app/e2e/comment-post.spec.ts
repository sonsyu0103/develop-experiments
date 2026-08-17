// コメント投稿の冪等キー (ADR 0015)。
//
// **ここは型検査でもビルドでも触れない領域**になります。
// キーは HTTP ヘッダとして出ていくだけで、画面には現れません。
// つまり**壊れても画面は正常に見え、症状は「二重投稿が増えた」という
// 形でしか出ない**ので、要求そのものを見て確かめます。
//
// 仕様の要点は 2 つです。
//
//   - 同じキーで再送すると、前回の結果がそのまま返る (処理は 1 回だけ)
//   - **同じキーで別の内容を送ると 422。** 黙って前回の結果を返すと、
//     クライアントのバグが見えなくなるため
//
// したがって画面側の正しさは「**内容が同じあいだキーを変えず、
// 内容を変えたらキーを変える**」ことになります。
import { expect, test, type Page } from '@playwright/test';

import {
  comment,
  comments,
  created,
  fail,
  mockMe,
  mockThread,
  ok,
  plainUser,
  thread,
} from './api-mock';

const url = '/threads/100';

/** 送られた `Idempotency-Key` を順に集めます。 */
type Sent = string[];

/**
 * コメントの取得と投稿をまとめて差し替えます。
 *
 * `outcome` は投稿の結果です。`'conflict'` は 409 —— 同時実行の競合で、
 * **再試行できる**種類の失敗になります。
 */
async function mockCommentApi(
  page: Page,
  outcome: 'created' | 'conflict',
  sent: Sent,
): Promise<void> {
  await page.route(/\/threads\/\d+\/comments(\?|$)/, (route) => {
    if (route.request().method() !== 'POST') {
      return ok(route, comments([]));
    }

    // **ヘッダ名は大文字小文字を区別しません。** Playwright は
    // 小文字にそろえて返します。
    sent.push(route.request().headers()['idempotency-key'] ?? '(なし)');

    if (outcome === 'conflict') {
      return fail(route, 409, 'CONFLICT', '直列化に失敗しました');
    }
    const body = JSON.parse(route.request().postData() ?? '{}') as { body?: string };
    return created(route, comment({ id: 1, seq: 1, body: body.body ?? '' }));
  });
}

async function open(page: Page): Promise<void> {
  await mockThread(page, (route) => ok(route, thread()));
  await page.goto(url);
  await expect(page.getByRole('button', { name: '投稿する' })).toBeVisible();
}

test.describe('コメントの投稿', () => {
  test('同じ内容で送り直すと、冪等キーは変わらない', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, plainUser);
    await mockCommentApi(page, 'conflict', sent);
    await open(page);

    await page.getByLabel('コメント').fill('ふぁ〜、眠いよ〜');
    await page.getByRole('button', { name: '投稿する' }).click();
    await expect(page.getByText('同時に投稿が重なりました')).toBeVisible();

    // **そのまま押し直す。** 「タイムアウトしたが実は成功していた」場合、
    // ここでキーが変わると 2 件目が作られます —— それを防ぐのが
    // ボタンの無効化ではなくキーであること、がこの検査の主題になります。
    await page.getByRole('button', { name: '投稿する' }).click();
    await expect.poll(() => sent.length).toBe(2);

    expect(sent[0]).not.toBe('(なし)');
    expect(sent[1]).toBe(sent[0]);
  });

  test('内容を書き換えて送り直すと、冪等キーは変わる', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, plainUser);
    await mockCommentApi(page, 'conflict', sent);
    await open(page);

    await page.getByLabel('コメント').fill('最初の本文');
    await page.getByRole('button', { name: '投稿する' }).click();
    await expect.poll(() => sent.length).toBe(1);

    // **別の投稿になったので、キーも別でなければなりません。**
    // 使い回すと API は 422 を返します (前回と違う内容が来た、という申告)。
    await page.getByLabel('コメント').fill('書き直した本文');
    await page.getByRole('button', { name: '投稿する' }).click();
    await expect.poll(() => sent.length).toBe(2);

    expect(sent[1]).not.toBe(sent[0]);
  });

  test('409 は「時間をおいて」とは案内しない', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, plainUser);
    await mockCommentApi(page, 'conflict', sent);
    await open(page);

    await page.getByLabel('コメント').fill('競合する本文');
    await page.getByRole('button', { name: '投稿する' }).click();

    // 409 は同時実行の競合で、**押せば通る**種類の失敗になります
    // (レス番号の直列化失敗。ADR 0019)。諦めさせる案内にしません。
    await expect(page.getByText('もう一度「投稿する」を押してください')).toBeVisible();
    await expect(page.getByText('時間をおいて')).toHaveCount(0);
    await expect(page.getByRole('button', { name: '投稿する' })).toBeEnabled();
  });

  test('未ログインの 409 では「二重には投稿されません」と言わない', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, null);
    await mockCommentApi(page, 'conflict', sent);
    await open(page);

    await page.getByLabel('コメント').fill('匿名で競合する本文');
    await page.getByRole('button', { name: '投稿する' }).click();

    // **冪等キーは未ログインでは無視されます** (ADR 0015 決定 4)。
    // 匿名にはキーの名前空間を分ける手段が無く、IP で分けると NAT の背後で
    // 他人と衝突するため。つまり**匿名の押し直しは本当に 2 件目を作りうる。**
    // ログイン中と同じ文言を出すと、そこを嘘で埋めることになります。
    await expect(page.getByText('二重には投稿されません')).toHaveCount(0);
    await expect(page.getByText('押し直す前に一覧を確認してください')).toBeVisible();
  });

  test('アップロードに失敗しても、添付済みの画像は外さない', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, plainUser);
    await mockCommentApi(page, 'created', sent);

    let uploads = 0;
    await page.route('**/images', (route) => {
      uploads += 1;
      return uploads === 1
        ? created(route, {
            id: '018f2c00-0000-7000-8000-000000000001',
            url: '/images/ok.png',
            width: 800,
            height: 600,
          })
        : fail(route, 500, 'INTERNAL', '想定外のエラー');
    });
    // プレビューの読み込み先。中身は見ないので 1x1 でよい。
    await page.route('**/images/ok.png', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: '' }),
    );

    await open(page);

    const file = { name: 'a.png', mimeType: 'image/png', buffer: Buffer.from('dummy') };
    await page.getByLabel('画像を添付').setInputFiles(file);
    await expect(page.getByText('添付しました (800×600)')).toBeVisible();

    // 2 枚目の選択が失敗しても、**下書きから 1 枚目が消えてはいけません。**
    // 消えると、文言はアップロードの話しかしないので気づけません。
    await page.getByLabel('画像を添付').setInputFiles(file);
    await expect(page.getByText('アップロードできませんでした')).toBeVisible();
    await expect(page.getByText('添付しました (800×600)')).toBeVisible();
  });

  test('投稿すると一覧の先頭に出て、入力欄が空になる', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, plainUser);
    await mockCommentApi(page, 'created', sent);
    await open(page);

    await page.getByLabel('コメント').fill('投稿できた本文');
    await page.getByRole('button', { name: '投稿する' }).click();

    await expect(page.getByText('投稿できた本文')).toBeVisible();
    // **空にしないと、同じ本文をもう一度押せてしまいます。**
    await expect(page.getByLabel('コメント')).toHaveValue('');
  });

  test('未ログインでは、名前欄を出して画像は使えないと伝える', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, null);
    await mockCommentApi(page, 'created', sent);
    await open(page);

    // 匿名投稿の表示名。**ログイン中は送っても無視されます** (ADR 0014)。
    await expect(page.getByLabel('名前 (任意)')).toBeVisible();
    // **画像の投稿にはログインが要ります** (ADR 0007)。押せない欄だけを
    // 見せると、壊れているのか権限が無いのかが分かりません。
    await expect(page.getByText('画像を使うにはログインが必要です')).toBeVisible();
    // 匿名では自分で消せないことも、投稿する前に伝えます。
    await expect(page.getByText('匿名の投稿は、あとから自分で削除できません')).toBeVisible();
  });

  test('ログイン中は匿名の名前欄を出さない', async ({ page }) => {
    const sent: Sent = [];
    await mockMe(page, plainUser);
    await mockCommentApi(page, 'created', sent);
    await open(page);

    // 送っても無視される欄を見せると、変えられると誤解させます。
    await expect(page.getByLabel('名前 (任意)')).toHaveCount(0);
    await expect(page.getByText('ログイン中の表示名で投稿されます')).toBeVisible();
  });
});
