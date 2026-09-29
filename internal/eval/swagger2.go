package eval

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// swagger2Version swagger 2.0 文档的版本标识（`swagger: "2.0"`），
// 用于识别 swaggo/swag 等注释驱动工具的产物并归一化到 OAS3 评测结构。
const swagger2Version = "2.0"

// jsonMediaType 归一化后请求体/响应体挂载的媒体类型（swagger 2.0 无 content 层）。
const jsonMediaType = "application/json"

// paramInBody swagger 2.0 中承载请求体的参数位置（OAS3 改为 requestBody）。
const paramInBody = "body"

// httpMethods OpenAPI path item 中表示 operation 的键（其余如 parameters/summary 非 operation）。
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// docHeader 文档版本嗅探：swagger 2.0 有 swagger 键，OAS3 有 openapi 键。
type docHeader struct {
	Swagger string `yaml:"swagger"` // swagger 2.0 版本号，OAS3 文档为空
}

// rawDoc 按 path item 原始节点解析，只解码 HTTP method 键，
// 避免 path 级 parameters/summary 等非 operation 键导致整体解析失败。
type rawDoc struct {
	BasePath    string                          `yaml:"basePath"`    // swagger 2.0 路径前缀（OAS3 无）
	Paths       map[string]map[string]yaml.Node `yaml:"paths"`       // path → 键 → 原始节点
	Components  oasComponents                   `yaml:"components"`  // OAS3 组件
	Definitions map[string]oasSchema            `yaml:"definitions"` // swagger 2.0 组件（等价 components.schemas）
}

// swag2Op swagger 2.0 operation（参数直接带类型，body 参数承载请求体）。
type swag2Op struct {
	OperationID string               `yaml:"operationId"` // operation 标识
	Parameters  []swag2Param         `yaml:"parameters"`  // 含 in=body 的请求体参数
	Responses   map[string]swag2Resp `yaml:"responses"`   // status → 响应
}

// swag2Param swagger 2.0 参数：非 body 参数类型平铺在参数自身，body 参数用 schema。
type swag2Param struct {
	Name     string        `yaml:"name"`     // 参数名
	In       string        `yaml:"in"`       // query/path/header/cookie/formData/body
	Required bool          `yaml:"required"` // 是否必填
	Type     interface{}   `yaml:"type"`     // 非 body 参数的类型
	Format   string        `yaml:"format"`   // 非 body 参数的格式
	Enum     []interface{} `yaml:"enum"`     // 非 body 参数的枚举
	Items    *oasSchema    `yaml:"items"`    // 数组参数元素
	Schema   *oasSchema    `yaml:"schema"`   // body 参数的 schema
}

// swag2Resp swagger 2.0 响应：schema 直接挂在响应上（无 content 媒体类型层）。
type swag2Resp struct {
	Description string     `yaml:"description"` // 响应描述
	Schema      *oasSchema `yaml:"schema"`      // 响应体 schema
}

// parseDoc 解析 OAS3 或 swagger 2.0 文档，统一归一化为 oasDoc。
func parseDoc(data []byte) (*oasDoc, error) {
	var h docHeader
	if err := yaml.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	var raw rawDoc
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	isSwagger2 := h.Swagger == swagger2Version
	d := &oasDoc{Paths: map[string]map[string]oasOp{}, Components: raw.Components}
	if isSwagger2 {
		d.Components = oasComponents{Schemas: raw.Definitions}
	}
	prefix := strings.TrimSuffix(raw.BasePath, "/")
	for path, item := range raw.Paths {
		ops := map[string]oasOp{}
		for key, node := range item {
			if !httpMethods[strings.ToLower(key)] {
				continue
			}
			node := node
			op, err := decodeOp(&node, isSwagger2)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", key, path, err)
			}
			ops[strings.ToLower(key)] = op
		}
		if len(ops) > 0 {
			d.Paths[prefix+path] = ops
		}
	}
	return d, nil
}

// decodeOp 解码单个 operation；swagger 2.0 转换为 OAS3 形态。
func decodeOp(node *yaml.Node, isSwagger2 bool) (oasOp, error) {
	if !isSwagger2 {
		var op oasOp
		err := node.Decode(&op)
		return op, err
	}
	var s swag2Op
	if err := node.Decode(&s); err != nil {
		return oasOp{}, err
	}
	return convertSwag2Op(s), nil
}

// convertSwag2Op swagger 2.0 operation → OAS3：body 参数转 requestBody，
// 参数平铺类型收进 schema，响应 schema 挂到 application/json content 下。
func convertSwag2Op(s swag2Op) oasOp {
	op := oasOp{OperationID: s.OperationID, Responses: map[string]oasResp{}}
	for _, p := range s.Parameters {
		if p.In == paramInBody {
			if p.Schema != nil {
				op.RequestBody = &oasBody{Content: map[string]struct {
					Schema oasSchema `yaml:"schema"`
				}{jsonMediaType: {Schema: *p.Schema}}}
			}
			continue
		}
		op.Parameters = append(op.Parameters, oasParam{
			Name: p.Name, In: p.In, Required: p.Required,
			Schema: oasSchema{Type: p.Type, Format: p.Format, Enum: p.Enum, Items: p.Items},
		})
	}
	for status, r := range s.Responses {
		resp := oasResp{Description: r.Description}
		if r.Schema != nil {
			resp.Content = map[string]struct {
				Schema oasSchema `yaml:"schema"`
			}{jsonMediaType: {Schema: *r.Schema}}
		}
		op.Responses[status] = resp
	}
	return op
}

// mergeAllOf 将 allOf 组合展开为单个 object schema：各分支 properties 合并（后者覆盖前者，
// 对应 swag `Envelope{result=T}` 用后一分支收窄信封槽的语义），required 取并集。
func mergeAllOf(s *oasSchema, comps map[string]oasSchema, depth int) oasSchema {
	out := *s
	out.AllOf = nil
	if depth > maxFlattenDepth {
		return out
	}
	props := map[string]oasSchema{}
	for k, v := range s.Props {
		props[k] = v
	}
	required := append([]string(nil), s.Required...)
	for i := range s.AllOf {
		part := s.AllOf[i]
		if part.Ref != "" {
			if sc, ok := comps[refName(part.Ref)]; ok {
				part = sc
			}
		}
		if len(part.AllOf) > 0 {
			part = mergeAllOf(&part, comps, depth+1)
		}
		for k, v := range part.Props {
			props[k] = v
		}
		required = append(required, part.Required...)
		if out.Type == nil {
			out.Type = part.Type
		}
	}
	out.Props = props
	out.Required = required
	return out
}
