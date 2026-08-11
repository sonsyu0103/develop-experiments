-- 列を落とせば、CHECK 制約も部分索引も追随して消える。
-- 明示的に DROP する必要はない (000002 の down と同じ考え方)。
--
-- **戻すと権限の割り当てが消える。** 誰が管理者だったかは復元できない。
-- 監査記録 (moderation_actions) を足したあとは、そちらに履歴が残る。
ALTER TABLE users DROP COLUMN IF EXISTS role;
