package codegraph

import (
	"crypto/sha256"
	"encoding/hex"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// missingFileHash 文件不可读时的指纹占位（证据校验据此判定证据失效）。
const missingFileHash = "missing"

// SetRoot 设置仓库根（证据路径相对化的基准）。
func (g *Graph) SetRoot(root string) { g.root = root }

// Root 仓库根绝对路径。
func (g *Graph) Root() string { return g.root }

// RelPath 绝对路径 → 仓库相对路径（斜杠分隔）；不在仓库内或未设根时原样返回。
// 产物里的证据路径必须与机器无关，否则同一仓库在不同目录生成的 spec 不逐字节一致。
func (g *Graph) RelPath(path string) string {
	if g.root == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(g.root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

// AbsPath 仓库相对路径 → 绝对路径（RelPath 的逆）。
func (g *Graph) AbsPath(path string) string {
	if filepath.IsAbs(path) || g.root == "" {
		return path
	}
	return filepath.Join(g.root, filepath.FromSlash(path))
}

// FileHashOf 文件内容指纹（懒计算，并发安全）；不可读返回 missingFileHash。
func (g *Graph) FileHashOf(path string) string {
	g.fileMu.Lock()
	defer g.fileMu.Unlock()
	g.loadFileLocked(path)
	return g.fileHash[path]
}

// FileLines 文件按行切分的源码（懒加载，并发安全）；不可读返回 nil。
func (g *Graph) FileLines(path string) []string {
	g.fileMu.Lock()
	defer g.fileMu.Unlock()
	g.loadFileLocked(path)
	return g.fileLines[path]
}

// loadFileLocked 读文件并同时填充指纹与行缓存（调用方持有 fileMu）。
func (g *Graph) loadFileLocked(path string) {
	if _, ok := g.fileHash[path]; ok {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		g.fileHash[path] = missingFileHash
		return
	}
	sum := sha256.Sum256(data)
	g.fileHash[path] = hex.EncodeToString(sum[:])
	g.fileLines[path] = strings.Split(string(data), "\n")
}

// SourceRange 文件 [from, to] 行（1 起、闭区间）的源码；越界部分截断。
func (g *Graph) SourceRange(path string, from, to int) string {
	lines := g.FileLines(path)
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

// ValidLine 行号是否落在文件内（证据校验）。
func (g *Graph) ValidLine(path string, line int) bool {
	return line >= 1 && line <= len(g.FileLines(path))
}

// FuncSource 函数声明的源码位置与文本（含 doc 注释之外的签名与函数体）；无函数体返回 ok=false。
func (g *Graph) FuncSource(id string) (file string, start, end int, text string, ok bool) {
	fd := g.funcDecls[id]
	if fd == nil || g.Fset == nil {
		return "", 0, 0, "", false
	}
	sp, ep := g.Fset.Position(fd.Pos()), g.Fset.Position(fd.End())
	if !sp.IsValid() || !ep.IsValid() {
		return "", 0, 0, "", false
	}
	return sp.Filename, sp.Line, ep.Line, g.SourceRange(sp.Filename, sp.Line, ep.Line), true
}

// EnclosingFunc 包含源码位置 pos 的函数声明符号 ID（有函数体的顶层函数/方法）；找不到返回空。
func (g *Graph) EnclosingFunc(pos token.Pos) string {
	best := ""
	for id, fd := range g.funcDecls {
		if fd.Pos() <= pos && pos < fd.End() && (best == "" || id < best) {
			best = id
		}
	}
	return best
}
