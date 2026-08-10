// 配置导入导出：全量 JSON 下载与替换式导入。
package web

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// exportPage GET /admin/export
func (s *Server) exportPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "export.html", baseData("导入导出 · 智能 API 网关", "export", map[string]any{
		"Flash": s.readFlash(w, r),
	}))
}

// exportConfig GET /admin/export/download
func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.ExportAll(r.Context())
	if err != nil {
		s.setFlash(w, r, "导出失败: "+err.Error())
		http.Redirect(w, r, "/admin/export", http.StatusFound)
		return
	}
	filename := "gateway-config-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// importConfig POST /admin/import：替换式导入（清空全部配置后重建）。
func (s *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.setFlash(w, r, "解析上传失败: "+err.Error())
		http.Redirect(w, r, "/admin/export", http.StatusFound)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.setFlash(w, r, "请选择要导入的 JSON 文件")
		http.Redirect(w, r, "/admin/export", http.StatusFound)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8<<20))
	if err != nil {
		s.setFlash(w, r, "读取文件失败: "+err.Error())
		http.Redirect(w, r, "/admin/export", http.StatusFound)
		return
	}
	if err := s.store.ImportAll(r.Context(), data); err != nil {
		s.setFlash(w, r, "导入失败（已回滚）: "+err.Error())
	} else {
		s.setFlash(w, r, "导入成功：现有渠道/模型/用户/令牌已整体替换。注意：令牌明文不可恢复，需重新生成")
	}
	http.Redirect(w, r, "/admin/export", http.StatusFound)
}
