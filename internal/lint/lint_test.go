// Package lint 以 AST 巡检守护本项目的两条硬性架构红线(仅静态启发式,作回归绊线用):
//
//  1. 事务内禁止调用 s.DB.*: 连接池 SetMaxOpenConns(1),事务持连接期间再查 s.DB 会自死锁
//     (真实事故: 管理员退款在 tx 内调 UserByPid,请求挂死)。约定写法: 事务外预读,事务内只用 tx。
//  2. 启发式说明: 按「Begin 行号 < s.DB 调用行号 < 首个 Commit 行号」的词法顺序判定,
//     不做数据流分析;error 分支里 Rollback 后立即 return 的既有写法不会误报。
package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoSDBInsideTx(t *testing.T) {
	root, _ := filepath.Abs("../..")
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			// 根目录名为 ".."(以点开头)不能跳过,只跳子目录里的隐藏目录与产物目录
			if path != root && (name == "dist" || name == "monitor-android" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var beginLine, commitLine int
			var sdbCalls []int
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						name := sel.Sel.Name
						line := fset.Position(x.Pos()).Line
						switch name {
						case "Begin":
							if beginLine == 0 {
								beginLine = line
							}
						case "Commit":
							if commitLine == 0 {
								commitLine = line
							}
						}
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "DB" {
						if ident, ok := x.X.(*ast.Ident); ok && ident.Name != "tx" {
							sdbCalls = append(sdbCalls, fset.Position(x.Pos()).Line)
						}
					}
				}
				return true
			})
			if beginLine == 0 || commitLine == 0 {
				continue
			}
			for _, ln := range sdbCalls {
				if ln > beginLine && ln < commitLine {
					pos := fset.Position(fn.Pos())
					hits = append(hits, pos.Filename+":"+itoa(ln)+" ("+fn.Name.Name+" 内 Begin@"+itoa(beginLine)+" Commit@"+itoa(commitLine)+")")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, h := range hits {
		t.Errorf("事务内调用 s.DB.*(单连接池自死锁风险): %s", h)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestNoSDBInsideRowsLoop 巡检规则二: 在 rows.Next() 游标循环体内调用 s.DB.*。
// 连接池 SetMaxOpenConns(1),游标未关闭期间再查 s.DB 会等待同一个连接 → 自死锁
// (真实事故: 早期渠道统计页在 rows 循环内 QueryRow,一访问就挂)。
// 约定写法: 先 rows 循环收集 ID/数据,Close 之后再做逐条查询(先收集后处理)。
func TestNoSDBInsideRowsLoop(t *testing.T) {
	root, _ := filepath.Abs("../..")
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if path != root && (name == "dist" || name == "monitor-android" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		isRowsNextCond := func(n ast.Node) bool {
			cond, ok := n.(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := cond.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Next" {
				return false
			}
			ident, ok := sel.X.(*ast.Ident)
			return ok && strings.Contains(strings.ToLower(ident.Name), "row")
		}
		collectSDB := func(body *ast.BlockStmt, pos token.Pos) []int {
			var out []int
			ast.Inspect(body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "DB" {
					if ident, ok := sel.X.(*ast.Ident); ok && ident.Name != "tx" {
						out = append(out, fset.Position(pos).Line)
					}
				}
				return true
			})
			return out
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lo, ok := n.(*ast.ForStmt)
				if !ok || !isRowsNextCond(lo.Cond) {
					return true
				}
				// 游标循环体内出现 s.DB.* 即命中(含更深层嵌套)
				inCursor := false
				line := 0
				ast.Inspect(lo.Body, func(m ast.Node) bool {
					if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "DB" {
						if ident, ok := sel.X.(*ast.Ident); ok && ident.Name != "tx" {
							inCursor = true
							line = fset.Position(m.Pos()).Line
						}
					}
					return true
				})
				if inCursor {
					hits = append(hits, fset.Position(fn.Pos()).Filename+":"+itoa(line)+
						" ("+fn.Name.Name+" 的 rows.Next 循环内)")
				}
				return true
			})
			_ = collectSDB
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, h := range hits {
		t.Errorf("rows.Next 游标内查询 s.DB.*(单连接池自死锁风险): %s", h)
	}
}
