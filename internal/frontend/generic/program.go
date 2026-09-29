// Package generic 是 L4 通用 LLM 前端：不做任何语言分析，只依赖源码文本。
//
// 程序视图是纯文本的（文件、行、哈希、正则识别的函数定义、括号/缩进划分的函数边界、按标识符近似的调用关系）；
// 路由由 LLM 按文件发现，契约由 LLM 按 operation 抽取，所有结论按文本核对（证据行存在、路径字面量与 handler
// 名出现在注册行附近、字段名出现在源码中），置信度按 text 级封顶。用于不支持的语言或完全无法解析的代码。
package generic

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/specforge/specforge/internal/frontend"
)

// sourceExts 视为源码的扩展名（覆盖主流 Web 后端语言）。
var sourceExts = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".jsx": true, ".tsx": true,
	".py": true, ".java": true, ".kt": true, ".kts": true, ".rb": true, ".php": true, ".cs": true, ".go": true,
	".rs": true, ".scala": true, ".swift": true, ".ex": true, ".exs": true}

// skipDirs 扫描跳过的目录（依赖、构建产物、VCS 与工具目录）。
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, ".venv": true, "venv": true, "__pycache__": true, ".specforge": true, ".idea": true, ".next": true}

// defPatterns 多语言函数定义正则（捕获组 1 为函数名）。
var defPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bfunction\s*\*?\s*([A-Za-z_$][\w$]*)\s*\(`),                                                                                          // JS/PHP
	regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:function\b|\([^)]*\)\s*=>|[A-Za-z_$][\w$]*\s*=>)`),                     // JS 箭头
	regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`),                                                                                               // Python/Ruby
	regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*[\[(]`),                                                                                  // Go
	regexp.MustCompile(`^\s*(?:(?:public|private|protected|internal|static|final|async|override|virtual|suspend)\s+)+[\w<>\[\],.?\s]*?\b([A-Za-z_]\w*)\s*\(`), // Java/C#/Kotlin/TS 方法
	regexp.MustCompile(`^\s*fun\s+([A-Za-z_]\w*)\s*\(`),                                                                                                       // Kotlin
	regexp.MustCompile(`^\s*(?:async\s+)?([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{\s*$`),                                                                            // JS 类方法简写
}

// notFuncNames 被方法简写正则误中的控制流关键字。
var notFuncNames = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "else": true, "do": true, "try": true, "with": true}

// callPattern 调用点：标识符后紧跟左括号。
var callPattern = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\(`)

// maxRegionLines 单个函数区域的最大行数（超长截断，防止括号失配吞掉整个文件）。
const maxRegionLines = 300

// braceLookahead 从定义行起寻找首个 '{' 的最大行数。
const braceLookahead = 3

// inlineMark 内联 handler 的符号名（区域从注册行开始）。
const inlineMark = "inline"

// Def 一个识别出的函数定义。
type Def struct {
	ID   string // 符号 ID：<相对路径>#<名字>@<行号>
	Name string // 函数名
	File string // 绝对路径
	Line int    // 定义行号
}

// Program 纯文本程序视图（实现 frontend.Program；并发安全、确定性）。
type Program struct {
	root  string   // 仓库根（绝对路径）
	files []string // 源码文件（绝对路径，有序）

	mu    sync.Mutex          // 保护 lines / hash
	lines map[string][]string // 文件 → 行
	hash  map[string]string   // 文件 → 内容指纹

	defsOnce sync.Once        // 定义表惰性构建
	defs     []Def            // 全部定义（按 ID 有序）
	byName   map[string][]Def // 名字 → 定义
	byID     map[string]Def   // ID → 定义
}

// NewProgram 扫描仓库源码文件构造程序视图。
func NewProgram(root string) (*Program, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	p := &Program{root: abs, lines: map[string][]string{}, hash: map[string]string{}}
	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != abs && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if sourceExts[filepath.Ext(path)] {
			p.files = append(p.files, path)
		}
		return nil
	})
	sort.Strings(p.files)
	return p, err
}

// Files 源码文件（绝对路径，有序）。
func (p *Program) Files() []string { return p.files }

// RelPath 实现 frontend.Program。
func (p *Program) RelPath(path string) string {
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(p.root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

// AbsPath 实现 frontend.Program。
func (p *Program) AbsPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.root, filepath.FromSlash(path))
}

// load 读取文件（缓存行与指纹）。
func (p *Program) load(path string) []string {
	abs := p.AbsPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if ls, ok := p.lines[abs]; ok {
		return ls
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		p.lines[abs], p.hash[abs] = nil, "missing"
		return nil
	}
	sum := sha256.Sum256(b)
	p.hash[abs] = hex.EncodeToString(sum[:])
	p.lines[abs] = strings.Split(string(b), "\n")
	return p.lines[abs]
}

// FileHash 实现 frontend.Program。
func (p *Program) FileHash(path string) string {
	p.load(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hash[p.AbsPath(path)]
}

// FileLines 实现 frontend.Program。
func (p *Program) FileLines(path string) []string { return p.load(path) }

// SourceRange 实现 frontend.Program。
func (p *Program) SourceRange(path string, from, to int) string {
	ls := p.load(path)
	if from < 1 {
		from = 1
	}
	if to > len(ls) {
		to = len(ls)
	}
	if from > to {
		return ""
	}
	return strings.Join(ls[from-1:to], "\n")
}

// ValidLine 实现 frontend.Program。
func (p *Program) ValidLine(path string, line int) bool {
	return line >= 1 && line <= len(p.load(path))
}

// IsSourceFile 实现 frontend.Program：仓库内、源码扩展名、不在依赖/产物目录。
func (p *Program) IsSourceFile(path string) bool {
	abs := p.AbsPath(path)
	rel := p.RelPath(abs)
	if rel == abs || !sourceExts[filepath.Ext(abs)] {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if skipDirs[seg] {
			return false
		}
	}
	return true
}

// buildDefs 扫描全部文件识别函数定义（首次查询时执行一次）。
func (p *Program) buildDefs() {
	p.defsOnce.Do(func() {
		p.byName, p.byID = map[string][]Def{}, map[string]Def{}
		for _, f := range p.files {
			for i, line := range p.load(f) {
				for _, re := range defPatterns {
					m := re.FindStringSubmatch(line)
					if m == nil || notFuncNames[m[1]] {
						continue
					}
					d := Def{ID: p.RelPath(f) + "#" + m[1] + "@" + strconv.Itoa(i+1), Name: m[1], File: f, Line: i + 1}
					if _, dup := p.byID[d.ID]; !dup {
						p.defs = append(p.defs, d)
						p.byID[d.ID] = d
						p.byName[d.Name] = append(p.byName[d.Name], d)
					}
					break
				}
			}
		}
		sort.Slice(p.defs, func(i, j int) bool { return p.defs[i].ID < p.defs[j].ID })
	})
}

// DefCount 识别出的函数定义数（统计用）。
func (p *Program) DefCount() int {
	p.buildDefs()
	return len(p.defs)
}

// Resolve 名字 → 定义：优先 near 文件内的定义，否则全仓唯一定义；歧义或不存在返回 false。
func (p *Program) Resolve(name, near string) (Def, bool) {
	p.buildDefs()
	if i := strings.LastIndexAny(name, ".:"); i >= 0 {
		name = name[i+1:] // UserController.get / module:handler → 末段
	}
	cands := p.byName[name]
	for _, d := range cands {
		if d.File == p.AbsPath(near) {
			return d, true
		}
	}
	if len(cands) == 1 {
		return cands[0], true
	}
	return Def{}, false
}

// InlineID 内联 handler 的符号 ID（区域从注册行开始）。
func (p *Program) InlineID(file string, line int) string {
	return p.RelPath(file) + "#" + inlineMark + "@" + strconv.Itoa(line)
}

// parseID 符号 ID → (绝对路径, 名字, 行号)。
func (p *Program) parseID(id string) (string, string, int, bool) {
	hash := strings.LastIndex(id, "#")
	at := strings.LastIndex(id, "@")
	if hash < 0 || at < hash {
		return "", "", 0, false
	}
	line, err := strconv.Atoi(id[at+1:])
	if err != nil {
		return "", "", 0, false
	}
	return p.AbsPath(id[:hash]), id[hash+1 : at], line, true
}

// FuncSource 实现 frontend.Program：从定义行起按括号配平（或 Python 式缩进）划出函数区域。
func (p *Program) FuncSource(id string) (string, int, int, string, bool) {
	file, _, start, ok := p.parseID(id)
	if !ok {
		return "", 0, 0, "", false
	}
	ls := p.load(file)
	if start < 1 || start > len(ls) {
		return "", 0, 0, "", false
	}
	end := blockEnd(ls, start)
	return file, start, end, strings.Join(ls[start-1:end], "\n"), true
}

// blockEnd 区域结束行：先找括号块，找不到时对以 ':' 结尾的定义行按缩进，否则只取定义行（单行箭头函数）。
func blockEnd(ls []string, start int) int {
	limit := min(len(ls), start+maxRegionLines-1)
	depth, opened := 0, false
	for i := start; i <= limit; i++ {
		for _, c := range stripStrings(ls[i-1]) {
			switch c {
			case '{':
				depth++
				opened = true
			case '}':
				depth--
			}
		}
		if opened && depth <= 0 {
			return i
		}
		if !opened && i-start >= braceLookahead {
			break
		}
	}
	if opened {
		return limit
	}
	if strings.HasSuffix(strings.TrimSpace(ls[start-1]), ":") {
		base := indentOf(ls[start-1])
		end := start
		for i := start + 1; i <= limit; i++ {
			if strings.TrimSpace(ls[i-1]) == "" {
				continue
			}
			if indentOf(ls[i-1]) <= base {
				break
			}
			end = i
		}
		return end
	}
	return start
}

// stripStrings 去掉行内字符串字面量与行尾注释（括号计数不受字符串中的括号影响）。
func stripStrings(line string) string {
	var b strings.Builder
	var quote rune
	for i, c := range line {
		switch {
		case quote != 0:
			if c == quote && (i == 0 || line[i-1] != '\\') {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return b.String()
		case c == '#':
			return b.String()
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// indentOf 行首空白宽度。
func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// Reach 实现 frontend.Program：handler 区域 + 按调用关系可达的函数（深度上限 reachDepth）。
func (p *Program) Reach(handler string) map[string]int {
	out := map[string]int{handler: 0}
	frontier := []string{handler}
	for d := 1; d <= reachDepth && len(frontier) > 0; d++ {
		var next []string
		for _, id := range frontier {
			for _, c := range p.Callees(id) {
				if _, seen := out[c]; !seen {
					out[c] = d
					next = append(next, c)
				}
			}
		}
		frontier = next
	}
	return out
}

// reachDepth 文本调用关系的可达深度。
const reachDepth = 2

// Callees 实现 frontend.Program：区域内「标识符(」且能解析到定义的函数（有序）。
func (p *Program) Callees(id string) []string {
	file, _, _, text, ok := p.FuncSource(id)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range callPattern.FindAllStringSubmatch(text, -1) {
		if d, ok := p.Resolve(m[1], file); ok && d.ID != id && !seen[d.ID] {
			seen[d.ID] = true
			out = append(out, d.ID)
		}
	}
	sort.Strings(out)
	return out
}

// Symbol 实现 frontend.Program。
func (p *Program) Symbol(id string) (frontend.SymbolInfo, bool) {
	p.buildDefs()
	d, ok := p.byID[id]
	if !ok {
		return frontend.SymbolInfo{}, false
	}
	return frontend.SymbolInfo{ID: d.ID, Kind: "function", File: d.File, Line: d.Line}, true
}

// SymbolExists 实现 frontend.Program。
func (p *Program) SymbolExists(id string) bool {
	_, ok := p.Symbol(id)
	return ok
}

// TypeOf 实现 frontend.Program：文本视图没有类型信息。
func (p *Program) TypeOf(string) (frontend.TypeInfo, bool) { return frontend.TypeInfo{}, false }

// FindSymbols 实现 frontend.Program。
func (p *Program) FindSymbols(query string) []string {
	p.buildDefs()
	q := strings.ToLower(query)
	var out []string
	if q == "" {
		return nil
	}
	for _, d := range p.defs {
		if strings.Contains(strings.ToLower(d.ID), q) {
			out = append(out, d.ID)
		}
	}
	return out
}

// FieldTypes 实现 frontend.Program：文本视图不提供字段类型。
func (p *Program) FieldTypes() map[string]string { return map[string]string{} }
