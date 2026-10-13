package service

import (
	"errors"
	"strings"
	"time"

	"kunzhifu/internal/model"
)

var (
	ErrTicketSubject = errors.New("标题不能为空且不超过 100 字")
	ErrTicketContent = errors.New("内容不能为空且不超过 2000 字")
	ErrTicketLimit   = errors.New("今日新建工单已达上限(5 个),请先等待回复或明日再试")
	ErrTicketDenied  = errors.New("工单不存在或无权操作")
	ErrTicketClosed  = errors.New("工单已关闭,如仍有问题请新建工单")
)

// 工单状态: 0 待平台回复 1 已回复待商户 2 已关闭

// CreateTicket 商户新建工单(每商户每日限 5 个,防刷)
func (s *Store) CreateTicket(userID int64, subject, content string) (int64, error) {
	subject = strings.TrimSpace(subject)
	content = strings.TrimSpace(content)
	if subject == "" || len([]rune(subject)) > 100 {
		return 0, ErrTicketSubject
	}
	if content == "" || len([]rune(content)) > 2000 {
		return 0, ErrTicketContent
	}
	lt := time.Unix(model.Now(), 0)
	dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location()).Unix()
	var n int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM tickets WHERE user_id=? AND created_at>=?", userID, dayStart).Scan(&n)
	if n >= 5 {
		return 0, ErrTicketLimit
	}
	now := model.Now()
	res, err := s.DB.Exec("INSERT INTO tickets(user_id,subject,status,created_at,updated_at) VALUES(?,?,0,?,?)",
		userID, subject, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	_, err = s.DB.Exec("INSERT INTO ticket_msgs(ticket_id,is_admin,content,created_at) VALUES(?,0,?,?)",
		id, content, now)
	return id, err
}

// MerchantTickets 商户的工单列表
func (s *Store) MerchantTickets(userID int64, limit int) ([]map[string]any, error) {
	rows, err := s.DB.Query(`SELECT id,subject,status,created_at,updated_at FROM tickets
		WHERE user_id=? ORDER BY updated_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, ca, ua int64
		var subject string
		var status int
		if rows.Scan(&id, &subject, &status, &ca, &ua) == nil {
			out = append(out, map[string]any{"ID": id, "Subject": subject, "Status": status, "CreatedAt": ca, "UpdatedAt": ua})
		}
	}
	return out, rows.Err()
}

// AdminTickets 平台工单列表(可按状态筛选,-1 全部)
func (s *Store) AdminTickets(status int, limit int) ([]map[string]any, error) {
	q := `SELECT t.id,t.subject,t.status,t.created_at,t.updated_at,u.username FROM tickets t
		JOIN users u ON u.id=t.user_id `
	var args []any
	if status >= 0 {
		q += "WHERE t.status=? "
		args = append(args, status)
	}
	q += "ORDER BY t.updated_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, ca, ua int64
		var subject, username string
		var st int
		if rows.Scan(&id, &subject, &st, &ca, &ua, &username) == nil {
			out = append(out, map[string]any{"ID": id, "Subject": subject, "Status": st,
				"CreatedAt": ca, "UpdatedAt": ua, "Username": username})
		}
	}
	return out, rows.Err()
}

// TicketOf 取工单(可带归属校验;uid<=0 跳过归属,供平台侧使用)
func (s *Store) TicketOf(id, uid int64) (subject string, status int, owner int64, err error) {
	if uid > 0 {
		err = s.DB.QueryRow("SELECT subject,status,user_id FROM tickets WHERE id=? AND user_id=?", id, uid).
			Scan(&subject, &status, &owner)
	} else {
		err = s.DB.QueryRow("SELECT subject,status,user_id FROM tickets WHERE id=?", id).
			Scan(&subject, &status, &owner)
	}
	return
}

// TicketSubject 取工单标题
func (s *Store) TicketSubject(id int64) (string, error) {
	var subject string
	err := s.DB.QueryRow("SELECT subject FROM tickets WHERE id=?", id).Scan(&subject)
	return subject, err
}

// TicketMsgs 工单会话内容
func (s *Store) TicketMsgs(ticketID int64) ([]map[string]any, error) {
	rows, err := s.DB.Query("SELECT is_admin,content,created_at FROM ticket_msgs WHERE ticket_id=? ORDER BY id ASC", ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var admin, ca int64
		var content string
		if rows.Scan(&admin, &content, &ca) == nil {
			out = append(out, map[string]any{"IsAdmin": admin == 1, "Content": content, "CreatedAt": ca})
		}
	}
	return out, rows.Err()
}

// AddTicketMsg 追加会话消息;商户回复把状态拉回待平台,平台回复置为已回复
func (s *Store) AddTicketMsg(ticketID int64, isAdmin bool, content string) error {
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > 2000 {
		return ErrTicketContent
	}
	now := model.Now()
	if _, err := s.DB.Exec("INSERT INTO ticket_msgs(ticket_id,is_admin,content,created_at) VALUES(?,?,?,?)",
		ticketID, boolToInt(isAdmin), content, now); err != nil {
		return err
	}
	st := 0
	if isAdmin {
		st = 1
	}
	_, err := s.DB.Exec("UPDATE tickets SET status=?, updated_at=? WHERE id=?", st, now, ticketID)
	return err
}

// CloseTicket 关闭工单(商户本人或平台)
func (s *Store) CloseTicket(ticketID, uid int64) error {
	if uid > 0 {
		res, err := s.DB.Exec("UPDATE tickets SET status=2, updated_at=? WHERE id=? AND user_id=?", model.Now(), ticketID, uid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrTicketDenied
		}
		return nil
	}
	_, err := s.DB.Exec("UPDATE tickets SET status=2, updated_at=? WHERE id=?", model.Now(), ticketID)
	return err
}

// OpenTicketCount 平台待处理工单数(侧栏提示用)
func (s *Store) OpenTicketCount() int64 {
	var n int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM tickets WHERE status=0").Scan(&n)
	return n
}
