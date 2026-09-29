package adapter

// fiberPkg gofiber v2 的包路径。
const fiberPkg = "github.com/gofiber/fiber/v2"

// fiberCtx fiber 请求上下文方法的符号 ID。
func fiberCtx(method string) string { return MethodSymbol(fiberPkg, "Ctx", method) }

// jsonContentType JSON 请求体媒体类型。
const jsonContentType = "application/json"

// defaultOKStatus 框架未显式设置状态码时的默认 HTTP 状态。
const defaultOKStatus = 200

// Fiber gofiber/v2 适配器（纯声明：路由形态 + 原语表）。
var Fiber Framework = routerFramework{
	name: "gofiber/v2", short: "fiber", module: fiberPkg,
	spec: RouterSpec{
		RouterTypes: map[string]bool{fiberPkg + ".App": true, fiberPkg + ".Group": true, fiberPkg + ".Router": true},
		GroupMethod: "Group",
		Verbs: map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
			"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS", "All": "ALL"},
	},
	prims: Primitives{
		BodyBinders: []BodyBinder{{Symbol: fiberCtx("BodyParser"), Arg: 0, ContentType: jsonContentType}},
		StructBinders: []StructBinder{
			{Symbol: fiberCtx("QueryParser"), Arg: 0, In: "query", TagKey: "query"},
			{Symbol: fiberCtx("ParamsParser"), Arg: 0, In: "path", TagKey: "params"},
			{Symbol: fiberCtx("ReqHeaderParser"), Arg: 0, In: "header", TagKey: "reqHeader"},
			{Symbol: fiberCtx("CookieParser"), Arg: 0, In: "cookie", TagKey: "cookie"},
		},
		ParamReaders: []ParamReader{
			{Symbol: fiberCtx("Query"), In: "query", NameArg: 0, Type: "string"},
			{Symbol: fiberCtx("QueryInt"), In: "query", NameArg: 0, Type: "integer"},
			{Symbol: fiberCtx("QueryBool"), In: "query", NameArg: 0, Type: "boolean"},
			{Symbol: fiberCtx("QueryFloat"), In: "query", NameArg: 0, Type: "number"},
			{Symbol: fiberCtx("Params"), In: "path", NameArg: 0, Type: "string"},
			{Symbol: fiberCtx("Get"), In: "header", NameArg: 0, Type: "string"},
			{Symbol: fiberCtx("Cookies"), In: "cookie", NameArg: 0, Type: "string"},
		},
		Writers: []Writer{
			{Symbol: fiberCtx("JSON"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: fiberCtx("XML"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
		},
	},
}
