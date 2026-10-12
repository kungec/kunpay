// Package webfs 内嵌全部前端资源,保证单二进制可直接部署。
package webfs

import (
	"embed"
	"io/fs"
)

//go:embed all:templates all:static
var embedded embed.FS

// Templates 返回 templates 目录的文件系统
func Templates() fs.FS {
	sub, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// Static 返回 static 目录的文件系统
func Static() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
