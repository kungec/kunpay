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
