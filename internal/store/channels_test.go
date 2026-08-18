package store

import (
	"context"
	"testing"
	"time"
)

func TestChannelCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, err := s.CreateChannel(ctx, "deepseek", "auto", "https://api.deepseek.com")
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("id 不应为 0")
	}

	ch, err := s.GetChannel(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name != "deepseek" || ch.Status != "active" || ch.Weight != 1 || ch.BalanceStrategy != "random" {
		t.Error("渠道默认字段不正确", ch)
	}

	if err := s.UpdateChannel(ctx, id, "deepseek2", "openai", "https://api.deepseek.com", "active", 3, "round_robin"); err != nil {
		t.Fatal(err)
	}
	ch, _ = s.GetChannel(ctx, id)
	if ch.Type != "openai" || ch.Weight != 3 || ch.BalanceStrategy != "round_robin" {
		t.Error("更新渠道失败", ch)
	}

	if err := s.SetChannelStatus(ctx, id, "disabled"); err != nil {
		t.Fatal(err)
	}
	ch, _ = s.GetChannel(ctx, id)
	if ch.Status != "disabled" {
		t.Error("状态更新失败")
	}

	if err := s.DeleteChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetChannel(ctx, id); err != ErrNotFound {
		t.Error("删除后应查不到", err)
	}
}

func TestChannelKeyCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cid, _ := s.CreateChannel(ctx, "c", "openai", "https://x.com")
	k1, err := s.AddChannelKey(ctx, cid, "enc:k1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.AddChannelKey(ctx, cid, "enc:k2")

	keys, err := s.ListChannelKeys(ctx, cid)
	if err != nil || len(keys) != 2 {
		t.Fatal("应有 2 个 key", len(keys), err)
	}
	if keys[0].KeyEnc != "enc:k1" {
		t.Error("key 内容不正确")
	}

	future := time.Now().Add(time.Hour)
	if err := s.SetKeyCooldown(ctx, k1, &future); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.ListChannelKeys(ctx, cid)
	if !keys[0].CooldownUntil.Valid {
		t.Error("冷却时间应已写入")
	}

	if err := s.ReplaceChannelKeys(ctx, cid, []string{"enc:k3"}); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.ListChannelKeys(ctx, cid)
	if len(keys) != 1 || keys[0].KeyEnc != "enc:k3" {
		t.Error("整体替换失败", keys)
	}

	if err := s.DeleteChannel(ctx, cid); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.ListChannelKeys(ctx, cid)
	if len(keys) != 0 {
		t.Error("删除渠道后 key 应级联删除")
	}
}

func TestModelSyncAndOverride(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cid, _ := s.CreateChannel(ctx, "c", "openai", "https://x.com")
	caps := map[string]Capabilities{
		"model-a": {System: true, Tools: true},
		"model-b": {},
	}
	syncedAt := time.Now().UTC()
	added, err := s.SyncModels(ctx, cid, caps, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Error("首次同步应新增 2 条，got", added)
	}

	// 再次同步：仅传 model-a → model-b 应被清理（上游已下线），model-a 不重复新增
	added, err = s.SyncModels(ctx, cid, map[string]Capabilities{"model-a": {System: false}}, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Error("已存在的模型不应重复新增，got", added)
	}
	m, err := s.GetModel(ctx, cid, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities.System {
		t.Error("模型能力应被更新为 false")
	}
	// model-b 不在新列表中，应被删除
	if _, err := s.GetModel(ctx, cid, "model-b"); err == nil {
		t.Error("model-b 不在新列表中，应被清理")
	}

	// 手动覆盖
	if err := s.OverrideCapabilities(ctx, m.ID, Capabilities{System: true, Vision: true}, []string{"system", "vision"}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetModel(ctx, cid, "model-a")
	if !m.Capabilities.System || !m.Capabilities.Vision || m.CapabilityOverride != `["system","vision"]` {
		t.Error("能力覆盖未生效", m)
	}

	// 启用/别名
	if err := s.SetModelEnabled(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelAlias(ctx, m.ID, "alias-x"); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetModel(ctx, cid, "model-a")
	if m.Enabled || m.Alias != "alias-x" {
		t.Error("启用/别名更新失败")
	}

	if n, _ := s.CountModels(ctx); n != 1 {
		t.Error("模型总数应为 1（model-b 已清理）")
	}

	// 删除渠道级联删模型
	if err := s.DeleteChannel(ctx, cid); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountModels(ctx); n != 0 {
		t.Error("删除渠道后模型应级联删除")
	}
}
