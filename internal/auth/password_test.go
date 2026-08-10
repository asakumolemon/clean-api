package auth

import (
	"testing"

	"api-gateway/internal/store"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("secret123")
	if err != nil {
		t.Fatal(err)
	}
	if h == "" {
		t.Fatal("hash 为空")
	}
	if !CheckPassword(h, "secret123") {
		t.Error("正确密码应通过校验")
	}
	if CheckPassword(h, "wrong") {
		t.Error("错误密码不应通过校验")
	}
}

func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("两次生成的令牌不应相同")
	}
	if len(a) < 20 {
		t.Error("令牌太短", len(a))
	}
	if HashToken(a) == a {
		t.Error("哈希不应等于明文")
	}
	if HashToken(a) != HashToken(a) {
		t.Error("同一令牌哈希应确定")
	}
}

func TestCheckModelAllowed(t *testing.T) {
	whitelist := &store.Token{ModelWhitelist: []string{"deepseek-chat", "gpt-4o"}}

	if !CheckModelAllowed(whitelist, "deepseek-chat") {
		t.Error("白名单内模型应放行")
	}
	if CheckModelAllowed(whitelist, "claude-3") {
		t.Error("白名单外模型应拒绝")
	}
	if CheckModelAllowed(whitelist, "") {
		t.Error("空模型应拒绝")
	}
	if CheckModelAllowed(nil, "deepseek-chat") {
		t.Error("nil 令牌应拒绝")
	}

	allowAll := &store.Token{AllowAll: true}
	if !CheckModelAllowed(allowAll, "anything-else") {
		t.Error("勾选允许全部后任意模型应放行")
	}
}
