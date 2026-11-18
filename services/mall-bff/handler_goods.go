// handler_goods.go：商品域端点（api-contract §3）。
// categories / goods 列表（键集游标分页）/ goods 详情。
// 游标与 page_size 在 bff 侧先裁剪校验（PageReq 契约注释："page_size 默认 20、
// 上限 50（BFF 侧裁剪）"）；垃圾游标 → 400 MALL_10005，不落下游。
package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/pkg/mallpage"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// goodsJSON 契约 §3 Goods 视图。type 以字符串出（proto int32 1/2/3 →
// general/recharge/subscription）；token_estimate 仅 recharge（omitempty）；
// detail 仅详情（列表项 proto 恒空串，omitempty 后不出现在列表 JSON）。
type goodsJSON struct {
	ID            uint64 `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	Subtitle      string `json:"subtitle"`
	PriceCent     int64  `json:"price_cent"`
	OrigPriceCent int64  `json:"orig_price_cent"`
	FaceValueCent int64  `json:"face_value_cent,omitempty"`
	BonusCent     int64  `json:"bonus_cent,omitempty"`
	ImageURL      string `json:"image_url"`
	Stock         int32  `json:"stock"`
	RefPlanID     uint64 `json:"ref_plan_id,omitempty"`
	OnSale        bool   `json:"on_sale"`
	Detail        string `json:"detail,omitempty"`
	TokenEstimate int64  `json:"token_estimate,omitempty"`
	CategoryID    uint64 `json:"category_id"`
	Limited       bool   `json:"limited,omitempty"`
}

// goodsTypeStr proto type 编码 → 契约字符串（表外值回落 general，防御）。
func goodsTypeStr(t int32) string {
	switch t {
	case 2:
		return "recharge"
	case 3:
		return "subscription"
	default:
		return "general"
	}
}

func toGoodsJSON(g *mallv1.GoodsItem) goodsJSON {
	return goodsJSON{
		ID:            g.GetId(),
		Type:          goodsTypeStr(g.GetType()),
		Name:          g.GetName(),
		Subtitle:      g.GetSubtitle(),
		PriceCent:     g.GetPriceCent(),
		OrigPriceCent: g.GetOrigPriceCent(),
		FaceValueCent: g.GetFaceValueCent(),
		BonusCent:     g.GetBonusCent(),
		ImageURL:      g.GetImageUrl(),
		Stock:         g.GetStock(),
		RefPlanID:     g.GetRefPlanId(),
		OnSale:        g.GetOnSale(),
		Detail:        g.GetDetail(),
		TokenEstimate: g.GetTokenEstimate(),
		CategoryID:    g.GetCategoryId(),
		Limited:       g.GetLimited(),
	}
}

// handleCategories 品类列表（六品类，精选为运营位）。
func handleCategories(goods GoodsClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := goods.ListCategories(ctx, &emptypb.Empty{})
		if err != nil {
			abortWith(c, err)
			return
		}
		items := make([]gin.H, 0, len(reply.GetItems()))
		for _, it := range reply.GetItems() {
			items = append(items, gin.H{"id": it.GetId(), "name": it.GetName(), "sort": it.GetSort()})
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

// handleListGoods 商品列表（category_id 可选；游标分页）。
func handleListGoods(goods GoodsClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		var categoryID uint64
		if q := c.Query("category_id"); q != "" {
			v, err := strconv.ParseUint(q, 10, 64)
			if err != nil {
				abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "category_id 须为非负整数"))
				return
			}
			categoryID = v
		}
		page, err := parsePageReq(c)
		if err != nil {
			abortWith(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := goods.ListGoods(ctx, &mallv1.ListGoodsReq{CategoryId: categoryID, Page: page})
		if err != nil {
			abortWith(c, err)
			return
		}
		items := make([]goodsJSON, 0, len(reply.GetItems()))
		for _, it := range reply.GetItems() {
			items = append(items, toGoodsJSON(it))
		}
		writePage(c, items, reply.GetNextCursor(), reply.GetHasMore())
	}
}

// handleGoodsDetail 商品详情（含 detail 富文本；不存在 → MALL_20001 载体还原）。
func handleGoodsDetail(goods GoodsClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil || id == 0 {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "商品 id 须为正整数"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		item, err := goods.GetGoods(ctx, &mallv1.GetGoodsReq{Id: id})
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, toGoodsJSON(item))
	}
}

// parsePageReq 游标分页公共解析：cursor 垃圾 → MALL_10005；page_size
// 缺省 20、上限 50、负数/垃圾 → MALL_10005。
func parsePageReq(c *gin.Context) (*mallv1.PageReq, error) {
	page := &mallv1.PageReq{}
	if cur := c.Query("cursor"); cur != "" {
		if _, _, err := mallpage.DecodeCursor(cur); err != nil {
			return nil, mallerr.E(mallerr.CodeInvalidParam, false, "cursor 非法（应为分页响应返回的不透明令牌）")
		}
		page.Cursor = cur
	}
	if q := c.Query("page_size"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			return nil, mallerr.E(mallerr.CodeInvalidParam, false, "page_size 须为非负整数")
		}
		if n > 50 {
			n = 50 // 契约 §0.3：上限 50，BFF 侧裁剪
		}
		if n > 0 {
			page.PageSize = int32(n)
		}
	}
	return page, nil
}
