package crypto

import (
	"strings"
	"testing"
)

func TestRoundTripWithKey(t *testing.T) {
	c, err := New("some-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	plain := "sk-very-secret-123"
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if enc == plain || !strings.HasPrefix(enc, encPrefix) {
		t.Error("加密结果应带前缀且与明文不同")
	}
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec != plain {
		t.Error("解密后应还原明文")
	}
}

func TestPlaintextFallback(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatal("无密钥时应返回 nil Cipher")
	}
	plain := "sk-plain"
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if enc != plain {
		t.Error("无密钥时应原样返回")
	}
	dec, err := c.Decrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if dec != plain {
		t.Error("无密钥时应原样返回")
	}
}

func TestDecryptLegacyPlaintextWithKey(t *testing.T) {
	c, _ := New("some-secret-key")
	// 历史明文数据在配置密钥后仍可读取
	dec, err := c.Decrypt("sk-legacy-plain")
	if err != nil || dec != "sk-legacy-plain" {
		t.Error("明文历史数据应原样返回", dec, err)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	c1, _ := New("key-one")
	c2, _ := New("key-two")
	enc, _ := c1.Encrypt("sk-x")
	if _, err := c2.Decrypt(enc); err == nil {
		t.Error("密钥更换后应解密失败")
	}
}

func TestEncryptUniqueNonce(t *testing.T) {
	c, _ := New("k")
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Error("相同明文每次加密应不同（随机 nonce）")
	}
}
