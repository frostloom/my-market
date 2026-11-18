// mall-goods：server.go 把 Store 结果映到 mallv1 契约消息，
// 并把 mallerr 契约码映射为 gRPC status（bff 侧再映回 HTTP）。
package main

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// goodsServer mallv1.GoodsService 实现（读路径，无状态，全部走 Store）。
type goodsServer struct {
	mallv1.UnimplementedGoodsServiceServer
	store *Store
}

func newGoodsServer(store *Store) *goodsServer {
	return &goodsServer{store: store}
}

// ListCategories 品类列表（契约 GET /categories）。
func (s *goodsServer) ListCategories(ctx context.Context, _ *emptypb.Empty) (*mallv1.ListCategoriesReply, error) {
	rows, err := s.store.ListCategories(ctx)
	if err != nil {
		return nil, toGRPCError(err)
	}
	items := make([]*mallv1.Category, 0, len(rows))
	for _, r := range rows {
		items = append(items, &mallv1.Category{Id: r.ID, Name: r.Name, Sort: r.Sort})
	}
	return &mallv1.ListCategoriesReply{Items: items}, nil
}

// ListGoods 按品类键集分页取在售商品（契约 GET /goods）。
// detail 仅详情返回，列表项恒为空（契约 §3）；token_estimate 仅 recharge 填充。
func (s *goodsServer) ListGoods(ctx context.Context, req *mallv1.ListGoodsReq) (*mallv1.ListGoodsReply, error) {
	var categoryID uint64
	var pageSize int32
	var cursor string
	if req != nil {
		categoryID = req.GetCategoryId()
		cursor = req.GetPage().GetCursor()
		pageSize = req.GetPage().GetPageSize()
	}
	rows, next, hasMore, err := s.store.ListGoods(ctx, categoryID, cursor, pageSize)
	if err != nil {
		return nil, toGRPCError(err)
	}
	items := make([]*mallv1.GoodsItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, toProtoItem(r, false))
	}
	return &mallv1.ListGoodsReply{Items: items, NextCursor: next, HasMore: hasMore}, nil
}

// GetGoods 商品详情（契约 GET /goods/{id}，含 detail 富文本）。
func (s *goodsServer) GetGoods(ctx context.Context, req *mallv1.GetGoodsReq) (*mallv1.GoodsItem, error) {
	var id uint64
	if req != nil {
		id = req.GetId()
	}
	row, err := s.store.GetGoods(ctx, id)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return toProtoItem(*row, true), nil
}

// toProtoItem 行 → 契约消息。withDetail 仅 GetGoods 为 true（契约 §3：
// 列表项不含详情富文本）；recharge 附 token_estimate（营销展示，非入账依据）。
func toProtoItem(r goodsRow, withDetail bool) *mallv1.GoodsItem {
	item := &mallv1.GoodsItem{
		Id:            r.ID,
		Type:          int32(r.Type),
		Name:          r.Name,
		Subtitle:      r.Subtitle,
		PriceCent:     r.PriceCent,
		OrigPriceCent: r.OrigPriceCent,
		FaceValueCent: r.FaceValueCent,
		BonusCent:     r.BonusCent,
		ImageUrl:      r.ImageURL,
		Stock:         r.Stock,
		OnSale:        r.OnSale != 0,
		RefPlanId:     r.RefPlanID,
	}
	if r.Type == typeRecharge {
		item.TokenEstimate = tokenEstimate(r.FaceValueCent + r.BonusCent)
	}
	if withDetail && r.Detail != nil {
		item.Detail = *r.Detail
	}
	return item
}

// toGRPCError mallerr → gRPC status（统一走 pkg/mallerr.ToGRPC 载体：
// gRPC 码按契约码 HTTP 语义行映射，ErrorInfo.Reason 携带 MALL 码 +
// metadata retryable，bff 侧 FromGRPC 还原；无码错误回落 Internal）。
func toGRPCError(err error) error {
	return mallerr.ToGRPC(err)
}
