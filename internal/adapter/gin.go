package adapter

// ginPkg gin-gonic/gin 的包路径。
const ginPkg = "github.com/gin-gonic/gin"

// ginCtx gin 请求上下文方法的符号 ID。
func ginCtx(method string) string { return MethodSymbol(ginPkg, "Context", method) }

// ginWriter gin 写出器：c.JSON(status, body) 形态（状态码在第 0 个实参）。
func ginWriter(method string) Writer {
	return Writer{Symbol: ginCtx(method), BodyArg: 1, StatusArg: 0, DefaultStatus: defaultOKStatus}
}

// Gin gin-gonic/gin 适配器（纯声明：路由形态 + 原语表）。
var Gin Framework = routerFramework{
	name: "gin", short: "gin", module: ginPkg,
	spec: RouterSpec{
		RouterTypes: map[string]bool{ginPkg + ".Engine": true, ginPkg + ".RouterGroup": true,
			ginPkg + ".IRouter": true, ginPkg + ".IRoutes": true},
		GroupMethod: "Group",
		Verbs: map[string]string{"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
			"PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS", "Any": "ALL"},
	},
	prims: Primitives{
		BodyBinders: []BodyBinder{
			{Symbol: ginCtx("ShouldBindJSON"), Arg: 0, ContentType: jsonContentType},
			{Symbol: ginCtx("BindJSON"), Arg: 0, ContentType: jsonContentType},
			{Symbol: ginCtx("ShouldBind"), Arg: 0, ContentType: jsonContentType},
			{Symbol: ginCtx("Bind"), Arg: 0, ContentType: jsonContentType},
		},
		StructBinders: []StructBinder{
			{Symbol: ginCtx("ShouldBindQuery"), Arg: 0, In: "query", TagKey: "form"},
			{Symbol: ginCtx("BindQuery"), Arg: 0, In: "query", TagKey: "form"},
			{Symbol: ginCtx("ShouldBindUri"), Arg: 0, In: "path", TagKey: "uri"},
			{Symbol: ginCtx("BindUri"), Arg: 0, In: "path", TagKey: "uri"},
			{Symbol: ginCtx("ShouldBindHeader"), Arg: 0, In: "header", TagKey: "header"},
			{Symbol: ginCtx("BindHeader"), Arg: 0, In: "header", TagKey: "header"},
		},
		ParamReaders: []ParamReader{
			{Symbol: ginCtx("Query"), In: "query", NameArg: 0, Type: "string"},
			{Symbol: ginCtx("DefaultQuery"), In: "query", NameArg: 0, Type: "string"},
			{Symbol: ginCtx("GetQuery"), In: "query", NameArg: 0, Type: "string"},
			{Symbol: ginCtx("Param"), In: "path", NameArg: 0, Type: "string"},
			{Symbol: ginCtx("GetHeader"), In: "header", NameArg: 0, Type: "string"},
			{Symbol: ginCtx("Cookie"), In: "cookie", NameArg: 0, Type: "string"},
		},
		Writers: []Writer{
			ginWriter("JSON"), ginWriter("IndentedJSON"), ginWriter("PureJSON"), ginWriter("SecureJSON"),
			ginWriter("AbortWithStatusJSON"), ginWriter("XML"),
		},
	},
}
