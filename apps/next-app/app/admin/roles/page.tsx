'use client';

// ロール変更の画面。**admin だけ**が開けます (ADR 0011 決定 1)。
//
// 3 段階に分けているのは、**投稿を消せる権限と、権限を配れる権限を
// 分離する**ためです。モデレーターを増やしても、権限を配れる人間は増えません。
import { useState } from 'react';

import { AdminGate } from '../AdminGate';
import { describe } from '../AdminGate';
import { ApiError, changeUserRole, type Me, type Role } from '../../lib/api';
import { button, card, colors, formatTime, input, label } from '../../lib/ui';

const roles: Role[] = ['user', 'moderator', 'admin'];

export default function RolesPage() {
  return (
    <AdminGate title="ロールの変更" require="admin">
      {(me) => <RoleForm me={me} />}
    </AdminGate>
  );
}

function RoleForm({ me }: { me: Me }) {
  const [publicID, setPublicID] = useState('');
  const [role, setRole] = useState<Role>('moderator');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<string | null>(null);

  // **自分自身は対象にできません。** 手前で気づけるように、
  // 送る前に伝えます (サーバも 403 で弾きます)。
  const isSelf = publicID.trim() === me.publicId;

  async function onSubmit() {
    setBusy(true);
    setResult(null);
    try {
      const recorded = await changeUserRole(publicID.trim(), {
        role,
        ...(reason.trim() ? { reason: reason.trim() } : {}),
      });
      // **返ってくる targetId は公開 ID です。**
      // `moderation_actions` に記録されるのは内部 ID ですが、
      // API は公開 ID へ詰め替えます —— 記録と応答で同じ列が
      // 違う値になるので、混同しないこと (ADR 0011 の 8)。
      setResult(
        `${recorded.targetId} を ${role} にしました ` +
          `(記録 #${recorded.id} / ${formatTime(recorded.createdAt)})`,
      );
      setReason('');
    } catch (e) {
      setResult(explain(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={card}>
      <p style={{ color: colors.dim, marginTop: 0 }}>
        受け付けるのは公開 ID (UUID) だけです。内部 ID は API に渡せません。
      </p>

      <label style={label} htmlFor="public-id">
        対象の公開 ID
      </label>
      <input
        id="public-id"
        style={{ ...input, width: '100%', maxWidth: '32rem' }}
        value={publicID}
        placeholder="0192f0a0-0000-7000-8000-000000000001"
        onChange={(e) => setPublicID(e.target.value)}
      />
      {isSelf && (
        <p style={{ color: colors.danger, margin: '0.3rem 0' }}>
          自分のロールは変更できません。最後の admin が自分を降格させると、
          誰もロールを配れなくなります。
        </p>
      )}

      <label style={label} htmlFor="role">
        新しいロール
      </label>
      <select
        id="role"
        style={input}
        value={role}
        onChange={(e) => setRole(e.target.value as Role)}
      >
        {roles.map((r) => (
          <option key={r} value={r}>
            {r}
          </option>
        ))}
      </select>

      <label style={label} htmlFor="role-reason">
        変更の理由 (任意・監査記録に残る)
      </label>
      <input
        id="role-reason"
        style={{ ...input, width: '100%', maxWidth: '32rem' }}
        value={reason}
        maxLength={500}
        onChange={(e) => setReason(e.target.value)}
      />

      <p style={{ margin: '0.8rem 0 0' }}>
        <button
          type="button"
          style={button}
          disabled={busy || publicID.trim() === '' || isSelf}
          onClick={() => void onSubmit()}
        >
          変更する
        </button>
      </p>

      {result !== null && <p style={{ color: colors.warn, margin: '0.5rem 0 0' }}>{result}</p>}
    </div>
  );
}

/**
 * 失敗の理由を、**そのまま次の操作に繋がる文言**にします。
 *
 * `message` は人間向けで予告なく変わるため、分岐は `code` で行います
 * (仕様書の `Error` スキーマ)。
 */
function explain(e: unknown): string {
  if (!(e instanceof ApiError)) return describe(e);

  switch (e.code) {
    case 'FAILED_PRECONDITION':
      // **422。** 入力の形は正しく、現在の状態と噛み合わないだけです。
      // 再試行しても解決しないので、次にやることを書きます。
      return '最後の admin は降格させられません。先に別の利用者を admin にしてください。';
    case 'NOT_FOUND':
      return '利用者が見つかりません (公開 ID が違うか、退会しています)。';
    case 'PERMISSION_DENIED':
      return '権限がありません (admin ではないか、自分自身を対象にしています)。';
    default:
      return describe(e);
  }
}
