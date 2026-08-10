// Package crypto 提供上游 API key 的 AES-GCM 加解密。
// 密钥来自环境变量 GATEWAY_ENC_KEY；未配置时降级为明文存储（启动时已有警告）。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const encPrefix = "enc:"

// Cipher 封装 AES-256-GCM。nil 表示未配置密钥（不加密）。
type Cipher struct {
	aead cipher.AEAD
}

// New 用密钥构造 Cipher。key 为空返回 nil（明文降级）。任意长度密钥用 sha256 派生为 32 字节。
func New(key string) (*Cipher, error) {
	if key == "" {
		return nil, nil
	}
	digest := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return nil, fmt.Errorf("创建 AES 密钥: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 加密明文。未配置密钥时原样返回（明文降级）。
func (c *Cipher) Encrypt(plain string) (string, error) {
	if c == nil {
		return plain, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成随机数: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密。未配置密钥或输入为明文时原样返回。
// 已配置密钥但密文解密失败（密钥更换/数据损坏）返回错误。
func (c *Cipher) Decrypt(s string) (string, error) {
	if c == nil {
		return s, nil
	}
	if !strings.HasPrefix(s, encPrefix) {
		return s, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, encPrefix))
	if err != nil {
		return "", fmt.Errorf("解码密文: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("密文长度不足")
	}
	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（GATEWAY_ENC_KEY 是否更换？）: %w", err)
	}
	return string(plain), nil
}
