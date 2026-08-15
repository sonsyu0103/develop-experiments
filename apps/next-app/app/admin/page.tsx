'use client';

// 通報キューの画面。**moderator 以上**が開けます。
//
// 【この画面はサーバから利用者ごとの内容を返しません】
// 取得はすべてブラウザから行います (app/lib/api.ts を参照)。
// そのため、サーバが返す HTML と RSC ペイロードは誰に対しても同じで、
// ADR 0005 の「認証済みの応答をキャッシュに載せない」で
// 確認すべき層が **fetch 1 か所だけ**に減ります。
// ルートを動的にする必要も、CDN でこのパスを避ける必要もありません。
import { AdminGate } from './AdminGate';
import { ImageDeleteForm } from './ImageDeleteForm';
import { ReportQueue } from './ReportQueue';
import { colors } from '../lib/ui';

export default function AdminPage() {
  return (
    <AdminGate title="通報キュー" require="moderator">
      {() => (
        <>
          <ReportQueue />
          <h2 style={{ color: colors.dim, marginTop: '2.5rem' }}>画像の削除</h2>
          <p style={{ color: colors.dim }}>
            投稿を消しても画像は残ります。通報は画像を対象にできないため、ここから消します。
          </p>
          <ImageDeleteForm />
        </>
      )}
    </AdminGate>
  );
}
