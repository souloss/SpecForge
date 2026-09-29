package infer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// cacheFormatVersion LLM 缓存条目格式版本：条目结构或键算法变化时递增，旧条目自然失效。
const cacheFormatVersion = "llmcache-v1"

// cacheShardLen 缓存目录按键前缀分片的长度（避免单目录文件过多）。
const cacheShardLen = 2

// CacheStats LLM 缓存命中统计（观测字段）。
type CacheStats struct {
	Hits        int   `json:"hits"`         // 命中（含读集校验通过）
	Misses      int   `json:"misses"`       // 未命中（无条目）
	Stale       int   `json:"stale"`        // 有条目但读集校验失败（依赖的代码已变）
	SavedTokens int64 `json:"saved_tokens"` // 命中所节省的 token（原调用用量之和）
}

// cacheEntry 磁盘缓存条目。
type cacheEntry struct {
	Text  string     `json:"text"`  // 模型原始输出
	Usage Usage      `json:"usage"` // 原调用用量（命中时计入节省）
	Reads []ToolRead `json:"reads"` // 工具读集（命中前逐条重放校验）
}

// CachedProvider 内容寻址的 LLM 调用缓存（设计文档 §5「LLM 调用也是可记忆化的 query」）。
//
// 键 = hash(格式版本, 模型名, system, prompt, schema, 工具签名, 轮数)：提示词或证据任何变化都换键，
// 无需手工失效。档位3 的输出还依赖模型在循环中「读到了什么」——条目记录每次工具调用的
// (工具, 入参, 输出指纹)，命中前在当前代码图上重放这些调用，任一输出指纹变化即判陈旧并重算。
// 只缓存成功调用；调用失败不落盘（下次重试）。并发安全。
type CachedProvider struct {
	inner Provider // 被包装的真实 provider
	dir   string   // 缓存目录

	mu    sync.Mutex // 保护 stats
	stats CacheStats // 命中统计
}

// NewCachedProvider 包装 provider；dir 为空时返回原 provider（禁用缓存）。
func NewCachedProvider(p Provider, dir string) Provider {
	if p == nil || dir == "" {
		return p
	}
	return &CachedProvider{inner: p, dir: dir}
}

// Name 实现 Provider（与内层同名：缓存不改变语义）。
func (c *CachedProvider) Name() string { return c.inner.Name() }

// LLMUsage 实现 UsageReporter：透传内层实际发生的用量（命中不计入）。
func (c *CachedProvider) LLMUsage() Usage {
	if ur, ok := c.inner.(UsageReporter); ok {
		return ur.LLMUsage()
	}
	return Usage{}
}

// CacheStats 返回命中统计快照。
func (c *CachedProvider) CacheStats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Complete 实现 Provider：命中且读集有效直接返回；否则调用内层并落盘（含读集）。
func (c *CachedProvider) Complete(ctx context.Context, req Request) (Response, error) {
	key := c.key(req)
	if e, ok := c.load(key); ok {
		if replayValid(ctx, e.Reads, req.Tools) {
			c.bump(func(s *CacheStats) { s.Hits++; s.SavedTokens += e.Usage.TotalTokens() })
			return Response{Text: []byte(e.Text), Usage: e.Usage}, nil
		}
		c.bump(func(s *CacheStats) { s.Stale++ })
	} else {
		c.bump(func(s *CacheStats) { s.Misses++ })
	}
	var scope *toolScope
	if len(req.Tools) > 0 {
		ctx, scope = withToolScope(ctx, req.Tools)
	}
	resp, err := c.inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	e := cacheEntry{Text: string(resp.Text), Usage: resp.Usage}
	if scope != nil {
		e.Reads = scope.Reads()
	}
	_ = c.save(key, e) // 落盘失败只损失缓存，不影响本次结果
	return resp, nil
}

// Cached 请求当前是否可由缓存直接满足（条目存在且读集仍有效）；不计入命中统计。
// 上层据此只对「需付费」的调用扣预算。
func (c *CachedProvider) Cached(ctx context.Context, req Request) bool {
	e, ok := c.load(c.key(req))
	return ok && replayValid(ctx, e.Reads, req.Tools)
}

// bump 在锁内更新统计。
func (c *CachedProvider) bump(f func(*CacheStats)) {
	c.mu.Lock()
	f(&c.stats)
	c.mu.Unlock()
}

// key 请求的内容寻址键。
func (c *CachedProvider) key(req Request) string {
	h := sha256.New()
	for _, part := range []string{cacheFormatVersion, c.inner.Name(), req.System, req.Prompt, string(req.Schema)} {
		io.WriteString(h, part)
		h.Write([]byte{0})
	}
	for _, t := range req.Tools {
		io.WriteString(h, t.Name+"\x00"+t.Description+"\x00"+canonicalJSON(t.InputSchema)+"\x00")
	}
	io.WriteString(h, itoa(req.MaxTurns))
	return hex.EncodeToString(h.Sum(nil))
}

// path 键对应的条目路径（按前缀分片）。
func (c *CachedProvider) path(key string) string {
	return filepath.Join(c.dir, key[:cacheShardLen], key+".json")
}

// load 读取条目；不存在或损坏返回 false。
func (c *CachedProvider) load(key string) (cacheEntry, bool) {
	var e cacheEntry
	b, err := os.ReadFile(c.path(key))
	if err != nil || json.Unmarshal(b, &e) != nil {
		return cacheEntry{}, false
	}
	return e, true
}

// save 原子写入条目（临时文件 + rename，并发写同键不会产生半截文件）。
func (c *CachedProvider) save(key string, e cacheEntry) error {
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}

// replayValid 在当前代码上重放读集：每次工具调用的输出指纹都不变才算有效。
func replayValid(ctx context.Context, reads []ToolRead, tools []ToolSpec) bool {
	if len(reads) == 0 {
		return true
	}
	byName := map[string]ToolSpec{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	for _, r := range reads {
		t, ok := byName[r.Tool]
		if !ok || t.Call == nil {
			return false
		}
		var input map[string]any
		if err := json.Unmarshal([]byte(r.Input), &input); err != nil {
			return false
		}
		out, err := t.Call(ctx, input)
		if err != nil || hashText(out) != r.OutHash {
			return false
		}
	}
	return true
}

// writeFileAtomic 先写同目录临时文件再 rename，保证读者只看到完整文件。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// itoa 非负整数转十进制串（避免为一处引入 strconv）。
func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
