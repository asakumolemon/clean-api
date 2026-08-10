// Package web 存放管理面静态资源（模板 + CSS/JS），编译期嵌入单二进制。
package web

import "embed"

//go:embed templates static
var FS embed.FS
