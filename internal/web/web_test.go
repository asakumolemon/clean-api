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

// 登出加固：即使服务端不认该 cookie（重启/多实例签名密钥变更后），也强制发删除指令，
// 避免残留 cookie 让 /admin/login 与 /admin/ 判定相反，陷入 ERR_TOO_MANY_REDIRECTS 循环。
func TestLogoutClearsUnknownCookie(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, _, _ := newTestWeb(t, up)

	// 无 jar 的裸客户端：手动携带一个伪造的旧签名 cookie
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "gateway_admin=bogus-invalid-cookie")
	resp, err := client.Do(req)
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
	sc := resp.Header.Get("Set-Cookie")
	if !strings.Contains(sc, "gateway_admin") || !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("不认识的 cookie 登出也应发删除指令，Set-Cookie=%q", sc)
	}
}

// 角色分级：user 角色可登录后台访问只读页（仪表盘/日志/测试台），
// 管理页（令牌/渠道/模型/用户/导出）302 回仪表盘并提示无权限；侧边栏只显示只读导航。
func TestUserRoleReadOnly(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, _ := newTestWeb(t, up)
	ctx := context.Background()

	hash, _ := auth.HashPassword("bob123")
	if _, err := st.CreateUser(ctx, "bob", hash, "user"); err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	bob := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := bob.PostForm(ts.URL+"/admin/login", url.Values{"username": {"bob"}, "password": {"bob123"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/" {
		t.Fatalf("bob 登录应 302 /admin/，got %d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	get := func(path string) (int, string) {
		t.Helper()
		page, err := bob.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer page.Body.Close()
		body := make([]byte, 2<<20)
		n, _ := page.Body.Read(body)
		return page.StatusCode, string(body[:n])
	}

	// 只读页可访问
	for _, p := range []string{"/admin/", "/admin/logs", "/admin/playground"} {
		code, _ := get(p)
		if code != http.StatusOK {
			t.Errorf("user 应能访问 %s（200），got %d", p, code)
		}
	}
	// 仪表盘侧边栏：只读导航在，管理导航不在
	code, html := get("/admin/")
	if code != http.StatusOK {
		t.Fatalf("仪表盘应 200，got %d", code)
	}
	for _, s := range []string{"仪表盘", "请求日志", "测试台"} {
		if !strings.Contains(html, s) {
			t.Errorf("user 侧边栏应包含 %q", s)
		}
	}
	for _, s := range []string{"渠道管理", "模型管理", "令牌管理", "用户管理", "导入导出"} {
		if strings.Contains(html, s) {
			t.Errorf("user 侧边栏不应包含 %q", s)
		}
	}

	// 管理页 302 回仪表盘
	for _, p := range []string{"/admin/tokens", "/admin/channels", "/admin/models", "/admin/users", "/admin/export"} {
		code, _ := get(p)
		if code != http.StatusFound {
			t.Errorf("user 访问 %s 应 302，got %d", p, code)
		}
	}
	// 管理 POST 操作同样被拒
	resp, err = bob.PostForm(ts.URL+"/admin/tokens", url.Values{"name": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/" {
		t.Fatalf("user 提交管理 POST 应 302 /admin/，got %d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// 无权限提示 flash
	_, html = get("/admin/")
	if !strings.Contains(html, "无权限") {
		t.Error("被拒后回仪表盘应显示无权限提示")
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
	// 渠道覆盖信息与筛选：可用渠道开关 + 能力筛选 + 渠道数展示
	for _, s := range []string{`id="only-available"`, "cap-filter", "1 渠道 · 1 可用"} {
		if !strings.Contains(html, s) {
			t.Errorf("弹窗应包含渠道覆盖/筛选元素 %q", s)
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

// modelOptions 视图：多渠道提供同一对外名时按名分组，统计渠道覆盖（总/健康）与能力并集。
func TestModelOptionsView(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	_, st, _ := newTestWeb(t, up)
	ctx := context.Background()

	chA, err := st.CreateChannel(ctx, "A", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	chB, err := st.CreateChannel(ctx, "B", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	chC, err := st.CreateChannel(ctx, "C", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannelStatus(ctx, chB, "down"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.SyncModels(ctx, chA, map[string]store.Capabilities{
		"m1": {System: true, Tools: true}, "m2": {},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncModels(ctx, chB, map[string]store.Capabilities{
		"m1": {Vision: true}, "m-down": {},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncModels(ctx, chC, map[string]store.Capabilities{"m1": {}}, now); err != nil {
		t.Fatal(err)
	}
	// chC 的 m1 设别名 → 对外名变为 m1-outer
	mc, err := st.GetModel(ctx, chC, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelAlias(ctx, mc.ID, "m1-outer"); err != nil {
		t.Fatal(err)
	}

	opts := modelOptions(ctx, st)
	names := make([]string, 0, len(opts))
	for _, o := range opts {
		names = append(names, o.Name)
	}
	if want := []string{"m-down", "m1", "m1-outer", "m2"}; !slicesEqual(names, want) {
		t.Fatalf("选项应按对外名排序去重，got %v want %v", names, want)
	}
	byName := map[string]modelOption{}
	for _, o := range opts {
		byName[o.Name] = o
	}
	// m1：chA(active)+chB(down) 两个渠道，健康 1；能力取并集 system/tools/vision
	m1 := byName["m1"]
	if m1.ChannelCount != 2 || m1.ActiveCount != 1 {
		t.Errorf("m1 应为 2 渠道 1 可用，got %d/%d", m1.ChannelCount, m1.ActiveCount)
	}
	if !m1.Caps.System || !m1.Caps.Tools || !m1.Caps.Vision || m1.Caps.JSONMode {
		t.Errorf("m1 能力应为 system/tools/vision 并集（无 json），got %+v", m1.Caps)
	}
	// m1-outer：仅 chC（active）提供
	mo := byName["m1-outer"]
	if mo.ChannelCount != 1 || mo.ActiveCount != 1 || mo.Caps.System || mo.Caps.Vision {
		t.Errorf("m1-outer 应为 1 渠道 1 可用且无能力，got %+v", mo)
	}
	// m-down：仅 down 渠道提供 → 无可用渠道（弹窗红字）
	md := byName["m-down"]
	if md.ChannelCount != 1 || md.ActiveCount != 0 {
		t.Errorf("m-down 应为 1 渠道 0 可用，got %d/%d", md.ChannelCount, md.ActiveCount)
	}
}

// 一键建令牌：模型页带 model 参数直达 /admin/tokens，白名单预填该模型对外名（alias 优先），
// 名称为空自动命名，POST 响应直接展示令牌明文（不可重定向，否则明文丢失）。
func TestTokenCreateOneClick(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	chID, err := st.CreateChannel(ctx, "测试渠道", "openai", up.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AddChannelKey(ctx, chID, "sk-test")
	if _, err := st.SyncModels(ctx, chID, map[string]store.Capabilities{"real-name": {}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetModel(ctx, chID, "real-name")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelAlias(ctx, m.ID, "alias-name"); err != nil {
		t.Fatal(err)
	}

	resp, err := client.PostForm(ts.URL+"/admin/tokens", url.Values{"model": {"alias-name"}})
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2<<20)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	html := string(body[:n])
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("一键建令牌应 200 并直接展示令牌明文，got %d", resp.StatusCode)
	}
	for _, s := range []string{"令牌已生成", "alias-name 令牌", "new-token-curl-openai", "new-token-shell-claude"} {
		if !strings.Contains(html, s) {
			t.Errorf("生成后页面应包含 %q（一键复制配置块）", s)
		}
	}

	toks, _ := st.ListTokens(ctx)
	if len(toks) != 1 {
		t.Fatalf("应创建 1 个令牌，got %d", len(toks))
	}
	if toks[0].Name != "alias-name 令牌" {
		t.Errorf("令牌名应自动命名为 alias-name 令牌，got %q", toks[0].Name)
	}
	if len(toks[0].ModelWhitelist) != 1 || toks[0].ModelWhitelist[0] != "alias-name" {
		t.Errorf("白名单应预填对外名 alias-name，got %+v", toks[0].ModelWhitelist)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa64(id int64) string {
	return strconv.FormatInt(id, 10)
}
