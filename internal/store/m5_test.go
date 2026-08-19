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

// LogUsageStats 按天×模型聚合请求数与 token 用量；时间筛选从/到边界正确。
func TestLogUsageStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 用固定日期而非 Now，保证 date(ts) 分组可预测。
	day1 := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 10, 23, 59, 0, 0, time.UTC)
	day3 := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

	insertLogs(t, s, 3, "deepseek-chat", 200, day1) // 3 次，pt 0,1,2 / ct 0,2,4
	insertLogs(t, s, 2, "gpt-4o", 200, day1)        // 2 次
	insertLogs(t, s, 2, "deepseek-chat", 404, day2) // 同模型跨天，应与 day1 分开分组
	insertLogs(t, s, 1, "gpt-4o", 200, day3)        // 次日 00:00

	// 1) 全量：按 day×model 分组与合计
	stats, err := s.LogUsageStats(ctx, LogFilter{}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]int64{ // day|model → {requests, prompt, completion}
		"2026-08-09|deepseek-chat": {3, 0 + 1 + 2, 0 + 2 + 4},
		"2026-08-09|gpt-4o":        {2, 0 + 1, 0 + 2},
		"2026-08-10|deepseek-chat": {2, 0 + 1, 0 + 2},
		"2026-08-11|gpt-4o":        {1, 0, 0},
	}
	if len(stats) != len(want) {
		t.Fatalf("分组行数应为 %d，got %d: %+v", len(want), len(stats), stats)
	}
	got := map[string][3]int64{}
	for _, u := range stats {
		got[u.Day+"|"+u.Model] = [3]int64{int64(u.Requests), u.PromptTokens, u.CompletionTokens}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s 应为 %v，got %v", k, v, got[k])
		}
	}

	// 2) 时间范围：from 排除更早日期、to 含 to 当天（To 为排他值，传 to 次日零点）
	from := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC) // 排他：ts < 8-11 零点 ⇒ 含 8-10 整天
	stats, err = s.LogUsageStats(ctx, LogFilter{From: &from, To: &to}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Day != "2026-08-10" || stats[0].Model != "deepseek-chat" || stats[0].Requests != 2 {
		t.Fatalf("8-10 范围应只剩 deepseek-chat 2 次，got %+v", stats)
	}

	// 3) 组合现有筛选仍生效：deepseek+2xx+from(8-09 零点起) → 只剩 8-09 的 3 次（8-10 的 deepseek 是 404 被滤掉）
	from = time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	stats, err = s.LogUsageStats(ctx, LogFilter{Model: "deepseek", Status: "2xx", From: &from}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Day != "2026-08-09" || stats[0].Requests != 3 {
		t.Fatalf("deepseek+2xx+from 应只余 8-09 的 3 次，got %+v", stats)
	}
}

// LogUsageStats 按传入时区（+8）分桶：UTC 16:30 → 本地跨日归 8-11。
func TestLogUsageStatsLocation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 写入 UTC 2026-08-10 16:30 → +8 本地 8-11 00:30，应归到 8-11
	insertLogs(t, s, 1, "deepseek-chat", 200, time.Date(2026, 8, 10, 16, 30, 0, 0, time.UTC))
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.LogUsageStats(ctx, LogFilter{}, shanghai)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Day != "2026-08-11" || stats[0].Model != "deepseek-chat" {
		t.Fatalf("+8 时区应把 8-10 16:30 UTC 归入 8-11，got %+v", stats)
	}
	// UTC 时区则仍是 8-10
	stats, err = s.LogUsageStats(ctx, LogFilter{}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Day != "2026-08-10" {
		t.Fatalf("UTC 时区应归 8-10，got %+v", stats)
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

// cache_hit 列（M7 后响应缓存命中标记）：写入/读取往返、CountCacheHits 计数（含筛选）、
// LogUsageStats 行内 CacheHits 聚合。
func TestRequestLogCacheHit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	hit := func(model string) {
		t.Helper()
		l := &RequestLog{TS: now, RequestID: "req-hit", Model: model, Status: 200, CacheHit: true, PromptTokens: 5, CompletionTokens: 3}
		if err := s.InsertRequestLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	miss := func(model string) {
		t.Helper()
		l := &RequestLog{TS: now, RequestID: "req-miss", Model: model, Status: 200}
		if err := s.InsertRequestLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	hit("deepseek-chat")
	miss("deepseek-chat")
	hit("gpt-4o")

	// 读取：CacheHit 往返正确
	logs, err := s.ListRequestLogs(ctx, LogFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits, missN := 0, 0
	for _, l := range logs {
		if l.CacheHit {
			hits++
		} else {
			missN++
		}
	}
	if hits != 2 || missN != 1 {
		t.Errorf("命中/未命中应为 2/1，got %d/%d", hits, missN)
	}

	// CountCacheHits：全量 + 带筛选（命中且匹配模型）
	n, _ := s.CountCacheHits(ctx, LogFilter{})
	if n != 2 {
		t.Errorf("全部命中应为 2，got %d", n)
	}
	n, _ = s.CountCacheHits(ctx, LogFilter{Model: "deepseek"})
	if n != 1 {
		t.Errorf("模型筛选命中应为 1，got %d", n)
	}

	// LogUsageStats：同日同模型分组，行内 CacheHits 正确
	stats, err := s.LogUsageStats(ctx, LogFilter{}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]int{}
	for _, st := range stats {
		byModel[st.Model] = st.CacheHits
	}
	if byModel["deepseek-chat"] != 1 || byModel["gpt-4o"] != 1 {
		t.Errorf("按模型命中数错误: %+v", byModel)
	}
}

// ensureColumn 补列幂等：重复执行不报错，列存在（migrate 已建 cache_hit，此处验证可重复补）。
func TestEnsureColumnIdempotent(t *testing.T) {
	s := newTestStore(t)
	// migrate 已带 cache_hit 列，重复补列应幂等成功
	if err := s.ensureColumn("request_logs", "cache_hit", "cache_hit INTEGER DEFAULT 0"); err != nil {
		t.Fatalf("重复补列应成功: %v", err)
	}
	// 补一个不存在的新列，再补一次验证幂等
	if err := s.ensureColumn("request_logs", "cache_mark", "cache_mark INTEGER DEFAULT 0"); err != nil {
		t.Fatalf("补新列应成功: %v", err)
	}
	if err := s.ensureColumn("request_logs", "cache_mark", "cache_mark INTEGER DEFAULT 0"); err != nil {
		t.Fatalf("再次补列应幂等: %v", err)
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
	t1, _ := s.CreateToken(ctx, uid, "t1", "kh1", []string{"deepseek-chat"}, false)
	if err := s.SetTokenGroup(ctx, t1, "生产"); err != nil {
		t.Fatal(err)
	}
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
	for _, want := range []string{`"username": "admin"`, `"key_enc": "enc:abc"`, `"deepseek-chat"`, `"model_whitelist"`, `"group": "生产"`} {
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
	for _, tk := range tokens {
		if tk.Name == "t1" && tk.Group != "生产" {
			t.Errorf("t1 恢复后分组应为「生产」，got %q", tk.Group)
		}
		if tk.Name == "t2" && tk.Group != "" {
			t.Errorf("t2 恢复后分组应为空，got %q", tk.Group)
		}
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

func TestRequestLogsTokenSnapshotAndFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "log-user", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	prodID, err := s.CreateToken(ctx, uid, "同名令牌", "log-prod", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTokenGroup(ctx, prodID, "生产"); err != nil {
		t.Fatal(err)
	}
	defaultID, err := s.CreateToken(ctx, uid, "同名令牌", "log-default", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, l := range []*RequestLog{
		{TS: now, RequestID: "prod", TokenID: prodID, TokenName: "同名令牌", TokenGroup: "生产", Model: "model-a", Status: 200, PromptTokens: 10, CompletionTokens: 5, CacheHit: true},
		{TS: now, RequestID: "default", TokenID: defaultID, TokenName: "同名令牌", Model: "model-a", Status: 200, PromptTokens: 3, CompletionTokens: 2},
	} {
		if err := s.InsertRequestLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := s.ListRequestLogs(ctx, LogFilter{TokenID: prodID}, 10, 0)
	if err != nil || len(logs) != 1 || logs[0].RequestID != "prod" || logs[0].TokenGroup != "生产" {
		t.Fatalf("按令牌 ID 应准确筛出生产日志，logs=%+v err=%v", logs, err)
	}
	if n, _ := s.CountRequestLogs(ctx, LogFilter{TokenGroup: "生产"}); n != 1 {
		t.Errorf("生产分组应为 1，got %d", n)
	}
	if n, _ := s.CountRequestLogs(ctx, LogFilter{TokenGroup: DefaultTokenGroupFilter}); n != 1 {
		t.Errorf("默认分组应为 1，got %d", n)
	}
	if n, _ := s.CountCacheHits(ctx, LogFilter{TokenID: prodID, TokenGroup: "生产"}); n != 1 {
		t.Errorf("组合筛选缓存命中应为 1，got %d", n)
	}
	stats, err := s.LogUsageStats(ctx, LogFilter{}, time.UTC)
	if err != nil || len(stats) != 2 {
		t.Fatalf("同模型不同令牌应拆为两行，stats=%+v err=%v", stats, err)
	}
	for _, stat := range stats {
		if stat.TokenID == prodID && (stat.TokenName != "同名令牌" || stat.TokenGroup != "生产" || stat.TotalTokens != 15) {
			t.Errorf("生产令牌快照或用量错误：%+v", stat)
		}
	}
}
