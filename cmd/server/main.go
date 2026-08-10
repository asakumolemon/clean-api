// 网关入口：加载配置、初始化 store/鉴权/路由、启动 HTTP 服务。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/api"
	"api-gateway/internal/auth"
	"api-gateway/internal/channel"
	"api-gateway/internal/config"
	"api-gateway/internal/crypto"
	"api-gateway/internal/router"
	"api-gateway/internal/store"
	"api-gateway/internal/web"
)

// version 构建版本（-ldflags "-X main.version=..." 注入，默认 dev）。
var version = "dev"

var (
	configPath = flag.String("config", "config.json", "配置文件路径（json）")
	adminPW    = flag.String("admin-password", "", "首启管理员初始密码（也可用环境变量 GATEWAY_ADMIN_PASSWORD）")
	showVer    = flag.Bool("version", false, "打印版本信息后退出")
)

func main() {
	flag.Parse()

	if *showVer {
		fmt.Printf("api-gateway %s\n", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("加载配置失败", err)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	if cfg.EncKey == "" {
		slog.Warn("未设置 GATEWAY_ENC_KEY：上游 API key 将明文存储，生产环境请务必配置")
	}
	if cfg.SessionSecret == "" {
		slog.Warn("未设置 session_secret：已生成随机密钥，重启后所有管理面登录态失效")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fatal("初始化数据库失败", err)
	}
	defer st.Close()

	// 密钥缺失告警（M6）：库中已有加密 key 但未配置 GATEWAY_ENC_KEY → 这些 key 将无法解密使用。
	if cfg.EncKey == "" {
		if n, err := st.CountEncryptedKeys(context.Background()); err == nil && n > 0 {
			slog.Error("检测到数据库中已有加密的渠道 key 但未配置 GATEWAY_ENC_KEY：这些 key 将无法解密，请求会失败。请配置与加密时相同的密钥", "加密 key 数", n)
		}
	}

	if err := ensureAdmin(context.Background(), st, cfg, *adminPW); err != nil {
		fatal("初始化管理员失败", err)
	}

	sessions, err := auth.NewSessionManager(cfg.SessionSecret, cfg.SessionSecure)
	if err != nil {
		fatal("初始化会话失败", err)
	}

	enc, err := crypto.New(cfg.EncKey)
	if err != nil {
		fatal("初始化加密失败", err)
	}
	chm := channel.NewManager(st, enc, time.Duration(cfg.DefaultTimeoutSec)*time.Second)
	chm.SetCooldown(time.Duration(cfg.KeyCooldownSec) * time.Second)
	chm.SetProbeCapabilities(cfg.ProbeCapabilities)
	rt := router.New(st, chm, cfg.RoutingStrategy, time.Duration(cfg.DefaultTimeoutSec)*time.Second, cfg.ModelRedirects)

	r := chi.NewRouter()
	r.Use(recoverMW)
	adminWeb, err := web.New(st, sessions, chm, rt, version)
	if err != nil {
		fatal("加载管理面模板失败", err)
	}
	adminWeb.Mount(r)

	apiServer := api.New(st, rt)
	r.Route("/v1", func(r chi.Router) {
		r.Use(sessions.APIAuth(st))
		r.Get("/models", apiServer.Models)
		r.Post("/chat/completions", apiServer.ChatCompletions)
		r.Post("/responses", apiServer.Responses)
		r.Post("/messages", apiServer.Messages)
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 渠道健康检查（M5）：定时巡检，连续失败标记 down，恢复自动回 active。
	if cfg.HealthCheckEnabled {
		hc := channel.NewHealthChecker(st, chm, time.Duration(cfg.DefaultTimeoutSec)*time.Second, cfg.HealthCheckMaxFailures)
		go hc.Start(ctx, time.Duration(cfg.HealthCheckIntervalSec)*time.Second)
		slog.Info("渠道健康检查已开启", "间隔秒", cfg.HealthCheckIntervalSec, "最大连续失败", cfg.HealthCheckMaxFailures)
	}

	// 请求日志保留清理（M5）：启动时 + 每小时执行一次。
	go func() {
		retention := time.Duration(cfg.LogRetentionDays) * 24 * time.Hour
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		clean := func() {
			before := time.Now().UTC().Add(-retention)
			n, err := st.DeleteRequestLogsBefore(context.Background(), before)
			if err != nil {
				slog.Error("清理请求日志失败", "error", err)
			} else if n > 0 {
				slog.Info("已清理过期请求日志", "条数", n, "保留天数", cfg.LogRetentionDays)
			}
		}
		clean()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				clean()
			}
		}
	}()

	go func() {
		slog.Info("网关已启动", "addr", cfg.Addr, "db", cfg.DBPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("HTTP 服务异常退出", err)
		}
	}()

	<-ctx.Done()
	slog.Info("收到退出信号，开始优雅关闭")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// ensureAdmin 首次启动自动创建管理员账号。
func ensureAdmin(ctx context.Context, st *store.Store, cfg *config.Config, pwFlag string) error {
	n, err := st.CountAdmin(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	pw := cfg.AdminPassword
	if pw == "" {
		pw = pwFlag
	}
	generated := ""
	if pw == "" {
		pw, generated = randomPassword(16)
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, cfg.AdminUsername, hash, "admin"); err != nil {
		return err
	}
	if generated != "" {
		slog.Warn("已自动生成管理员初始密码，请立即登录并修改", "username", cfg.AdminUsername, "password", pw)
	} else {
		slog.Info("已创建管理员账号", "username", cfg.AdminUsername, "来源", "配置/环境变量/参数")
	}
	return nil
}

func randomPassword(n int) (string, string) {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	pw := base64.RawURLEncoding.EncodeToString(b)[:n]
	return pw, pw
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}

// recoverMW panic 恢复中间件（M6）：记录堆栈日志，未写响应头时返回 500 JSON，
// 避免单个请求 panic 导致整个进程崩溃。
func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("请求处理 panic，已恢复", "panic", rec, "path", r.URL.Path, "method", r.Method, "stack", string(debug.Stack()))
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"message": "internal server error",
						"type":    "internal_error",
					},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
