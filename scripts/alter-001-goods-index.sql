-- alter-001: goods 表补在售键集分页索引 idx_on_sale_created (on_sale, created_at, id)。
--
-- 背景：ListGoods 按 WHERE on_sale=1 ORDER BY created_at DESC, id DESC 做键集分页，
-- 既有 idx_category/idx_type_sale 均推不动该排序；本索引让无品类（category_id=0）
-- 与带品类过滤两类查询都能走索引序扫描（medium 期审计项）。
-- 权威定义：docs/spec/db-schema.sql goods 表（新建库走 schema-apply.sh 即带此索引）。
--
-- 幂等性：MySQL 8 的 ALTER TABLE ADD INDEX 无 IF NOT EXISTS，重复执行报
-- Duplicate key name 1061。本脚本用 information_schema.statistics 判存——
-- 已存在则跳过，可重复执行；对"建到一半"的半成品索引（理论不会出现，
-- ALTER 原子）同样按存在处理。
--
-- 用法（宿主执行，凭据从 deploy/compose/.env 读，不落 argv；与 schema-apply.sh 同姿势）：
--   PW="$(sed -n 's/^MYSQL_ROOT_PASSWORD=//p' deploy/compose/.env)"
--   docker compose -f deploy/compose/docker-compose.yml exec -T \
--     -e MYSQL_PWD="$PW" mysql mysql -uroot --default-character-set=utf8mb4 < scripts/alter-001-goods-index.sql

USE `market`;

DELIMITER $$

DROP PROCEDURE IF EXISTS alter_001_add_goods_index$$
CREATE PROCEDURE alter_001_add_goods_index()
BEGIN
  DECLARE idx_count INT;
  SELECT COUNT(*) INTO idx_count
    FROM information_schema.statistics
   WHERE table_schema = 'market'
     AND table_name   = 'goods'
     AND index_name   = 'idx_on_sale_created';

  IF idx_count = 0 THEN
    ALTER TABLE `goods`
      ADD INDEX `idx_on_sale_created` (`on_sale`, `created_at`, `id`)
      COMMENT '在售键集分页 ORDER BY 下推';
    SELECT 'alter-001: idx_on_sale_created added' AS result;
  ELSE
    SELECT 'alter-001: idx_on_sale_created already exists, skipped' AS result;
  END IF;
END$$

DELIMITER ;

CALL alter_001_add_goods_index();
DROP PROCEDURE alter_001_add_goods_index;
