package service

import (
	"kunzhifu/internal/model"
)

// PushMessage 向商户推送一条站内消息
func (s *Store) PushMessage(userID int64, msgType, title, content string) {
	_, err := s.DB.Exec("INSERT INTO messages(user_id,type,title,content,read,created_at) VALUES(?,?,?,?,0,?)",
		userID, msgType, title, content, model.Now())
	if err != nil {
		return
	}
}

// UserMessages 商户消息列表(最新 50 条)
func (s *Store) UserMessages(userID int64) []map[string]any {
	rows, err := s.DB.Query("SELECT id,type,title,content,read,created_at FROM messages WHERE user_id=? ORDER BY id DESC LIMIT 50", userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, createdAt int64
		var msgType, title, content string
		var read int
		if rows.Scan(&id, &msgType, &title, &content, &read, &createdAt) == nil {
			out = append(out, map[string]any{
				"ID": id, "Type": msgType, "Title": title,
				"Content": content, "Read": read == 1, "CreatedAt": createdAt,
			})
		}
	}
	return out
}

// UnreadMessageCount 商户未读消息数
func (s *Store) UnreadMessageCount(userID int64) int64 {
	var n int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE user_id=? AND read=0", userID).Scan(&n)
	return n
}

// MarkMessagesRead 标记商户所有消息为已读
func (s *Store) MarkMessagesRead(userID int64) {
	s.DB.Exec("UPDATE messages SET read=1 WHERE user_id=? AND read=0", userID)
}
