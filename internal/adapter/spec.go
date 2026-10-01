package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spec 可序列化的框架适配器声明（与 fiber.go / gin.go 同构）：LLM 生成（L3）后经引擎复核落盘为
// <repo>/.specforge/adapters/<name>.json，供人审阅、提交与跨仓库复用；下次运行直接加载，不再调用 LLM。
type Spec struct {
	Name          string            `json:"name"`                    // 框架标识
	Short         string            `json:"short"`                   // 证据来源标签前缀
	Module        string            `json:"module"`                  // 框架模块路径前缀
	RouterTypes   []string          `json:"routerTypes"`             // 路由对象类型 ID（去指针）
	GroupMethod   string            `json:"groupMethod"`             // 分组方法名（无则空）
	Verbs         map[string]string `json:"verbs"`                   // 注册方法名 → HTTP 方法
	HandlerArg    int               `json:"handlerArg"`              // handler 实参下标；-1 = 末位
	PathArg       int               `json:"pathArg,omitempty"`       // 路径实参下标；0 = 第一个实参
	PathArgs      map[string]int    `json:"pathArgs,omitempty"`      // 按注册方法覆盖路径实参下标
	MethodArgs    map[string]int    `json:"methodArgs,omitempty"`    // 按注册方法设置 HTTP 方法实参下标
	NestedMethods map[string]bool   `json:"nestedMethods,omitempty"` // 回调式分组方法名
	BodyBinders   []BodyBinder      `json:"bodyBinders,omitempty"`   // 请求体绑定原语
	StructBinders []StructBinder    `json:"structBinders,omitempty"` // 结构体参数绑定原语
	ParamReaders  []ParamReader     `json:"paramReaders,omitempty"`  // 单参数读取原语
	Writers       []Writer          `json:"writers,omitempty"`       // 响应写出原语
	Learned       bool              `json:"learned"`                 // 是否由 LLM 生成（经复核）
	Dropped       []string          `json:"dropped,omitempty"`       // 复核时剪除的条目及原因（供审阅）
}

// Framework 声明 → 适配器。
func (s Spec) Framework() Framework {
	types := map[string]bool{}
	for _, t := range s.RouterTypes {
		types[t] = true
	}
	return routerFramework{
		name: s.Name, short: s.Short, module: s.Module,
		spec: RouterSpec{RouterTypes: types, GroupMethod: s.GroupMethod, Verbs: s.Verbs,
			HandlerArg: s.HandlerArg, PathArg: s.PathArg, PathArgs: s.PathArgs,
			MethodArgs: s.MethodArgs, NestedMethods: s.NestedMethods},
		prims: Primitives{BodyBinders: s.BodyBinders, StructBinders: s.StructBinders,
			ParamReaders: s.ParamReaders, Writers: s.Writers},
	}
}

// specExt 适配器声明文件扩展名。
const specExt = ".json"

// LoadSpecs 读取目录下全部适配器声明（按文件名排序）；目录不存在返回空。
func LoadSpecs(dir string) ([]Spec, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), specExt) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]Spec, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		var s Spec
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("adapter spec %s: %w", n, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// SaveSpec 写入适配器声明（文件名取框架名，非法字符替换为 _）。
func SaveSpec(dir string, s Spec) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == ' ' {
			return '_'
		}
		return r
	}, s.Name)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name+specExt)
	return p, os.WriteFile(p, append(b, '\n'), 0o644)
}

// DetectWith 在内置框架之外叠加额外声明（已落盘/刚学习的适配器）后识别。
func DetectWith(imports []string, extra []Framework) []Framework {
	out := Detect(imports)
	for _, fw := range extra {
		for _, imp := range imports {
			if moduleMatches(imp, fw.Module()) {
				out = append(out, fw)
				break
			}
		}
	}
	return out
}
