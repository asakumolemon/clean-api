// Package web 存放管理面模板，编译期嵌入单二进制。样式走 Tailwind Play CDN（运行时加载）。
package web

import "embed"

//go:embed templates
var FS embed.FS
