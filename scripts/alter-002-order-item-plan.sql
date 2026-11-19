-- alter-002: market.mall_order_items 补 ref_plan_id 列（R2.5-T2 plan_id 快照裁定）。
--
-- 背景：subscribe 行履约需要 plan_id——下单时快照进 mall_order_items（裁定：
-- 审计完整 + 退款时可用；general/recharge 行填 0）。worker subscribeLine 按
-- (order_no, line_no) 读行快照调 billing.ActivateSubscription。
-- 权威定义：docs/spec/db-schema.sql mall_order_items 表（新建库走
-- schema-apply.sh 即带此列；存量库跑本脚本补列）。
--
-- 幂等性：MySQL 8 的 ALTER TABLE ADD COLUMN 无 IF NOT EXISTS，重复执行报
-- Duplicate column name 1060。本脚本用 information_schema.columns 判存——
-- 已存在则跳过，可重复执行。
--
-- 用法（宿主执行，凭据从 deploy/compose/.env 读，不落 argv；与 alter-001 同姿势）：
--   PW="$(sed -n 's/^MYSQL_ROOT_PASSWORD=//p' deploy/compose/.env)"
--   docker compose -f deploy/compose/docker-compose.yml exec -T \
--     -e MYSQL_PWD="$PW" mysql mysql -uroot --default-character-set=utf8mb4 < scripts/alter-002-order-item-plan.sql

USE `market`;

DELIMITER $$

DROP PROCEDURE IF EXISTS alter_002_add_item_plan_column$$
CREATE PROCEDURE alter_002_add_item_plan_column()
BEGIN
  DECLARE col_count INT;
  SELECT COUNT(*) INTO col_count
    FROM information_schema.columns
   WHERE table_schema = 'market'
     AND table_name   = 'mall_order_items'
     AND column_name  = 'ref_plan_id';

  IF col_count = 0 THEN
    ALTER TABLE `mall_order_items`
      ADD COLUMN `ref_plan_id` BIGINT UNSIGNED NOT NULL DEFAULT 0
      COMMENT '订阅行下单时快照 billing plan id（general/recharge 行=0）'
      AFTER `bonus_cent`;
    SELECT 'alter-002: mall_order_items.ref_plan_id added' AS result;
  ELSE
    SELECT 'alter-002: mall_order_items.ref_plan_id already exists, skipped' AS result;
  END IF;
END$$

DELIMITER ;

CALL alter_002_add_item_plan_column();
DROP PROCEDURE alter_002_add_item_plan_column;
