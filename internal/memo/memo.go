// Package memo 实现运行级 memo 缓存：输入指纹未变 → 复用上次生成的全部产物，
// 跳过 packages.Load 与全量分析。
//
// 这是设计文档 §5「增量查询引擎」的第 1 层（整次运行粒度）；LLM 调用级缓存见 infer.CachedProvider。
// go/packages 按包做带类型加载、无法对「单文件变更」增量类型检查，真正可省的是「输入完全未变」
// 的整段重算。指纹覆盖：引擎构建身份 + 服务名 + 画像内容 + LLM 配置 + 仓库内全部 .go/go.mod/go.sum。
//
// 缓存是内容寻址的（指纹为键），每个指纹一个目录，整目录原子落盘（临时目录 + rename），
// 天然并发安全、不会读到半截产物；命中与否都保守——宁可多算，绝不把陈旧产物当新鲜结果。
package memo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// formatVersion memo 条目格式版本：目录结构或指纹算法变化时递增。
const formatVersion = "specforge-memo-v3"

// MaxEntries 保留的 memo 条目上限：写入新条目后按最近使用时间淘汰更旧的条目。
const MaxEntries = 16

// skipDirs 指纹计算跳过的目录（VCS 元数据、工具产物、三方依赖）。
var skipDirs = map[string]bool{".git": true, ".specforge": true, "vendor": true, "node_modules": true}

// Fingerprint 计算一次运行的输入指纹。
//
// 覆盖：格式版本 + 引擎构建身份 + 服务过滤名 + 画像内容（显式画像文件字节；未显式指定时
// 取 <repo>/.specforge/profile.yaml，不存在记 "default-profile"）+ LLM 运行模式（llmKey）+
// 仓库内全部源码文件（isSource 由语言前端判定）的内容哈希（按相对路径排序）。
func Fingerprint(repoDir, service, profilePath, engineVersion, llmKey string, isSource func(name string) bool) (string, error) {
	return FingerprintWithInputs(repoDir, service, profilePath, engineVersion, llmKey, nil, isSource)
}

// FingerprintWithInputs extends the repository fingerprint with external input
// files such as an existing OpenAPI document or runtime observation log.
// Paths are normalized and content-addressed so changing an input invalidates
// the run memo without making the external file part of the source scan.
func FingerprintWithInputs(repoDir, service, profilePath, engineVersion, llmKey string, inputs []string, isSource func(name string) bool) (string, error) {
	h := sha256.New()
	for _, part := range []string{formatVersion, engineVersion, service, llmKey} {
		io.WriteString(h, part+"\x00")
	}
	if profilePath == "" {
		profilePath = filepath.Join(repoDir, ".specforge", "profile.yaml")
	}
	if b, err := os.ReadFile(profilePath); err == nil {
		h.Write(b)
	} else {
		io.WriteString(h, "default-profile")
	}
	h.Write([]byte{0})

	type fh struct {
		rel  string
		hash string
	}
	var files []fh
	err := filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSource(d.Name()) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(repoDir, path)
		files = append(files, fh{filepath.ToSlash(rel), hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	for _, f := range files {
		io.WriteString(h, f.rel+"\x00"+f.hash+"\x00")
	}
	var extra []string
	for _, input := range inputs {
		abs, err := filepath.Abs(input)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		extra = append(extra, filepath.ToSlash(abs)+"\x00"+hex.EncodeToString(sum[:]))
	}
	sort.Strings(extra)
	for _, item := range extra {
		io.WriteString(h, "external\x00"+item+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Store 磁盘 memo 缓存：<dir>/<指纹>/<产物名>。
type Store struct {
	dir string // 缓存根目录；空 = 禁用
}

var storeMu sync.Mutex

// New 创建 Store（惰性，首次 Save 时建目录）；dir 为空时 Load 恒未命中、Save 为空操作。
func New(dir string) *Store { return &Store{dir: dir} }

// Load 命中时返回指纹下全部产物（文件名 → 内容）；names 中任一缺失视为未命中。
// 命中会刷新条目的修改时间（LRU 淘汰依据）。
func (s *Store) Load(fp string, names ...string) (map[string][]byte, bool) {
	if s.dir == "" {
		return nil, false
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	entry := filepath.Join(s.dir, fp)
	out := make(map[string][]byte, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(entry, n))
		if err != nil {
			return nil, false
		}
		out[n] = b
	}
	now := time.Now()
	_ = os.Chtimes(entry, now, now)
	return out, true
}

// Save 原子写入一个指纹的全部产物，并淘汰超出 MaxEntries 的最旧条目。
func (s *Store) Save(fp string, files map[string][]byte) error {
	if s.dir == "" {
		return nil
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.dir, ".tmp-")
	if err != nil {
		return err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o644); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	final := filepath.Join(s.dir, fp)
	os.RemoveAll(final) // 同指纹并发写：后到者覆盖，内容相同
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return s.prune(MaxEntries)
}

// prune 保留最近使用的 keep 个条目，删除其余条目与残留临时目录。
func (s *Store) prune(keep int) error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	type ent struct {
		name string
		mod  time.Time
	}
	var list []ent
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if info, err := e.Info(); err == nil {
			list = append(list, ent{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	for i := keep; i < len(list); i++ {
		os.RemoveAll(filepath.Join(s.dir, list[i].name))
	}
	return nil
}
