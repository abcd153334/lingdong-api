-- 账务指纹：恢复后必须逐项一致，否则这份备份不可用。
-- 唯一来源：backup.sh / restore.sh 都用这一份；PowerShell 版历史上内联过同样的 SQL，
-- 以后改口径只改这里（否则两套脚本会给出不同的"一致"结论）。
-- COALESCE 是必须的：空库返回 NULL，而 NULL 与 0 在字符串比较里不相等。
SELECT
  (SELECT count(*) FROM users WHERE deleted_at IS NULL)                    AS users,
  (SELECT COALESCE(sum(balance),0) FROM users)                             AS balance_micro,
  (SELECT COALESCE(sum(frozen),0) FROM users)                              AS frozen_micro,
  (SELECT COALESCE(sum(used),0) FROM users)                                AS used_micro,
  (SELECT count(*) FROM transactions)                                      AS txns,
  (SELECT COALESCE(sum(amount),0) FROM transactions)                       AS txn_amount_micro,
  (SELECT count(*) FROM usages)                                            AS usages,
  (SELECT COALESCE(sum(charge),0) FROM usages)                             AS charge_micro,
  (SELECT count(*) FROM orders WHERE status='paid')                        AS paid_orders;
