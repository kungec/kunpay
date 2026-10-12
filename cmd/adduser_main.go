package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	db, _ := sql.Open("sqlite", "/tmp/kunpay-e2e.db")
	defer db.Close()
	now := time.Now().Unix()
	hash := ""
	db.QueryRow("SELECT password_hash FROM users WHERE username='demo'").Scan(&hash)
	for _, u := range []struct{ name string; pid int }{{"mchA", 1002}, {"mchB", 1003}} {
		_, err := db.Exec(
			"INSERT OR IGNORE INTO users(username,email,password_hash,role,status,pid,skey,created_at) VALUES(?,?,?,?,1,?,?,?)",
			u.name, u.name+"@t.com", hash, u.pid, "test"+fmt.Sprint(u.pid), now)
		fmt.Println(u.name, "err:", err)
	}
}
