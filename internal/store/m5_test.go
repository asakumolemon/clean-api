package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// --- request_logs ---

func insertLogs(t *testing.T, s *Store, n int, model string, status int, ts time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		l := &RequestLog{
			TS: ts, RequestID: "req-" + model, Model: model, Status: status,
			PromptTokens: i, CompletionTokens: i * 2,
		}
		if err := s.InsertRequestLog(context.Background(), l); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRequestLogsCRUDAndFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertLogs(t, s, 3, "deepseek-chat", 200, now)
	insertLogs(t, s, 2, "gpt-4o", 404, now)
	insertLogs(t, s, 2, "deepseek-chat", 502, now)

	total, _ := s.CountRequestLogsTotal(ctx)
	if total != 7 {
		t.Fatalf("总数应为 7，got %d", total)
	}
	// 模型筛选（模糊）
	n, _ := s.CountRequestLogs(ctx, LogFilter{Model: "deepseek"})
	if n != 5 {
		t.Error("模型筛选应为 5，got", n)
	}
	// 状态筛选
	n, _ = s.CountRequestLogs(ctx, LogFilter{Status: "4xx"})
	if n != 2 {
		t.Error("4xx 筛选应为 2，got", n)
	}
	n, _ = s.CountRequestLogs(ctx, LogFilter{Status: "5xx"})
	if n != 2 {
		t.Error("5xx 筛选应为 2，got", n)
	}
	// 组合筛选
	n, _ = s.CountRequestLogs(ctx, LogFilter{Model: "deepseek", Status: "2xx"})
	if n != 3 {
		t.Error("组合筛选应为 3，got", n)
	}
	// 分页：倒序取 3 条（最新在前）
	logs, err := s.ListRequestLogs(ctx, LogFilter{}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("分页应返回 3 条，got %d", len(logs))
	}
	if logs[0].ID < logs[2].ID {
		t.Error("应按 id 倒序")
	}
	// 第二页
	logs2, _ := s.ListRequestLogs(ctx, LogFilter{}, 3, 3)
	if len(logs2) != 3 {
		t.Errorf("第二页应 3 条，got %d", len(logs2))
	}
}

func TestRequestLogsCleanup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	insertLogs(t, s, 3, "old", 200, time.Now().UTC().Add(-48*time.Hour))
	insertLogs(t, s, 2, "new", 200, time.Now().UTC())

	n, err := s.DeleteRequestLogsBefore(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("应删除 3 条过期日志，got %d", n)
	}
	total, _ := s.CountRequestLogsTotal(ctx)
	if total != 2 {
		t.Errorf("剩余应为 2 条，got %d", total)
	}
}

// --- users 扩展 ---

func TestUserRolePasswordDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "bob", "hash1", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserRole(ctx, uid, "admin"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUserByID(ctx, uid)
	if u.Role != "admin" {
		t.Error("角色应更新为 admin")
	}
	if err := s.UpdateUserPassword(ctx, uid, "hash2"); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUserByID(ctx, uid)
	if u.PasswordHash != "hash2" {
		t.Error("密码应已更新")
	}
	// 给用户建令牌，删除用户应级联删除令牌
	if _, err := s.CreateToken(ctx, uid, "t1", "hash-t", []string{"m"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserByID(ctx, uid); err != ErrNotFound {
		t.Error("用户应已删除")
	}
	tokens, _ := s.ListTokens(ctx)
	if len(tokens) != 0 {
		t.Errorf("用户令牌应级联删除，got %d", len(tokens))
	}
}

// --- 导入导出 ---

func TestExportImportRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 造数据
	uid, _ := s.CreateUser(ctx, "admin", "hash-a", "admin")
	uid2, _ := s.CreateUser(ctx, "bob", "hash-b", "user")
	_, _ = s.CreateToken(ctx, uid, "t1", "kh1", []string{"deepseek-chat"}, false)
	_, _ = s.CreateToken(ctx, uid2, "t2", "kh2", nil, true)
	chID, _ := s.CreateChannel(ctx, "deepseek 主号", "openai", "https://api.deepseek.com")
	_, _ = s.AddChannelKey(ctx, chID, "enc:abc")
	_, _ = s.SyncModels(ctx, chID, map[string]Capabilities{
		"deepseek-chat": {System: true, Tools: true},
		"ds-coder":      {System: true},
	}, time.Now().UTC())

	data, err := s.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 导出文件应包含关键内容
	js := string(data)
	for _, want := range []string{`"username": "admin"`, `"key_enc": "enc:abc"`, `"deepseek-chat"`, `"model_whitelist"`} {
		if !strings.Contains(js, want) {
			t.Errorf("导出文件缺少 %s", want)
		}
	}

	// 清空后导入
	if err := s.ImportAll(ctx, data); err != nil {
		t.Fatal(err)
	}
	users, _ := s.ListUsers(ctx)
	if len(users) != 2 || users[0].Username != "admin" {
		t.Errorf("用户应恢复 2 个，got %+v", users)
	}
	tokens, _ := s.ListTokens(ctx)
	if len(tokens) != 2 {
		t.Errorf("令牌应恢复 2 个，got %d", len(tokens))
	}
	channels, _ := s.ListChannels(ctx)
	if len(channels) != 1 || channels[0].BaseURL != "https://api.deepseek.com" {
		t.Errorf("渠道应恢复，got %+v", channels)
	}
	keys, _ := s.ListChannelKeys(ctx, chID)
	if len(keys) != 1 || keys[0].KeyEnc != "enc:abc" {
		t.Errorf("渠道 key 应恢复，got %+v", keys)
	}
	models, _ := s.ListModelsByChannel(ctx, chID)
	if len(models) != 2 || !models[0].Capabilities.Tools {
		t.Errorf("模型与能力应恢复，got %+v", models)
	}
	// 幂等：id 保持原值
	if users[0].ID != uid {
		t.Error("用户 id 应保持原始值")
	}
}

// 导入非法文件应报错且不破坏现有数据。
func TestImportInvalid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "admin", "h", "admin")

	if err := s.ImportAll(ctx, []byte(`not json`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if err := s.ImportAll(ctx, []byte(`{"version":99}`)); err == nil {
		t.Fatal("未知版本应报错")
	}
	// 现有数据不受影响
	if _, err := s.GetUserByID(ctx, uid); err != nil {
		t.Error("导入失败不应破坏现有数据")
	}
}

// CountEncryptedKeys：统计 enc: 前缀的密文数量。
func TestCountEncryptedKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	chID, _ := s.CreateChannel(ctx, "渠道", "openai", "https://example.com")
	if n, _ := s.CountEncryptedKeys(ctx); n != 0 {
		t.Fatalf("初始应为 0，got %d", n)
	}
	_, _ = s.AddChannelKey(ctx, chID, "enc:abc")
	_, _ = s.AddChannelKey(ctx, chID, "plain-key")
	_, _ = s.AddChannelKey(ctx, chID, "enc:def")
	n, err := s.CountEncryptedKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("应为 2 个加密 key，got %d", n)
	}
}
