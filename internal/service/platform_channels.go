package service

import (
	"errors"

	"kunzhifu/internal/model"
)

// 平台渠道多码: 同类型可配置 N 个收款码,开启轮询后按最久未使用轮流展示。
// 与商户自有通道同机制,区别是平台码属于运营方(管理后台-渠道管理维护)。

var ErrPlatformRotateNeedTwo = errors.New("开启轮询需要至少 2 个启用的收款码")

const platItemCols = "id,type,name,qrcode,enabled,created_at,last_used_at"

func scanPlatItem(row interface{ Scan(...any) error }) (*model.MerchantItem, error) {
	it := &model.MerchantItem{}
	err := row.Scan(&it.ID, &it.Type, &it.Name, &it.Qrcode, &it.Enabled, &it.CreatedAt, &it.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return it, nil
}

// PlatformItems 平台某类型的全部收款码
func (s *Store) PlatformItems(typ string) ([]model.MerchantItem, error) {
	rows, err := s.DB.Query("SELECT "+platItemCols+" FROM platform_channel_items WHERE type=? ORDER BY id ASC", typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.MerchantItem
	for rows.Next() {
		it, err := scanPlatItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// PlatformRotate 平台某类型是否开启轮询
func (s *Store) PlatformRotate(typ string) bool {
	var rotate int
	err := s.DB.QueryRow("SELECT rotate FROM channels WHERE type=?", typ).Scan(&rotate)
	return err == nil && rotate == 1
}

// AddPlatformItem 添加平台收款码
func (s *Store) AddPlatformItem(typ, name, qrcode string) error {
	_, err := s.DB.Exec(`INSERT INTO platform_channel_items(type,name,qrcode,enabled,created_at,last_used_at)
		VALUES(?,?,?,1,?,0)`, typ, name, qrcode, model.Now())
	return err
}

// PlatformItemOf 取平台收款码
func (s *Store) PlatformItemOf(id int64) (*model.MerchantItem, error) {
	return scanPlatItem(s.DB.QueryRow("SELECT "+platItemCols+" FROM platform_channel_items WHERE id=?", id))
}

// SetPlatformItemState 启停平台收款码;不足 2 个自动关轮询
func (s *Store) SetPlatformItemState(id int64, enabled int) error {
	it, err := s.PlatformItemOf(id)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec("UPDATE platform_channel_items SET enabled=? WHERE id=?", enabled, id); err != nil {
		return err
	}
	if enabled == 0 {
		s.autoClosePlatformRotate(it.Type)
	}
	return nil
}

// DeletePlatformItem 删除平台收款码
func (s *Store) DeletePlatformItem(id int64) error {
	it, err := s.PlatformItemOf(id)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec("DELETE FROM platform_channel_items WHERE id=?", id); err != nil {
		return err
	}
	s.autoClosePlatformRotate(it.Type)
	return nil
}

// SetPlatformRotate 开关平台轮询(需 ≥2 个启用码)
func (s *Store) SetPlatformRotate(typ string, on bool) error {
	if on {
		var n int
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM platform_channel_items WHERE type=? AND enabled=1", typ).Scan(&n)
		if n < 2 {
			return ErrPlatformRotateNeedTwo
		}
	}
	_, err := s.DB.Exec("UPDATE channels SET rotate=? WHERE type=?", boolToInt(on), typ)
	return err
}

func (s *Store) autoClosePlatformRotate(typ string) {
	var n int
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM platform_channel_items WHERE type=? AND enabled=1", typ).Scan(&n)
	if n < 2 {
		_, _ = s.DB.Exec("UPDATE channels SET rotate=0 WHERE type=?", typ)
	}
}

// PickPlatformQR 平台收银台取码: 按订单号取模确定性轮询。返回 (收款码地址, 码ID)。
func (s *Store) PickPlatformQR(orderID int64, typ string) (string, int64) {
	items, err := s.PlatformItems(typ)
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
	rotate := s.PlatformRotate(typ)
	idx := 0
	if rotate && len(enabled) >= 2 {
		idx = int(orderID % int64(len(enabled)))
	}
	pick := enabled[idx]
	_, _ = s.DB.Exec("UPDATE platform_channel_items SET last_used_at=? WHERE id=?", model.Now(), pick.ID)
	return pick.Qrcode, pick.ID
}
