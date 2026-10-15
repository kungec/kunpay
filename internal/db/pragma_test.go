package db

import "testing"

func TestSynchronousPragma(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"FULL", "2"}, {"NORMAL", "1"}, {"", "1"}, {"BOGUS", "1"},
	} {
		d, err := Open(t.TempDir()+"/t.db", tc.mode)
		if err != nil {
			t.Fatalf("open %s: %v", tc.mode, err)
		}
		var got string
		if err := d.QueryRow("PRAGMA synchronous").Scan(&got); err != nil {
			t.Fatalf("pragma: %v", err)
		}
		d.Close()
		if got != tc.want {
			t.Errorf("db-sync=%q → synchronous=%s, 期望 %s", tc.mode, got, tc.want)
		}
	}
}
