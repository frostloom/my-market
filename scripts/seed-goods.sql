-- R1-T1: market 商品种子（scripts/seed-goods.sql）。
--
-- 前置：bash scripts/schema-apply.sh 已执行（market 库 9 张表 + 六品类就绪）；
-- **必须先跑 scripts/seed-plans.sql**（billing ai_gateway.plans 套餐）——
-- goods 301 的 ref_plan_id 按套餐名「月度·标准」解析，套餐缺失则本脚本
-- 写入 NULL 撞 NOT NULL 报错（依赖显式化，不静默落 0）。
-- 应用（宿主执行；凭据从 deploy/compose/.env 读，不硬编码、不落 argv）：
--   docker compose -f deploy/compose/docker-compose.yml exec -T \
--     -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql \
--     mysql -uroot --default-character-set=utf8mb4 market < scripts/seed-goods.sql
--
-- 幂等：goods 表无业务唯一键，这里用**显式主键 id** + INSERT ... AS new
-- ON DUPLICATE KEY UPDATE（MySQL 8.0.19+），重复执行 = 原位更新，不产生重复行。
--   id 段位：101-140 general（手机/服饰/穿搭/家居 各 10）
--            201-203 recharge（¥10/¥50/¥100 面值，bonus 0/300/1000 分）
--            301 subscription 会员月卡（R2.5：上架 on_sale=1，ref_plan_id 指向
--            billing「月度·标准」套餐；价格 8000 分=¥80 与套餐月费对齐）
-- 品类归属（裁定口径）：
--   - general 只进 手机/服饰/穿搭/家居 四个真实品类（品类 id 按名称查，不硬编码自增值）；
--   - 精选 = 运营位聚合、福利 = 秒杀入口，均不直接挂商品；
--   - recharge/subscription 不属浏览品类，category_id=0（走充值/订阅独立入口）。
-- 金额单位：分（BIGINT）。图片：占位路径 /img/goods/{id}.png。
-- 文件末尾附自检 SELECT（type=1 ≥32、categories=6、表 9 张等），输出 PASS/FAIL。

SET NAMES utf8mb4;
USE `market`;

-- 品类 id 按名称解析（db-schema.sql 种子按 sort 顺序插入，通常为 1..6，但不依赖自增值）。
-- 精选/福利不在此挂商品（精选=运营位聚合、福利=秒杀入口），故不解析其 id。
SET @cat_phone    = (SELECT id FROM categories WHERE name = '手机');
SET @cat_cloth    = (SELECT id FROM categories WHERE name = '服饰');
SET @cat_outfit   = (SELECT id FROM categories WHERE name = '穿搭');
SET @cat_home     = (SELECT id FROM categories WHERE name = '家居');
-- R2.5：goods 301 关联的 billing 套餐 id 按名解析（跨库同实例引用 ai_gateway.plans；
-- seed-plans.sql 已保证该行存在，缺失则下方 INSERT 撞 NOT NULL 报错而非静默落 0）。
SET @r25_plan_id = (SELECT id FROM ai_gateway.plans WHERE name = '月度·标准' AND status = 0);

-- ==================== general（type=1）====================
INSERT INTO goods
  (id, type, category_id, name, subtitle, price_cent, orig_price_cent,
   face_value_cent, bonus_cent, image_url, stock, limited, ref_plan_id, on_sale)
VALUES
  -- ---- 手机 101-110 ----
  (101, 1, @cat_phone,  '星耀 X7 Pro（12GB+256GB）',   '骁龙旗舰芯片 | 120W 超级快充',        499900, 549900, 0, 0, '/img/goods/101.png', 320, 0, 0, 1),
  (102, 1, @cat_phone,  '星耀 X7（12GB+128GB）',       '大内存长续航 | OLED 直屏',            399900, 429900, 0, 0, '/img/goods/102.png', 300, 0, 0, 1),
  (103, 1, @cat_phone,  '星云 Neo 5G（8GB+256GB）',    '轻薄机身 | 双模 5G',                  219900, 239900, 0, 0, '/img/goods/103.png', 260, 0, 0, 1),
  (104, 1, @cat_phone,  '星云 Ultra（16GB+512GB）',    '2K 高刷屏 | 潜望长焦',                699900, 749900, 0, 0, '/img/goods/104.png', 180, 0, 0, 1),
  (105, 1, @cat_phone,  '云雀 A3（8GB+128GB）',        '千元机皇 | 5000mAh 大电池',           149900, 169900, 0, 0, '/img/goods/105.png', 500, 0, 0, 1),
  (106, 1, @cat_phone,  '云雀 A3 Pro（12GB+256GB）',   '快充加持 | 全面屏',                   189900, 209900, 0, 0, '/img/goods/106.png', 420, 0, 0, 1),
  (107, 1, @cat_phone,  '极光 S2 折叠屏（12GB+512GB）','内外双屏 | 铰链铰寿命 20 万次',       899900, 999900, 0, 0, '/img/goods/107.png', 120, 0, 0, 1),
  (108, 1, @cat_phone,  '长风 Note12（6GB+128GB）',    '入门首选 | 双卡双待',                  99900, 119900, 0, 0, '/img/goods/108.png', 660, 0, 0, 1),
  (109, 1, @cat_phone,  '长风 Note12 Pro（8GB+256GB）','一亿像素主摄',                        139900, 159900, 0, 0, '/img/goods/109.png', 480, 0, 0, 1),
  (110, 1, @cat_phone,  '石墨 P1 卫星通信版（16GB+512GB）','无网也能发消息 | 钛合金边框',     799900, 849900, 0, 0, '/img/goods/110.png', 150, 0, 0, 1),
  -- ---- 服饰 111-120 ----
  (111, 1, @cat_cloth,  '男款三合一冲锋衣',            '防风防水 | 内胆可拆',                  39900,  49900, 0, 0, '/img/goods/111.png', 210, 0, 0, 1),
  (112, 1, @cat_cloth,  '女款 90 白鸭绒羽绒服',        '蓬松度 600 | 轻暖',                    59900,  69900, 0, 0, '/img/goods/112.png', 260, 0, 0, 1),
  (113, 1, @cat_cloth,  '重磅纯棉圆领 T 恤',           '260g 面料 | 不透不变形',                6900,   9900, 0, 0, '/img/goods/113.png', 800, 0, 0, 1),
  (114, 1, @cat_cloth,  '直筒水洗牛仔裤',              '经典靛蓝 | 微弹舒适',                  19900,  25900, 0, 0, '/img/goods/114.png', 350, 0, 0, 1),
  (115, 1, @cat_cloth,  '羊毛混纺针织开衫',            '软糯亲肤 | 通勤百搭',                  25900,  32900, 0, 0, '/img/goods/115.png', 190, 0, 0, 1),
  (116, 1, @cat_cloth,  '加绒运动卫裤',                '束脚版型 | 保暖不臃肿',                12900,  16900, 0, 0, '/img/goods/116.png', 400, 0, 0, 1),
  (117, 1, @cat_cloth,  '法兰绒长袖衬衫',              '磨毛工艺 | 保暖透气',                  15900,  19900, 0, 0, '/img/goods/117.png', 310, 0, 0, 1),
  (118, 1, @cat_cloth,  '双面呢羊毛大衣',              '挺括有型 | 御寒必备',                  79900,  99900, 0, 0, '/img/goods/118.png', 130, 0, 0, 1),
  (119, 1, @cat_cloth,  '速干透气 Polo 衫',            '吸湿排汗 | 商务运动两穿',               8900,  11900, 0, 0, '/img/goods/119.png', 520, 0, 0, 1),
  (120, 1, @cat_cloth,  '轻量 UPF50+ 防晒衣',          '凉感面料 | 户外必备',                  16900,  21900, 0, 0, '/img/goods/120.png', 380, 0, 0, 1),
  -- ---- 穿搭 121-130 ----
  (121, 1, @cat_outfit, '复古低帮帆布鞋',              '硫化鞋底 | 百搭经典',                  19900,  25900, 0, 0, '/img/goods/121.png', 460, 0, 0, 1),
  (122, 1, @cat_outfit, '轻量缓震跑鞋',                '高弹中底 | 上脚无负担',                29900,  39900, 0, 0, '/img/goods/122.png', 300, 0, 0, 1),
  (123, 1, @cat_outfit, '板层平檐棒球帽',              '可调节帽围 | 四季通用',                 5900,   7900, 0, 0, '/img/goods/123.png', 540, 0, 0, 1),
  (124, 1, @cat_outfit, '简约都市双肩包',              '15.6 寸电脑仓 | 防泼水面料',           25900,  32900, 0, 0, '/img/goods/124.png', 220, 0, 0, 1),
  (125, 1, @cat_outfit, '复古方形偏光墨镜',            'UV400 | 金属铰链',                     15900,  19900, 0, 0, '/img/goods/125.png', 240, 0, 0, 1),
  (126, 1, @cat_outfit, '手工编织皮带',                '头层牛皮 | 自动扣',                     8900,  12900, 0, 0, '/img/goods/126.png', 330, 0, 0, 1),
  (127, 1, @cat_outfit, '帆布托特包',                  '大容量 | 通勤买菜两相宜',               9900,  13900, 0, 0, '/img/goods/127.png', 410, 0, 0, 1),
  (128, 1, @cat_outfit, '羊毛罗纹围巾',                '加长加宽 | 柔软保暖',                   7900,  10900, 0, 0, '/img/goods/128.png', 370, 0, 0, 1),
  (129, 1, @cat_outfit, '极简快拆表带',                '氟橡胶材质 | 20mm 通用',                4900,   6900, 0, 0, '/img/goods/129.png', 600, 0, 0, 1),
  (130, 1, @cat_outfit, '素色宽檐渔夫帽',              '双面可戴 | 遮阳防晒',                   6900,   8900, 0, 0, '/img/goods/130.png', 450, 0, 0, 1),
  -- ---- 家居 131-140 ----
  (131, 1, @cat_home,   '60 支长绒棉四件套',           'A 类母婴级 | 裸睡级触感',              29900,  39900, 0, 0, '/img/goods/131.png', 200, 0, 0, 1),
  (132, 1, @cat_home,   '慢回弹记忆棉枕头',            '颈椎承托 | 可拆洗枕套',                12900,  16900, 0, 0, '/img/goods/132.png', 340, 0, 0, 1),
  (133, 1, @cat_home,   '北欧落地阅读灯',              '三档调光 | 护眼防频闪',                32900,  39900, 0, 0, '/img/goods/133.png', 160, 0, 0, 1),
  (134, 1, @cat_home,   '釉下彩马克杯（4 只装）',      '微波炉可用 | 釉面温润',                 9900,  12900, 0, 0, '/img/goods/134.png', 280, 0, 0, 1),
  (135, 1, @cat_home,   '大容量收纳箱三件套',          '可叠加 | 带滑轮',                      15900,  19900, 0, 0, '/img/goods/135.png', 250, 0, 0, 1),
  (136, 1, @cat_home,   '大豆蜡香薰蜡烛',              '天然精油 | 40h 燃烧',                   5900,   7900, 0, 0, '/img/goods/136.png', 390, 0, 0, 1),
  (137, 1, @cat_home,   '珊瑚绒加厚浴巾',              '吸水速干 | 不掉毛',                     8900,  11900, 0, 0, '/img/goods/137.png', 360, 0, 0, 1),
  (138, 1, @cat_home,   '静音超声波加湿器',            '5L 大水箱 | 低噪不扰眠',               19900,  24900, 0, 0, '/img/goods/138.png', 230, 0, 0, 1),
  (139, 1, @cat_home,   '北欧短绒地毯',                '防滑底背 | 易打理',                    26900,  33900, 0, 0, '/img/goods/139.png', 140, 0, 0, 1),
  (140, 1, @cat_home,   '免打孔浴室置物架',            '太空铝材质 | 承重 10kg',               10900,  13900, 0, 0, '/img/goods/140.png', 470, 0, 0, 1),
  -- ---- recharge（type=2）：面值=price_cent，赠送入账=面值+bonus ----
  (201, 2, 0, '余额充值 ¥10',  '到账 10.00 元',                 1000,  0, 1000,    0, '/img/goods/201.png', 0, 0, 0, 1),
  (202, 2, 0, '余额充值 ¥50',  '到账 53.00 元（赠 ¥3）',        5000,  0, 5000,  300, '/img/goods/202.png', 0, 0, 0, 1),
  (203, 2, 0, '余额充值 ¥100', '到账 110.00 元（赠 ¥10）',     10000,  0, 10000, 1000, '/img/goods/203.png', 0, 0, 0, 1),
  -- ---- subscription（type=3）：会员月卡，关联 billing「月度·标准」套餐（R2.5 上架） ----
  (301, 3, 0, '会员月卡·标准', '月度·标准套餐 | ¥80/月 · 30 天 · 每 5h 滚动额度', 8000, 0, 0, 0, '/img/goods/301.png', 999, 0, @r25_plan_id, 1)
AS new
ON DUPLICATE KEY UPDATE
  type            = new.type,
  category_id     = new.category_id,
  name            = new.name,
  subtitle        = new.subtitle,
  price_cent      = new.price_cent,
  orig_price_cent = new.orig_price_cent,
  face_value_cent = new.face_value_cent,
  bonus_cent      = new.bonus_cent,
  image_url       = new.image_url,
  stock           = new.stock,
  limited         = new.limited,
  ref_plan_id     = new.ref_plan_id,
  on_sale         = new.on_sale;

-- ==================== 自检（PASS/FAIL 逐项输出） ====================
SELECT
  CASE WHEN (SELECT COUNT(*) FROM goods WHERE type = 1) >= 32 THEN 'PASS' ELSE 'FAIL' END
    AS chk_general_ge_32,
  (SELECT COUNT(*) FROM goods WHERE type = 1) AS general_count;

SELECT
  CASE WHEN (SELECT COUNT(*) FROM categories) = 6 THEN 'PASS' ELSE 'FAIL' END
    AS chk_categories_6,
  (SELECT COUNT(*) FROM categories) AS categories_count;

SELECT
  CASE WHEN (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'market') = 9 THEN 'PASS' ELSE 'FAIL' END
    AS chk_tables_9,
  (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'market') AS tables_count;

SELECT
  CASE WHEN (SELECT COUNT(*) FROM goods WHERE type = 2
              AND (face_value_cent, bonus_cent) IN ((1000,0),(5000,300),(10000,1000))
              AND price_cent = face_value_cent) = 3 THEN 'PASS' ELSE 'FAIL' END
    AS chk_recharge_3tiers,
  (SELECT COUNT(*) FROM goods WHERE type = 2) AS recharge_count;

SELECT
  CASE WHEN (SELECT COUNT(*) FROM goods WHERE type = 3 AND ref_plan_id > 0 AND on_sale = 1) = 1 THEN 'PASS' ELSE 'FAIL' END
    AS chk_subscription_onsale,
  (SELECT COUNT(*) FROM goods WHERE type = 3) AS subscription_count;

-- R2.5：goods 301 与 billing 套餐联动自检（跨库 JOIN：关联行存在、在售、定期、30 天）。
SELECT
  CASE WHEN (SELECT COUNT(*) FROM goods g JOIN ai_gateway.plans p ON g.ref_plan_id = p.id
             WHERE g.id = 301 AND p.status = 0 AND p.plan_type = 'recurring' AND p.validity_days = 30) = 1
       THEN 'PASS' ELSE 'FAIL' END
    AS chk_goods301_plan_linkage;

SELECT
  CASE WHEN (SELECT COUNT(*) FROM goods WHERE type = 1 AND (on_sale <> 1 OR stock < 100)) = 0 THEN 'PASS' ELSE 'FAIL' END
    AS chk_general_onsale_stock,
  (SELECT COUNT(*) FROM goods WHERE type = 1 AND category_id IN
      (SELECT id FROM categories WHERE name IN ('手机','服饰','穿搭','家居'))) AS general_in_4_real_cats;

-- 幂等行数复核：总行数应恰为 44（40 general + 3 recharge + 1 subscription）
SELECT
  CASE WHEN (SELECT COUNT(*) FROM goods) = 44 THEN 'PASS' ELSE 'FAIL' END
    AS chk_total_rows_44,
  (SELECT COUNT(*) FROM goods) AS goods_total;
