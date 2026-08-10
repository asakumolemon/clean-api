package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUserCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "admin", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("id 不应为 0")
	}

	u, err := s.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != id || u.Role != "admin" || u.PasswordHash != "hash" {
		t.Error("读取到的用户不匹配", u)
	}

	byID, err := s.GetUserByID(ctx, id)
	if err != nil || byID.Username != "admin" {
		t.Error("GetUserByID 失败", err)
	}

	if _, err := s.GetUserByUsername(ctx, "nobody"); err != ErrNotFound {
		t.Error("不存在用户应返回 ErrNotFound", err)
	}

	n, err := s.CountAdmin(ctx)
	if err != nil || n != 1 {
		t.Error("管理员数量应为 1", n, err)
	}
}

func TestTokenCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	uid, _ := s.CreateUser(ctx, "admin", "hash", "admin")
	id, err := s.CreateToken(ctx, uid, "nextchat", "hash1", []string{"deepseek-chat"}, false)
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.GetTokenByHash(ctx, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !tok.Enabled || tok.AllowAll || len(tok.ModelWhitelist) != 1 || tok.ModelWhitelist[0] != "deepseek-chat" {
		t.Error("令牌字段不匹配", tok)
	}

	// 白名单必填校验逻辑在 web 层，store 允许空列表 + allowAll
	id2, err := s.CreateToken(ctx, uid, "all", "hash2", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	tok2, _ := s.GetTokenByHash(ctx, "hash2")
	if !tok2.AllowAll {
		t.Error("allowAll 应写入")
	}

	if err := s.SetTokenEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	tok, _ = s.GetTokenByHash(ctx, "hash1")
	if tok.Enabled {
		t.Error("禁用后应返回 disabled")
	}

	if err := s.UpdateTokenWhitelist(ctx, id, []string{"gpt-4o"}, false); err != nil {
		t.Fatal(err)
	}
	tok, _ = s.GetTokenByHash(ctx, "hash1")
	if len(tok.ModelWhitelist) != 1 || tok.ModelWhitelist[0] != "gpt-4o" {
		t.Error("白名单更新失败", tok.ModelWhitelist)
	}

	tokens, err := s.ListTokens(ctx)
	if err != nil || len(tokens) != 2 {
		t.Error("列表应为 2 条", len(tokens), err)
	}

	if err := s.DeleteToken(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokenByHash(ctx, "hash1"); err != ErrNotFound {
		t.Error("吊销后应查不到", err)
	}
	_ = id2
}
