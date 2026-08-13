package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
	"api-gateway/internal/channel"
	"api-gateway/internal/crypto"
	"api-gateway/internal/router"
	"api-gateway/internal/store"
)

func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "model-a"}, {"id": "model-b"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	return httptest.NewServer(mux)
}

func newTestWeb(t *testing.T, up *httptest.Server) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	hash, _ := auth.HashPassword("admin123")
	uid, err := st.CreateUser(ctx, "admin", hash, "admin")
	if err != nil || uid == 0 {
		t.Fatal("创建管理员失败", err)
	}
	am, err := auth.NewSessionManager("test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := crypto.New("test-enc-key")
	chm := channel.NewManager(st, enc, 10*time.Second)
	rt := router.New(st, chm, "random", 10*time.Second, nil)

	srv, err := New(st, am, chm, rt, "test")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	srv.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"admin123"}})
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatal("登录失败", resp.StatusCode, err)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/" {
		t.Fatalf("登录后应跳转 /admin/，got %q（可能是登录失败）", loc)
	}
	resp.Body.Close()
	return ts, st, client
}

func TestChannelProbeFlowViaWeb(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	resp, err := client.PostForm(ts.URL+"/admin/channels", url.Values{
		"name":     {"fake"},
		"base_url": {up.URL},
		"type":     {"auto"},
		"api_keys": {"sk-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("创建渠道应 302，got", resp.StatusCode)
	}

	// 轮询渠道页直到探测完成
	deadline := time.Now().Add(10 * time.Second)
	done := false
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		page, err := client.Get(ts.URL + "/admin/channels")
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 1<<20)
		n, _ := page.Body.Read(body)
		page.Body.Close()
		lastStatus = page.StatusCode
		lastBody = string(body[:n])
		if strings.Contains(lastBody, "探测完成") {
			done = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !done {
		t.Fatalf("探测未在超时内完成, status=%d body=%s", lastStatus, lastBody[:min(500, len(lastBody))])
	}

	ch, err := st.GetChannel(ctx, 1)
	if err != nil || ch.Type != "openai" {
		t.Fatal("渠道类型应识别为 openai", ch, err)
	}
	models, _ := st.ListModels(ctx)
	if len(models) != 2 {
		t.Fatalf("应同步 2 个模型，got %d", len(models))
	}
}

func TestChannelProbeFailAndManualRetry(t *testing.T) {
	// 全 404 的上游：探测失败，然后手动指定类型重试成功
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	up := httptest.NewServer(mux)
	defer up.Close()

	ts, _, client := newTestWeb(t, up)

	resp, err := client.PostForm(ts.URL+"/admin/channels", url.Values{
		"name": {"bad"}, "base_url": {up.URL}, "type": {"auto"}, "api_keys": {"sk-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(10 * time.Second)
	failed := false
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		page, _ := client.Get(ts.URL + "/admin/channels")
		body := make([]byte, 1<<20)
		n, _ := page.Body.Read(body)
		page.Body.Close()
		lastStatus = page.StatusCode
		lastBody = string(body[:n])
		if strings.Contains(lastBody, "探测失败") {
			failed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !failed {
		t.Fatalf("全 404 上游应探测失败, status=%d body=%s", lastStatus, lastBody[:min(500, len(lastBody))])
	}
}

// --- M5：日志页 / 测试台 / 用户管理 ---

func TestLogsPage(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	// 造日志数据
	for i := 0; i < 3; i++ {
		_ = st.InsertRequestLog(ctx, &store.RequestLog{
			TS: time.Now().UTC(), RequestID: "req-log-test", Model: "model-a",
			ChannelID: 1, Status: 200, LatencyMS: 12, PromptTokens: 10, CompletionTokens: 5,
		})
	}
	_ = st.InsertRequestLog(ctx, &store.RequestLog{
		TS: time.Now().UTC(), RequestID: "req-err", Model: "model-b", Status: 404, LatencyMS: 3,
	})

	page, err := client.Get(ts.URL + "/admin/logs")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2<<20)
	n, _ := page.Body.Read(body)
	page.Body.Close()
	html := string(body[:n])
	if page.StatusCode != http.StatusOK {
		t.Fatalf("日志页应 200，got %d", page.StatusCode)
	}
	if !strings.Contains(html, "req-log-test") || !strings.Contains(html, "共 4 条记录") {
		t.Error("日志页应展示日志与总数")
	}

	// 筛选：model=model-b
	page, _ = client.Get(ts.URL + "/admin/logs?model=model-b")
	body = make([]byte, 2<<20)
	n, _ = page.Body.Read(body)
	page.Body.Close()
	html = string(body[:n])
	if !strings.Contains(html, "req-err") || strings.Contains(html, "req-log-test") {
		t.Error("模型筛选应只显示 model-b 的日志")
	}
}

// 日志页内嵌的按天×模型 token 统计：汇总卡片与明细表随筛选（含日期范围）渲染。
func TestLogsPageStats(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	day1 := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)
	// 8-10：model-a × 2（pt 100/200、ct 50/80）
	for i, pt := range []int{100, 200} {
		_ = st.InsertRequestLog(ctx, &store.RequestLog{
			TS: day1, RequestID: "stat-a", Model: "model-a", Status: 200,
			PromptTokens: pt, CompletionTokens: 50 + 30*i,
		})
	}
	// 8-10：model-b × 1
	_ = st.InsertRequestLog(ctx, &store.RequestLog{
		TS: day1, RequestID: "stat-b", Model: "model-b", Status: 200,
		PromptTokens: 1000, CompletionTokens: 500,
	})
	// 8-11：model-a × 1（范围外，用于验证日期筛选）
	_ = st.InsertRequestLog(ctx, &store.RequestLog{
		TS: day2, RequestID: "stat-outside", Model: "model-a", Status: 200,
		PromptTokens: 999, CompletionTokens: 999,
	})

	get := func(qs string) string {
		t.Helper()
		page, err := client.Get(ts.URL + "/admin/logs" + qs)
		if err != nil {
			t.Fatal(err)
		}
		defer page.Body.Close()
		if page.StatusCode != http.StatusOK {
			t.Fatalf("日志页应 200，got %d", page.StatusCode)
		}
		body := make([]byte, 2<<20)
		n, _ := page.Body.Read(body)
		return string(body[:n])
	}

	// 全量：卡片合计应含全部记录；明细表出现两天两模型
	html := get("")
	for _, s := range []string{"Token 消耗统计", "model-a", "model-b", "输入 Tokens", "输出 Tokens"} {
		if !strings.Contains(html, s) {
			t.Errorf("统计区应包含 %q", s)
		}
	}
	if !strings.Contains(html, "2026-08-10") || !strings.Contains(html, "2026-08-11") {
		t.Error("统计表应包含 8-10 与 8-11 两天的分组")
	}

	// 日期筛选 8-10~8-11 含当天；表单回填
	html = get("?from=2026-08-10&to=2026-08-11")
	for _, s := range []string{`name="from" value="2026-08-10"`, `name="to" value="2026-08-11"`} {
		if !strings.Contains(html, s) {
			t.Errorf("日期筛选应回填 %q", s)
		}
	}
	// 8-10: model-a 合计 pt=300 ct=130；model-b pt=1000 ct=500；总 Tokens = 1930
	if !strings.Contains(html, "stat-outside") {
		t.Error("to 含 to 当天，8-11 日志应仍显示")
	}
	// 汇总卡片：8-10+8-11 全量 pt=2299（300+1000+999）ct=1629（130+500+999），总 Tokens = 3928
	if !strings.Contains(html, "3928") {
		t.Error("汇总卡片应显示总 Tokens 3928")
	}

	// 日期筛选 8-10 单天：排除 8-11，stat-outside 不再显示
	html = get("?from=2026-08-10&to=2026-08-10")
	if strings.Contains(html, "stat-outside") {
		t.Error("8-10 单天范围不应包含 8-11 日志")
	}
	// 单天 model-a 明细行 pt=300 ct=130 总 430
	if !strings.Contains(html, "430") {
		t.Error("8-10 model-a 总 Tokens 应为 430")
	}
}

func TestPlayground(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	// 直接建渠道+模型（type=openai，跳过探测流程）
	chID, err := st.CreateChannel(ctx, "测试渠道", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AddChannelKey(ctx, chID, "sk-test")
	_, _ = st.SyncModels(ctx, chID, map[string]store.Capabilities{"model-a": {System: true}}, time.Now().UTC())

	// 页面：模型下拉可见
	page, err := client.Get(ts.URL + "/admin/playground")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 1<<20)
	n, _ := page.Body.Read(body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body[:n]), "model-a") {
		t.Fatalf("测试台页应 200 且包含模型下拉，got %d", page.StatusCode)
	}

	// 对话：上游返回 {"choices":[]}，页面应 200 且无错误提示
	resp, err := client.PostForm(ts.URL+"/admin/playground/chat", url.Values{
		"model":   {"model-a"},
		"system":  {"你是助手"},
		"message": {"你好"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body = make([]byte, 2<<20)
	n, _ = resp.Body.Read(body)
	resp.Body.Close()
	html := string(body[:n])
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("对话应 200，got %d", resp.StatusCode)
	}
	if strings.Contains(html, "对话失败") {
		t.Error("对话不应失败：", html)
	}
}

func TestUsersPage(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)

	// 列表包含 admin
	page, err := client.Get(ts.URL + "/admin/users")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 1<<20)
	n, _ := page.Body.Read(body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body[:n]), "admin") {
		t.Fatalf("用户页应 200 且包含 admin，got %d", page.StatusCode)
	}

	// 新建用户
	resp, err := client.PostForm(ts.URL+"/admin/users", url.Values{
		"username": {"bob"}, "password": {"bob123"}, "role": {"user"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("创建用户应 302，got", resp.StatusCode)
	}
	users, _ := st.ListUsers(context.Background())
	if len(users) != 2 {
		t.Fatalf("应 2 个用户，got %d", len(users))
	}
	bob := users[1]
	if bob.Role != "user" {
		t.Error("新用户角色应为 user")
	}

	// 改角色
	resp, err = client.PostForm(ts.URL+"/admin/users/"+itoa64(bob.ID)+"/role", url.Values{"role": {"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got, _ := st.GetUserByID(context.Background(), bob.ID)
	if got.Role != "admin" {
		t.Error("角色应更新为 admin")
	}

	// 删除
	resp, err = client.PostForm(ts.URL+"/admin/users/"+itoa64(bob.ID)+"/delete", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := st.GetUserByID(context.Background(), bob.ID); err != store.ErrNotFound {
		t.Error("用户应已删除")
	}
}

// 登出：POST /admin/logout 清除会话 cookie，之后访问受保护页应跳回登录页。
func TestLogout(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, _, client := newTestWeb(t, up)

	// 登录态下受保护页可访问
	page, err := client.Get(ts.URL + "/admin/tokens")
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("登录后访问 /admin/tokens 应 200，got %d", page.StatusCode)
	}

	// 退出：POST 应 302 跳登录页
	resp, err := client.PostForm(ts.URL+"/admin/logout", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("登出应 302，got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("登出后应跳转 /admin/login，got %q", loc)
	}

	// 会话已清除：再访问受保护页应被重定向回登录页
	page, err = client.Get(ts.URL + "/admin/tokens")
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != http.StatusFound {
		t.Fatalf("登出后访问 /admin/tokens 应 302 回登录页，got %d", page.StatusCode)
	}
	if loc := page.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("登出后应跳转 /admin/login，got %q", loc)
	}
}

// 模型列表分页：每页 20 条；操作表单携带当前页，操作后仍回到该页。
func TestModelsPagePagination(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	chID, err := st.CreateChannel(ctx, "分页渠道", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AddChannelKey(ctx, chID, "sk-test")
	caps := map[string]store.Capabilities{}
	for i := 0; i < 25; i++ {
		caps[fmt.Sprintf("model-%02d", i)] = store.Capabilities{System: true}
	}
	if _, err := st.SyncModels(ctx, chID, caps, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	get := func(qs string) string {
		t.Helper()
		page, err := client.Get(ts.URL + "/admin/models" + qs)
		if err != nil {
			t.Fatal(err)
		}
		defer page.Body.Close()
		if page.StatusCode != http.StatusOK {
			t.Fatalf("模型页应 200，got %d", page.StatusCode)
		}
		body := make([]byte, 2<<20)
		n, _ := page.Body.Read(body)
		return string(body[:n])
	}

	// 第 1 页：前 20 个，有「下一页」无「上一页」
	html := get("")
	for _, s := range []string{"共 25 个模型", "第 1 / 2 页", "model-00", "model-19", "下一页"} {
		if !strings.Contains(html, s) {
			t.Errorf("第 1 页应包含 %q", s)
		}
	}
	for _, s := range []string{"model-20", "上一页"} {
		if strings.Contains(html, s) {
			t.Errorf("第 1 页不应包含 %q", s)
		}
	}

	// 第 2 页：后 5 个，有「上一页」无「下一页」
	html = get("?page=2")
	for _, s := range []string{"第 2 / 2 页", "model-20", "model-24", "上一页", `name="page" value="2"`} {
		if !strings.Contains(html, s) {
			t.Errorf("第 2 页应包含 %q", s)
		}
	}
	for _, s := range []string{"model-00", "下一页"} {
		if strings.Contains(html, s) {
			t.Errorf("第 2 页不应包含 %q", s)
		}
	}

	// toggle 带 page=2 提交 → 重定向回 /admin/models?page=2（操作不丢页码）
	resp, err := client.PostForm(ts.URL+"/admin/models/1/toggle", url.Values{"page": {"2"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("toggle 应 302，got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/models?page=2" {
		t.Fatalf("toggle 后应跳回 /admin/models?page=2，got %q", loc)
	}
}

func TestTokenCreateWithModelPicker(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	// 建渠道并同步模型，供白名单弹窗选择
	chID, err := st.CreateChannel(ctx, "测试渠道", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AddChannelKey(ctx, chID, "sk-test")
	if _, err := st.SyncModels(ctx, chID, map[string]store.Capabilities{
		"model-a": {}, "model-b": {},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 令牌页应渲染弹窗候选（模型 checkbox）
	page, err := client.Get(ts.URL + "/admin/tokens")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 1<<20)
	n, _ := page.Body.Read(body)
	page.Body.Close()
	html := string(body[:n])
	if page.StatusCode != http.StatusOK || !strings.Contains(html, "models-modal") {
		t.Fatalf("令牌页应包含多选弹窗，got %d", page.StatusCode)
	}
	for _, m := range []string{"model-a", "model-b"} {
		if !strings.Contains(html, `name="models" value="`+m+`"`) {
			t.Errorf("弹窗应包含模型 %s 的 checkbox", m)
		}
	}

	// 弹窗内模型搜索框：输入框 + 行标记 + 无匹配空态 + 过滤逻辑
	for _, s := range []string{`id="model-search"`, "model-row", "model-search-empty", "applySearch"} {
		if !strings.Contains(html, s) {
			t.Errorf("弹窗应包含搜索功能元素 %q", s)
		}
	}

	// 多值提交 models（模拟弹窗勾选两个模型）
	resp, err := client.PostForm(ts.URL+"/admin/tokens", url.Values{
		"name":   {"multi"},
		"models": {"model-a", "model-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body = make([]byte, 2<<20)
	n, _ = resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body[:n]), "multi") {
		t.Fatalf("创建后应渲染新令牌名，got %d", resp.StatusCode)
	}

	toks, _ := st.ListTokens(ctx)
	if len(toks) != 1 || len(toks[0].ModelWhitelist) != 2 {
		t.Fatalf("应创建 1 个含 2 个白名单模型的令牌，got %d 个", len(toks))
	}
	if toks[0].ModelWhitelist[0] != "model-a" || toks[0].ModelWhitelist[1] != "model-b" {
		t.Error("白名单应为 model-a/model-b", toks[0].ModelWhitelist)
	}
}

func itoa64(id int64) string {
	return strconv.FormatInt(id, 10)
}
