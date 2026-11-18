// handler_cart.go：购物车四端点（api-contract §4）。
// tenant 全部取自会话中间件（客户端传 tenant 一律忽略——sequences §6 口径）。
// 业务校验（行数上限/qty 越界/类型可购）在 mall-order 侧，错误经载体还原；
// bff 只做入参形状与 qty 边界快筛（MALL_20005：qty>99 或 <1）。
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

const cartBodyLimit = 4 << 10

// cartItemJSON 契约 §4：{"id":..,"goods":Goods,"qty":..}。
type cartItemJSON struct {
	ID    uint64    `json:"id"`
	Goods goodsJSON `json:"goods"`
	Qty   int32     `json:"qty"`
}

// cartJSON 契约 §4：{"items":[...],"total_cent":..}。
type cartJSON struct {
	Items     []cartItemJSON `json:"items"`
	TotalCent int64          `json:"total_cent"`
}

func toCartJSON(v *mallv1.CartReply) cartJSON {
	items := make([]cartItemJSON, 0, len(v.GetItems()))
	for _, it := range v.GetItems() {
		items = append(items, cartItemJSON{ID: it.GetId(), Goods: toGoodsJSON(it.GetGoods()), Qty: it.GetQty()})
	}
	return cartJSON{Items: items, TotalCent: v.GetTotalCent()}
}

// handleGetCart 购物车视图。
func handleGetCart(orders OrderClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := orders.GetCart(ctx, &mallv1.GetCartReq{TenantId: tenantID(c)})
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, toCartJSON(reply))
	}
}

// bindCartBody 读取并限长 JSON 请求体（防大包打下游）。
func bindCartBody(c *gin.Context, into any) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, cartBodyLimit+1))
	if err != nil || len(raw) > cartBodyLimit {
		return mallerr.E(mallerr.CodeInvalidParam, false, "请求体过大（上限 4KB）")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return mallerr.E(mallerr.CodeInvalidParam, false, "请求体须为合法 JSON")
	}
	return nil
}

// handleAddCart POST /cart/items：upsert 同商品合并 qty；上游拒绝（20 行上限
// MALL_20004 / recharge 拒 MALL_30007 / 下架 MALL_20002 等）经载体还原。
func handleAddCart(orders OrderClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			GoodsID uint64 `json:"goods_id"`
			Qty     int32  `json:"qty"`
		}
		if err := bindCartBody(c, &in); err != nil {
			abortWith(c, err)
			return
		}
		if in.GoodsID == 0 {
			abortWith(c, mallerr.E(mallerr.CodeInvalidParam, false, "goods_id 必填"))
			return
		}
		if in.Qty < 1 || in.Qty > 99 {
			abortWith(c, mallerr.E(mallerr.CodeGoodsQtyExceeded, false, "商品数量超限（1-99）"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := orders.AddCart(ctx, &mallv1.AddCartReq{TenantId: tenantID(c), GoodsId: in.GoodsID, Qty: in.Qty})
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, toCartJSON(reply))
	}
}

// handlePatchCartQty PATCH /cart/items/{id}：qty=0 等价删除；越界 → MALL_20005。
func handlePatchCartQty(orders OrderClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		itemID, err := parseIDParam(c, "id")
		if err != nil {
			abortWith(c, err)
			return
		}
		var in struct {
			Qty int32 `json:"qty"`
		}
		if err := bindCartBody(c, &in); err != nil {
			abortWith(c, err)
			return
		}
		if in.Qty < 0 || in.Qty > 99 {
			abortWith(c, mallerr.E(mallerr.CodeGoodsQtyExceeded, false, "商品数量超限（0-99，0 等价删除）"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		reply, err := orders.PatchCartQty(ctx, &mallv1.PatchCartQtyReq{TenantId: tenantID(c), ItemId: itemID, Qty: in.Qty})
		if err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, toCartJSON(reply))
	}
}

// handleRemoveCart DELETE /cart/items/{id}：幂等 200 {}。
func handleRemoveCart(orders OrderClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		itemID, err := parseIDParam(c, "id")
		if err != nil {
			abortWith(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		if _, err := orders.RemoveCart(ctx, &mallv1.RemoveCartReq{TenantId: tenantID(c), ItemId: itemID}); err != nil {
			abortWith(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{})
	}
}

// parseIDParam 解析路径中的正整数 id。
func parseIDParam(c *gin.Context, name string) (uint64, error) {
	v, err := parseUintStr(c.Param(name))
	if err != nil || v == 0 {
		return 0, mallerr.E(mallerr.CodeInvalidParam, false, "%s 须为正整数", name)
	}
	return v, nil
}
