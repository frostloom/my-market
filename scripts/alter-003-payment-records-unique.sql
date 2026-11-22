-- alter-003: market.payment_records 补回调幂等组合唯一键（2026-11-22，规格对齐）。
--
-- 背景：表注释自称「Provider 回调幂等依据：order_no+provider_trade_no」，但 DDL
-- 只有普通索引——幂等完全依赖「写入方先 FOR UPDATE 锁订单行」的代码纪律
-- （2026-11-21 审查 M1）。本脚本把组合键落为 UNIQUE（DB 兜底与规格一致），
-- 同时以 uk 前缀替代原 idx_order_no（按单查询走唯一键左前缀，不再重复建索引）。
-- 权威定义：docs/spec/db-schema.sql payment_records 表（新建库走 schema-apply.sh
-- 即带此键；存量库跑本脚本补齐）。
--
-- 幂等性：重复执行用 information_schema.statistics 判存——已存在则跳过；
-- idx_order_no 的删除同样判存。存在重复组合行时拒绝执行并报数量
-- （需人工去重后重跑，不自动合并资金语义数据）。
--
-- 用法（宿主执行，凭据从 deploy/compose/.env 读，不落 argv；与 alter-001/002 同姿势）：
--   PW="$(sed -n 's/^MYSQL_ROOT_PASSWORD=//p' deploy/compose/.env)"
--   docker compose -f deploy/compose/docker-compose.yml exec -T \
--     -e MYSQL_PWD="$PW" mysql mysql -uroot --default-character-set=utf8mb4 < scripts/alter-003-payment-records-unique.sql

USE `market`;

DELIMITER $$

DROP PROCEDURE IF EXISTS alter_003_payment_records_unique$$
CREATE PROCEDURE alter_003_payment_records_unique()
BEGIN
  DECLARE dup_count INT;
  DECLARE uk_count INT;
  DECLARE idx_count INT;

  -- 前置：组合键重复行 → 拒绝（自动去重会静默改写资金语义，必须人工裁定）。
  SELECT COUNT(*) INTO dup_count FROM (
    SELECT order_no, provider, provider_trade_no
      FROM payment_records
     GROUP BY order_no, provider, provider_trade_no
    HAVING COUNT(*) > 1
  ) d;

  IF dup_count > 0 THEN
    SIGNAL SQLSTATE '45000' SET
      MESSAGE_TEXT = 'alter-003: duplicate (order_no, provider, provider_trade_no) rows exist; dedupe manually first';
  END IF;

  SELECT COUNT(*) INTO uk_count
    FROM information_schema.statistics
   WHERE table_schema = 'market'
     AND table_name   = 'payment_records'
     AND index_name   = 'uk_order_provider_trade';

  IF uk_count = 0 THEN
    ALTER TABLE `payment_records`
      ADD UNIQUE KEY `uk_order_provider_trade` (`order_no`, `provider`, `provider_trade_no`);
    SELECT 'alter-003: payment_records.uk_order_provider_trade added' AS result;
  ELSE
    SELECT 'alter-003: uk_order_provider_trade already exists, skipped' AS result;
  END IF;

  -- 原 idx_order_no 与唯一键左前缀重复，判存后删除。
  SELECT COUNT(*) INTO idx_count
    FROM information_schema.statistics
   WHERE table_schema = 'market'
     AND table_name   = 'payment_records'
     AND index_name   = 'idx_order_no';

  IF idx_count > 0 THEN
    ALTER TABLE `payment_records` DROP INDEX `idx_order_no`;
    SELECT 'alter-003: redundant idx_order_no dropped' AS result;
  ELSE
    SELECT 'alter-003: idx_order_no already absent, skipped' AS result;
  END IF;
END$$

DELIMITER ;

CALL alter_003_payment_records_unique();
DROP PROCEDURE IF EXISTS alter_003_payment_records_unique;
