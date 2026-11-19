-- R2.5-T2: billing 月卡套餐种子（scripts/seed-plans.sql）。
--
-- 前置：ai_gateway 库 plans 表就绪（services/ai-gateway/scripts/schema.sql 已建）。
-- 应用（宿主执行；凭据从 deploy/compose/.env 读，不硬编码、不落 argv）：
--   PW="$(sed -n 's/^MYSQL_ROOT_PASSWORD=//p' deploy/compose/.env)"
--   docker compose -f deploy/compose/docker-compose.yml exec -T \
--     -e MYSQL_PWD="$PW" mysql \
--     mysql -uroot --default-character-set=utf8mb4 < scripts/seed-plans.sql
--
-- 必须先于 scripts/seed-goods.sql 执行：goods 301 的 ref_plan_id 按套餐名解析
-- （seed-goods.sql 内 SET @r25_plan_id = (SELECT id ...)），套餐缺失会导致
-- 商品种子写入 NULL 撞 NOT NULL 直接报错（依赖显式化，不静默落 0）。
--
-- 列值口径：与 services/ai-gateway/scripts/seed/main.go::seedM7 的「月度·标准」完全一致
-- （月费 ¥80 / 30 天 / 每 5h 滚动刷新 / 轻量档 200 万 token）。按 name upsert
-- （uk_name 幂等，ODKU 更新列值不换 id）：seedM7 先跑/后跑都收敛到同一行，
-- goods 301 的 ref_plan_id 按名解析始终有效，无 id 硬编码。
--
-- 状态：status=0（在售）、plan_type='recurring'——billing ActivateSubscription
-- 的「模拟月费校验」以这三项为准（不接真钱）。
--
-- 文件末尾附自检 SELECT（套餐在售/定期/周期 30 天），输出 PASS/FAIL。

USE `ai_gateway`;

INSERT INTO plans
  (name, plan_type, price_money, validity_days, refresh_hours,
   window_quota_cent, week_quota_cent, cycle_quota_cent, allowed_models,
   tier_quota, status)
VALUES
  ('月度·标准', 'recurring', 80, 30, 5,
   0, 0, 0, NULL,
   '{"轻量":2000000,"文本":1000000,"长文本":600000,"视觉":300000,"推理":500000,"旗舰":200000}',
   0)
AS new
ON DUPLICATE KEY UPDATE
  plan_type     = new.plan_type,
  price_money   = new.price_money,
  validity_days = new.validity_days,
  refresh_hours = new.refresh_hours,
  tier_quota    = new.tier_quota,
  status        = new.status;

-- ==================== 自检（PASS/FAIL 逐项输出） ====================
SELECT
  CASE WHEN COUNT(*) = 1 THEN 'PASS' ELSE 'FAIL' END AS chk_plan_exists,
  CASE WHEN MIN(plan_type) = 'recurring' THEN 'PASS' ELSE 'FAIL' END AS chk_plan_recurring,
  CASE WHEN MIN(status) = 0 THEN 'PASS' ELSE 'FAIL' END AS chk_plan_onsale,
  CASE WHEN MIN(validity_days) = 30 THEN 'PASS' ELSE 'FAIL' END AS chk_plan_30d,
  CASE WHEN MIN(price_money) = 80 THEN 'PASS' ELSE 'FAIL' END AS chk_plan_80yuan,
  id, name, price_money, validity_days, refresh_hours, status
FROM plans WHERE name = '月度·标准'
GROUP BY id, name, price_money, validity_days, refresh_hours, status;
