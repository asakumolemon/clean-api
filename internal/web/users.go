// 用户管理页：列表、新增、改角色、重置密码、删除（删除需先删其令牌）。
package web

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
	"api-gateway/internal/store"
)

// userView 用户行视图（附带令牌数）。
type userView struct {
	store.User
	TokenCount int
}

// usersPage GET /admin/users
func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, _ := s.store.ListUsers(ctx)
	views := make([]userView, 0, len(users))
	for _, u := range users {
		tokens, _ := s.store.ListTokensByUser(ctx, u.ID)
		views = append(views, userView{User: u, TokenCount: len(tokens)})
	}
	s.render(w, r, "users.html", baseData("用户管理 · 智能 API 网关", "users", map[string]any{
		"Flash": s.readFlash(w, r),
		"Users": views,
	}))
}

// createUser POST /admin/users
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	role := r.FormValue("role")
	if role != "admin" && role != "user" {
		role = "user"
	}
	if username == "" || password == "" {
		s.setFlash(w, r, "用户名与密码不能为空")
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.setFlash(w, r, "密码哈希失败: "+err.Error())
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	if _, err := s.store.CreateUser(r.Context(), username, hash, role); err != nil {
		s.setFlash(w, r, "创建用户失败: "+err.Error())
	} else {
		s.setFlash(w, r, "用户已创建："+username)
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

// setUserRole POST /admin/users/{id}/role
func (s *Server) setUserRole(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	role := r.FormValue("role")
	if role != "admin" && role != "user" {
		s.setFlash(w, r, "无效角色")
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	if err := s.store.UpdateUserRole(r.Context(), id, role); err != nil {
		s.setFlash(w, r, "修改角色失败: "+err.Error())
	} else {
		s.setFlash(w, r, "角色已更新")
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

// resetUserPassword POST /admin/users/{id}/password
func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	pw := r.FormValue("password")
	if pw == "" {
		s.setFlash(w, r, "新密码不能为空")
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		s.setFlash(w, r, "密码哈希失败: "+err.Error())
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), id, hash); err != nil {
		s.setFlash(w, r, "重置密码失败: "+err.Error())
	} else {
		s.setFlash(w, r, "密码已重置")
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}

// deleteUser POST /admin/users/{id}/delete：禁止删除自己（避免锁死管理面）。
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	me := s.currentUser(w, r)
	if me != nil && me.ID == id {
		s.setFlash(w, r, "不能删除当前登录账号")
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		s.setFlash(w, r, "删除用户失败: "+err.Error())
	} else {
		s.setFlash(w, r, "用户已删除（含其全部令牌）")
	}
	http.Redirect(w, r, "/admin/users", http.StatusFound)
}
