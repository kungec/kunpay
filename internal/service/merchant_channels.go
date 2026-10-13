package service

import (
	"errors"
	"strings"

	"kunzhifu/internal/model"
)

// 商户自有通道: 同类型可绑定 N 个收款码,开启轮询后按"最久未使用"轮流展示。

var ErrRotateNeedTwo = errors.New("开启轮询需要至少 2 个启用的收款码")

func scanItem(row interface{ Scan(...any) error }) (*model.MerchantItem, error) {
	it := &model.MerchantItem{}
	err := row.Scan(&it.ID, &it.UserID, &it.Type, &it.Name, &it.Qrcode, &it.Enabled, &it.Weight, &it.CreatedAt, &it.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return it, nil
}

const itemCols = "id,user_id,type,name,qrcode,enabled,COALESCE(weight,1),created_at,last_used_at"

// MerchantItems 商户某类型的全部收款码
func (s *Store) MerchantItems(userID int64, typ string) ([]model.MerchantItem, error) {
	rows, err := s.DB.Query("SELECT "+itemCols+" FROM merchant_channel_items WHERE user_id=? AND type=? ORDER BY id ASC", userID, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.MerchantItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// MerchantRotate 商户某类型是否开启轮询
func (s *Store) MerchantRotate(userID int64, typ string) bool {
	var rotate int
	err := s.DB.QueryRow("SELECT rotate FROM merchant_channels WHERE user_id=? AND type=?", userID, typ).Scan(&rotate)
	return err == nil && rotate == 1
}

// AddMerchantItem 添加收款码
func (s *Store) AddMerchantItem(userID int64, typ, name, qrcode string, weight int64) error {
	weight = clampWeight(weight)
	_, err := s.DB.Exec(`INSERT INTO merchant_channel_items(user_id,type,name,qrcode,enabled,weight,created_at,last_used_at)
		VALUES(?,?,?,?,1,?,?,0)`, userID, typ, strings.TrimSpace(name), qrcode, weight, model.Now())
	return err
}

// SetMerchantItemWeight 调整收款码权重(轮询按权重比例分配订单)
func (s *Store) SetMerchantItemWeight(userID, itemID int64, weight int64) error {
	_, err := s.DB.Exec("UPDATE merchant_channel_items SET weight=? WHERE id=? AND user_id=?",
		clampWeight(weight), itemID, userID)
	return err
}

func clampWeight(w int64) int64 {
	if w < 1 {
		return 1
	}
	if w > 1000 {
		return 1000
	}
	return w
}

// MerchantItemOf 取商户的一个收款码(校验归属)
func (s *Store) MerchantItemOf(userID, itemID int64) (*model.MerchantItem, error) {
	it, err := scanItem(s.DB.QueryRow("SELECT "+itemCols+" FROM merchant_channel_items WHERE id=? AND user_id=?", itemID, userID))
	if err != nil {
		return nil, err
	}
	return it, nil
}

// SetMerchantItemState 启停收款码;停用后若轮询条件不满足自动关闭轮询
func (s *Store) SetMerchantItemState(userID, itemID int64, enabled int) error {
	it, err := s.MerchantItemOf(userID, itemID)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec("UPDATE merchant_channel_items SET enabled=? WHERE id=?", enabled, itemID); err != nil {
		return err
	}
	if enabled == 0 {
		s.autoCloseRotate(userID, it.Type)
	}
	return nil
}

// DeleteMerchantItem 删除收款码;删除后若轮询条件不满足自动关闭轮询
func (s *Store) DeleteMerchantItem(userID, itemID int64) error {
	it, err := s.MerchantItemOf(userID, itemID)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec("DELETE FROM merchant_channel_items WHERE id=? AND user_id=?", itemID, userID); err != nil {
		return err
	}
	s.autoCloseRotate(userID, it.Type)
	return nil
}

// SetMerchantRotate 开关轮询;开启要求该类型 ≥2 个启用的收款码
func (s *Store) SetMerchantRotate(userID int64, typ string, on bool) error {
	if on {
		var n int
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM merchant_channel_items WHERE user_id=? AND type=? AND enabled=1",
			userID, typ).Scan(&n)
		if n < 2 {
			return ErrRotateNeedTwo
		}
	}
	_, err := s.DB.Exec(`INSERT INTO merchant_channels(user_id,type,rotate,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id,type) DO UPDATE SET rotate=excluded.rotate, updated_at=excluded.updated_at`,
		userID, typ, boolToInt(on), model.Now())
	return err
}

func (s *Store) autoCloseRotate(userID int64, typ string) {
	var n int
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM merchant_channel_items WHERE user_id=? AND type=? AND enabled=1",
		userID, typ).Scan(&n)
	if n < 2 {
		_, _ = s.DB.Exec("UPDATE merchant_channels SET rotate=0, updated_at=? WHERE user_id=? AND type=?",
			model.Now(), userID, typ)
	}
}

// PickMerchantQR 收银台取码: 开启轮询且 ≥2 个启用码时按订单号取模轮流选取(确定性,
// 同一订单刷新多次仍命中同一码)。返回 (收款码地址, 码ID),无可用码返回 ("",0)。
func (s *Store) PickMerchantQR(orderID int64, userID int64, typ string) (string, int64) {
	items, err := s.MerchantItems(userID, typ)
	if err != nil {
		return "", 0
	}
	var enabled []model.MerchantItem
	for _, it := range items {
		if it.Enabled == 1 {
			enabled = append(enabled, it)
		}
	}
	if len(enabled) == 0 {
		return "", 0
	}
	rotate := s.MerchantRotate(userID, typ)
	idx := 0
	if rotate && len(enabled) >= 2 {
		idx = weightedIndex(enabled, func(i int) int64 { return enabled[i].Weight }, orderID)
	}
	pick := enabled[idx]
	_, _ = s.DB.Exec("UPDATE merchant_channel_items SET last_used_at=? WHERE id=?", model.Now(), pick.ID)
	return pick.Qrcode, pick.ID
}

// weightedIndex 按权重确定性选码: r = orderID % 总权重,落在哪个累计区间就选哪个。
// 同一订单刷新多次仍命中同一码,高权重码按比例获得更多订单。
func weightedIndex[T any](items []T, weightOf func(int) int64, orderID int64) int {
	total := int64(0)
	for i := range items {
		total += clampWeight(weightOf(i))
	}
	if total <= 0 {
		return 0
	}
	r := orderID % total
	var acc int64
	for i := range items {
		acc += clampWeight(weightOf(i))
		if r < acc {
			return i
		}
	}
	return 0
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
