-- my-market market 库 DDL（spec/db-schema.sql）
-- 依据 PRD v0.3 §7 + D15/D17。金额单位：分（BIGINT）。
-- 与 ai 库（ai-gateway 既有）同实例不同 schema：dev 同容器 MySQL :3307。
-- 字符集 utf8mb4 / 引擎 InnoDB / 行格式 DYNAMIC；主键统一 BIGINT UNSIGNED AUTO_INCREMENT。

CREATE DATABASE IF NOT EXISTS `market`
  DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
USE `market`;

-- ---------- 品类 ----------
CREATE TABLE `categories` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `name`       VARCHAR(32)  NOT NULL COMMENT '精选/手机/服饰/穿搭/家居/福利',
  `sort`       INT          NOT NULL DEFAULT 0,
  `status`     TINYINT      NOT NULL DEFAULT 1 COMMENT '1 上架 0 下架',
  `created_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB COMMENT='商品品类';

-- ---------- 商品 ----------
CREATE TABLE `goods` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `type`            TINYINT      NOT NULL COMMENT '1 general 2 recharge 3 subscription',
  `category_id`     BIGINT UNSIGNED NOT NULL,
  `name`            VARCHAR(128) NOT NULL,
  `subtitle`        VARCHAR(255) NOT NULL DEFAULT '',
  `price_cent`      BIGINT       NOT NULL COMMENT '售价（分）',
  `orig_price_cent` BIGINT       NOT NULL DEFAULT 0 COMMENT '划线价',
  `face_value_cent` BIGINT       NOT NULL DEFAULT 0 COMMENT 'recharge 面值（分），仅 type=2 有效',
  `bonus_cent`      BIGINT       NOT NULL DEFAULT 0 COMMENT 'recharge 赠送（分），入账=面值+赠送',
  `image_url`       VARCHAR(512) NOT NULL DEFAULT '',
  `detail`          MEDIUMTEXT   NULL COMMENT '详情富文本',
  `stock`           INT          NOT NULL DEFAULT 0 COMMENT 'general 库存；下单事务内条件更新',
  `limited`         TINYINT      NOT NULL DEFAULT 0 COMMENT 'recharge/subscription 是否限量（限量走 stock）',
  `ref_plan_id`     BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'subscription 映射 billing plan id',
  `on_sale`         TINYINT      NOT NULL DEFAULT 1,
  `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_category` (`category_id`, `id`),
  KEY `idx_type_sale` (`type`, `on_sale`)
) ENGINE=InnoDB COMMENT='商品（general/recharge/subscription）';

-- ---------- 购物车 ----------
CREATE TABLE `cart_items` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `tenant_id`  BIGINT UNSIGNED NOT NULL,
  `goods_id`   BIGINT UNSIGNED NOT NULL,
  `qty`        INT          NOT NULL DEFAULT 1 COMMENT '1-99',
  `created_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_tenant_goods` (`tenant_id`, `goods_id`),
  KEY `idx_tenant` (`tenant_id`),
  CONSTRAINT `chk_qty` CHECK (`qty` BETWEEN 1 AND 99)
) ENGINE=InnoDB COMMENT='购物车（可购类型：general/subscription）';

-- ---------- 订单 ----------
CREATE TABLE `mall_orders` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `order_no`       CHAR(21)     NOT NULL COMMENT 'M+yyyyMMddHHmmss+6位随机',
  `tenant_id`      BIGINT UNSIGNED NOT NULL,
  `status`         TINYINT      NOT NULL COMMENT '1 created 2 paid 3 fulfilled 4 refunded 5 closed',
  `amount_cent`    BIGINT       NOT NULL,
  `idem_key`       VARCHAR(64)  NOT NULL,
  `kind`           TINYINT      NOT NULL COMMENT '1 recharge直购 2 cart混合 3 seckill',
  `closed_reason`  TINYINT      NOT NULL DEFAULT 0 COMMENT '1 timeout 2 user（退款终态为 refunded，流水另记）',
  `paid_at`        DATETIME(3)  NULL,
  `fulfilled_at`   DATETIME(3)  NULL,
  `closed_at`      DATETIME(3)  NULL,
  `created_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_order_no` (`order_no`),
  UNIQUE KEY `uk_tenant_idem` (`tenant_id`, `idem_key`),
  KEY `idx_tenant_list` (`tenant_id`, `id`),
  KEY `idx_status_created` (`status`, `created_at`) COMMENT '超时扫描/对账'
) ENGINE=InnoDB COMMENT='商城订单主表';

-- ---------- 订单行 ----------
CREATE TABLE `mall_order_items` (
  `id`                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `order_id`            BIGINT UNSIGNED NOT NULL,
  `order_no`            CHAR(21)     NOT NULL COMMENT '冗余，履约 ref 用',
  `line_no`             INT          NOT NULL COMMENT '行号从 1；ref_id = order_no:line_no',
  `type`                TINYINT      NOT NULL COMMENT '1 ship(general) 2 credit(recharge) 3 subscribe',
  `goods_id`            BIGINT UNSIGNED NOT NULL,
  `qty`                 INT          NOT NULL,
  `price_snapshot_cent` BIGINT       NOT NULL COMMENT '下单时行价格快照',
  `face_value_cent`     BIGINT       NOT NULL DEFAULT 0,
  `bonus_cent`          BIGINT       NOT NULL DEFAULT 0,
  `created_at`          DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_order_line` (`order_id`, `line_no`),
  KEY `idx_order_no` (`order_no`)
) ENGINE=InnoDB COMMENT='订单行（行级价格快照与履约类型）';

-- ---------- 支付流水 ----------
CREATE TABLE `payment_records` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `order_no`          CHAR(21)     NOT NULL,
  `provider`          VARCHAR(16)  NOT NULL COMMENT 'mock | stripe_test | wechat',
  `provider_trade_no` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '外部支付单号',
  `status`            TINYINT      NOT NULL COMMENT '1 created 2 paid 3 failed 4 refunded',
  `amount_cent`       BIGINT       NOT NULL,
  `callback_payload`  JSON         NULL COMMENT '回调原文（审计）',
  `signature_verified` TINYINT     NOT NULL DEFAULT 0,
  `created_at`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_order_no` (`order_no`),
  KEY `idx_provider_trade` (`provider_trade_no`)
) ENGINE=InnoDB COMMENT='支付流水（Provider 回调幂等依据：order_no+provider_trade_no）';

-- ---------- 履约 outbox ----------
CREATE TABLE `fulfillment_tasks` (
  `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `order_no`    CHAR(21)     NOT NULL,
  `line_no`     INT          NOT NULL,
  `task_type`   TINYINT      NOT NULL COMMENT '1 ship 2 credit 3 subscribe',
  `status`      TINYINT      NOT NULL DEFAULT 1 COMMENT '1 pending 2 done 3 failed 4 voided(退款作废)',
  `retry_count` INT          NOT NULL DEFAULT 0 COMMENT '指数退避，上限 5',
  `last_error`  VARCHAR(512) NOT NULL DEFAULT '',
  `next_run_at` DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_order_line` (`order_no`, `line_no`),
  KEY `idx_status_next` (`status`, `next_run_at`) COMMENT 'worker 拉取/对账扫描'
) ENGINE=InnoDB COMMENT='履约 outbox：支付事务原子写入，worker 异步执行';

-- ---------- 秒杀活动 ----------
CREATE TABLE `seckill_activities` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `goods_id`           BIGINT UNSIGNED NOT NULL,
  `seckill_price_cent` BIGINT       NOT NULL,
  `total_stock`        INT          NOT NULL,
  `sold_stock`         INT          NOT NULL DEFAULT 0 COMMENT 'DB 侧兜底计数',
  `redis_key`          VARCHAR(128) NOT NULL DEFAULT '' COMMENT '库存预热键',
  `start_at`           DATETIME(3)  NOT NULL,
  `end_at`             DATETIME(3)  NOT NULL,
  `status`             TINYINT      NOT NULL DEFAULT 1 COMMENT '1 草稿 2 上线 3 结束',
  `created_at`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_status_window` (`status`, `start_at`, `end_at`),
  KEY `idx_goods` (`goods_id`)
) ENGINE=InnoDB COMMENT='秒杀活动（库存独立于 goods.stock）';

-- ---------- 秒杀订单 ----------
CREATE TABLE `seckill_orders` (
  `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `voucher_no`  CHAR(21)     NOT NULL COMMENT 'seckill 服务预生成，= order_no',
  `activity_id` BIGINT UNSIGNED NOT NULL,
  `tenant_id`   BIGINT UNSIGNED NOT NULL,
  `status`      TINYINT      NOT NULL DEFAULT 1 COMMENT '1 queued 2 pending_payment 3 paid 4 fulfilled 5 timeout 6 cancelled',
  `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_voucher` (`voucher_no`),
  UNIQUE KEY `uk_activity_tenant` (`activity_id`, `tenant_id`) COMMENT '每人限购 1 的最终防线',
  KEY `idx_status_created` (`status`, `created_at`) COMMENT '超时扫描（15 分钟）与对账'
) ENGINE=InnoDB COMMENT='秒杀订单（Kafka 消费者落库）';

-- ---------- 初始品类数据 ----------
INSERT INTO `categories` (`name`, `sort`) VALUES
  ('精选', 1), ('手机', 2), ('服饰', 3), ('穿搭', 4), ('家居', 5), ('福利', 6);
