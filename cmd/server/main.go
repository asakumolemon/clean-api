// 网关入口：加载配置、初始化 store/鉴权/路由、启动 HTTP 服务。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

var (
	configPath = flag.String("config", "config.json", "配置文件路径（json）")
	adminPW    = flag.String("admin-password", "", "首启管理员初始密码（也可用环境变量 GATEWAY_ADMIN_PASSWORD）")
)

func main() {
	flag.Parse()

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
	rt := router.New(st, chm, cfg.RoutingStrategy, time.Duration(cfg.DefaultTimeoutSec)*time.Second)

	r := chi.NewRouter()
	adminWeb, err := web.New(st, sessions, chm)
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
