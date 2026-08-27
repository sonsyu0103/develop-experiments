// 問い合わせフォーム (docs/adr/0008-contact-and-mail.md)。
//
// **ここで見るのは文言と状態遷移です** (ADR 0020)。
// 送信そのものが正しいかは Go 側の検査が持っています。画面側にしか無く、
// かつ壊れても気づきにくいのは次の 3 つになります。
//
//   - `202` を「送信しました」と言い換えていないこと (決定 1)。
//     言い換えると、送れなかった場合に利用者は成功したと思ったままになる
//   - honeypot が画面に見えていないこと (決定 4)。
//     見えていたら人間が埋め、その問い合わせは黙って捨てられる
//   - 429 の案内が「入力の誤り」に見えないこと (ADR 0013 決定 3)
import { expect, test } from '@playwright/test';

import { fail, mockMe, moderator } from './api-mock';

/** `POST /contact` を差し替えます。 */
async function mockContact(page: import('@playwright/test').Page, status: 'accepted' | 'rate-limited' | 'invalid') {
  await page.route('**/contact', (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    switch (status) {
      case 'accepted':
        return route.fulfill({
          status: 202,
          contentType: 'application/json',
          body: JSON.stringify({ status: 'accepted', receivedAt: '2026-08-17T12:00:00Z' }),
        });
      case 'rate-limited':
        return fail(route, 429, 'RESOURCE_EXHAUSTED', '問い合わせの送信が多すぎます');
      case 'invalid':
        return fail(route, 400, 'INVALID_ARGUMENT', '本文を入力してください');
    }
  });
}

/** フォームを最低限埋めます。 */
async function fillForm(page: import('@playwright/test').Page) {
  await page.getByLabel('お名前').fill('ホシノ');
  await page.getByLabel('メールアドレス').fill('hoshino@example.com');
  await page.getByLabel('件名').fill('ログインできません');
  await page.getByLabel('お問い合わせ内容').fill('画面が戻ってきます。');
}

test.describe('問い合わせフォーム', () => {
  test('未ログインでも開けて送れる', async ({ page }) => {
    // **401 が既定の状態です** (決定 4)。
    // 「ログインできない」という問い合わせが来る以上、
    // ここでログイン画面へ飛ばすと詰みます。
    await mockMe(page, null);
    await mockContact(page, 'accepted');
    await page.goto('/contact');

    await fillForm(page);
    await page.getByRole('button', { name: '送信する' }).click();

    await expect(page.getByText('問い合わせを受け付けました。')).toBeVisible();
  });

  test('「送信しました」とは言わない', async ({ page }) => {
    await mockMe(page, null);
    await mockContact(page, 'accepted');
    await page.goto('/contact');

    await fillForm(page);
    await page.getByRole('button', { name: '送信する' }).click();

    // **202 は受理までしか終わっていません。**
    // メールは定期処理が後から送るので、ここで完了を名乗ってはいけません。
    await expect(page.getByText('送信しました')).toHaveCount(0);
    // 未ログインなので控えも届きません (ADR 0008 決定 2)。
    await expect(page.getByText('控えのメールは届きません')).toBeVisible();
  });

  // **控えの届き先が、入力欄の値ではないと分かること** (ADR 0008 決定 2)。
  //
  // 入力欄は書き換えられるので、そこに書かれたアドレスを
  // 「控えはここへ届きます」と案内すると**嘘の案内**になります。
  // 画面が示すのは常にアカウントに登録されているアドレスです。
  test('ログイン済みなら控えの届き先が登録アドレスだと分かる', async ({ page }) => {
    await mockMe(page, moderator);
    await mockContact(page, 'accepted');
    await page.goto('/contact');

    await fillForm(page);
    // **入力欄を他人のアドレスへ書き換えても、案内は登録アドレスのまま。**
    // ここが入力欄に追従したら、決定 2 が画面の上で破れています。
    await page.getByLabel('メールアドレス').fill('someone-else@example.com');
    await expect(
      page.getByText(`控えは登録アドレス (${moderator.email}) 宛にお送りします。`),
    ).toBeVisible();

    await page.getByRole('button', { name: '送信する' }).click();

    // 受理後の案内も登録アドレス。入力した他人のアドレスは出てこない。
    await expect(page.getByText(moderator.email)).toBeVisible();
    await expect(page.getByText('someone-else@example.com')).toHaveCount(0);
  });

  test('ログイン済みなら氏名とアドレスが埋まる', async ({ page }) => {
    await mockMe(page, moderator);
    await mockContact(page, 'accepted');
    await page.goto('/contact');

    await expect(page.getByLabel('お名前')).toHaveValue(moderator.displayName);
    await expect(page.getByLabel('メールアドレス')).toHaveValue(moderator.email);
  });

  test('honeypot は画面に出ない', async ({ page }) => {
    await mockMe(page, null);
    await page.goto('/contact');

    // **見えていたら人間が埋めます。** 埋まった問い合わせは
    // API 側で黙って捨てられるので、気づく手立てがありません。
    await expect(page.locator('#contact-website')).toBeHidden();
  });

  test('429 は入力の誤りとして案内しない', async ({ page }) => {
    await mockMe(page, null);
    await mockContact(page, 'rate-limited');
    await page.goto('/contact');

    await fillForm(page);
    await page.getByRole('button', { name: '送信する' }).click();

    await expect(page.getByText('短時間に送りすぎています')).toBeVisible();
    // 入力を直せば通ると読めると、利用者は文面を書き直し続けることになります。
    await expect(page.getByText('入力を確認してください')).toHaveCount(0);
  });

  test('400 は入力の誤りとして案内する', async ({ page }) => {
    await mockMe(page, null);
    await mockContact(page, 'invalid');
    await page.goto('/contact');

    await fillForm(page);
    await page.getByRole('button', { name: '送信する' }).click();

    await expect(page.getByText('入力を確認してください')).toBeVisible();
  });

  test('必須が埋まるまで送信できない', async ({ page }) => {
    await mockMe(page, null);
    await page.goto('/contact');

    const submit = page.getByRole('button', { name: '送信する' });
    await expect(submit).toBeDisabled();

    await fillForm(page);
    await expect(submit).toBeEnabled();
  });
});
