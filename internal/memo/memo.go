// Package memo 实现运行级 memo 缓存：输入指纹未变 → 复用上次生成的
// openapi.yaml / report.md 字节，跳过 packages.Load 与全量分析。
//
// 这是设计文档 §5「增量查询引擎」在静态流水线阶段的 80/20 落地：
// 静态管线总耗时 ~1.5s，其中 ~90% 是 go/packages 的带类型加载——它按包
// 工作、无法对「单文件变更」做增量类型检查，符号级缓存救不了它。真正
// 可省的是「输入完全未变」的整段重算。指纹覆盖：引擎版本 + 服务名 +
// 画像内容 + 仓库内全部 .go/go.mod/go.sum 的内容哈希。
//
// 缓存是「内容寻址」的（指纹为键），天然支持并发、天然避免脏读；
// 命中与否都保守——宁可多算（指纹外的输入变化没被覆盖时重算），绝不
// 复用旧输出（不会把陈旧 spec 当新鲜结果返回）。
package memo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Summary 一次运行的统计摘要（缓存命中时回填 Result 用）。
type Summary struct {
	Service          string `json:"service"`
	Operations       int    `json:"operations"`
	Routes           int    `json:"routes"`
	RoutesUnresolved int    `json:"routes_unresolved"`
	SchemaTypes      int    `json:"schema_types"`
	Facts            int    `json:"facts"`
	LowConfidence    int    `json:"low_confidence"`
	ElapsedMs        int64  `json:"elapsed_ms"`
	Cached           bool   `json:"cached"`
}

// Fingerprint 计算一次运行的输入指纹。
//
// 覆盖：引擎版本 + 服务过滤名 + 画像内容（显式画像文件字节，无画像则
// 记 "default-profile"）+ 仓库内全部 .go / go.mod / go.sum 的内容哈希
// （按相对路径排序）。跳过 .git / .specforge / vendor / node_modules。
func Fingerprint(repoDir, service, profilePath, engineVersion string) (string, error) {
	h := sha256.New()
	io.WriteString(h, "specforge-memo-v1\x00")
	io.WriteString(h, engineVersion+"\x00")
	io.WriteString(h, service+"\x00")
	if profilePath != "" {
		if b, err := os.ReadFile(profilePath); err == nil {
			h.Write(b)
		}
	} else {
		io.WriteString(h, "default-profile")
	}
	h.Write([]byte{0})

	type fh struct {
		rel  string
		hash string
	}
	var files []fh
	_ = filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".specforge", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") && name != "go.mod" && name != "go.sum" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(repoDir, path)
		files = append(files, fh{rel, hex.EncodeToString(sum[:])})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	for _, f := range files {
		io.WriteString(h, f.rel+"\x00"+f.hash+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Store 磁盘 memo 缓存（目录下按指纹存 spec.yaml / report.md / summary.json）。
type Store struct {
	dir string
}

// New 创建（惰性，首次 Save 时建目录）。
func New(dir string) *Store { return &Store{dir: dir} }

// Load 命中返回三个产物的字节与摘要；未命中返回 ok=false。
func (s *Store) Load(fp string) (spec, report []byte, summary Summary, ok bool) {
	if s.dir == "" {
		return nil, nil, summary, false
	}
	spec, err1 := os.ReadFile(filepath.Join(s.dir, fp+".yaml"))
	report, err2 := os.ReadFile(filepath.Join(s.dir, fp+".report.md"))
	sb, err3 := os.ReadFile(filepath.Join(s.dir, fp+".summary.json"))
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, nil, summary, false
	}
	if json.Unmarshal(sb, &summary) != nil {
		return nil, nil, Summary{}, false
	}
	return spec, report, summary, true
}

// Save 写入一次运行的全部产物。
func (s *Store) Save(fp string, spec, report []byte, summary Summary) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, fp+".yaml"), spec, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, fp+".report.md"), report, 0o644); err != nil {
		return err
	}
	sb, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, fp+".summary.json"), sb, 0o644)
}
