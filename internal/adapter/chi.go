package adapter

// chi is compatible with net/http and exposes the same Router interface in v4 and v5.

const (
	chiV4Pkg       = "github.com/go-chi/chi"
	chiV5Pkg       = "github.com/go-chi/chi/v5"
	renderPkg      = "github.com/go-chi/render"
	chiV4RenderPkg = "github.com/go-chi/chi/render"
	httpPkg        = "net/http"
	jsonPkg        = "encoding/json"
	urlPkg         = "net/url"
	ioPkg          = "io"
)

func chiMethod(pkg, recv, method string) string { return MethodSymbol(pkg, recv, method) }

func chiFunc(pkg, name string) string { return pkg + "." + name }

func chiInterfaceMethod(pkg, iface, method string) string { return pkg + "." + iface + "." + method }

func chiPrimitives(pkg string) Primitives {
	prims := Primitives{
		BodyBinders: []BodyBinder{
			{Symbol: chiMethod(jsonPkg, "Decoder", "Decode"), Arg: 0, ContentType: jsonContentType},
		},
		ParamReaders: []ParamReader{
			{Symbol: chiFunc(pkg, "URLParam"), In: "path", NameArg: 1, Type: "string"},
			{Symbol: chiFunc(pkg, "URLParamFromCtx"), In: "path", NameArg: 1, Type: "string"},
			{Symbol: chiMethod(urlPkg, "Values", "Get"), In: "query", NameArg: 0, Type: "string"},
			{Symbol: chiMethod(httpPkg, "Header", "Get"), In: "header", NameArg: 0, Type: "string"},
			{Symbol: chiMethod(httpPkg, "Request", "PathValue"), In: "path", NameArg: 0, Type: "string"},
		},
		Writers: []Writer{
			{Symbol: chiFunc(renderPkg, "JSON"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiFunc(renderPkg, "XML"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiFunc(renderPkg, "PlainText"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiFunc(httpPkg, "Error"), BodyArg: 1, StatusArg: 2, DefaultStatus: defaultOKStatus},
			{Symbol: chiMethod(jsonPkg, "Encoder", "Encode"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiInterfaceMethod(httpPkg, "ResponseWriter", "Write"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiInterfaceMethod(httpPkg, "ResponseWriter", "WriteHeader"), BodyArg: NoArg, StatusArg: 0, DefaultStatus: defaultOKStatus},
			{Symbol: chiFunc(ioPkg, "Copy"), BodyArg: 1, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
		},
	}
	prims.BodyBinders = append(prims.BodyBinders, BodyBinder{Symbol: chiFunc(renderPkg, "Bind"), Arg: 1, ContentType: jsonContentType})
	prims.Writers = append(prims.Writers,
		Writer{Symbol: chiFunc(renderPkg, "JSON"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
		Writer{Symbol: chiFunc(renderPkg, "XML"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
		Writer{Symbol: chiFunc(renderPkg, "PlainText"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
	)
	if pkg == chiV4Pkg {
		prims.BodyBinders = append(prims.BodyBinders, BodyBinder{Symbol: chiFunc(chiV4RenderPkg, "Bind"), Arg: 1, ContentType: jsonContentType})
		prims.Writers = append(prims.Writers,
			Writer{Symbol: chiFunc(chiV4RenderPkg, "JSON"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			Writer{Symbol: chiFunc(chiV4RenderPkg, "XML"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			Writer{Symbol: chiFunc(chiV4RenderPkg, "PlainText"), BodyArg: 2, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
		)
	}
	return prims
}

func chiRouterSpec(pkg string) RouterSpec {
	types := map[string]bool{
		pkg + ".Mux":    true,
		pkg + ".Router": true,
	}
	verbs := map[string]string{
		"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
		"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS", "Connect": "CONNECT",
		"Trace": "TRACE", "Any": "ALL", "Handle": "ALL", "HandleFunc": "ALL",
		"Method": "ALL", "MethodFunc": "ALL",
	}
	return RouterSpec{RouterTypes: types, ChiPathSyntax: true, GroupMethod: "Group", Verbs: verbs,
		PathArgs:      map[string]int{"Method": 1, "MethodFunc": 1},
		MethodArgs:    map[string]int{"Method": 0, "MethodFunc": 0},
		NestedMethods: map[string]bool{"Group": true, "Route": true}}
}

// ChiV4 is the built-in adapter for github.com/go-chi/chi.
var ChiV4 Framework = routerFramework{
	name: "chi/v4", short: "chi", module: chiV4Pkg,
	spec: chiRouterSpec(chiV4Pkg), prims: chiPrimitives(chiV4Pkg),
}

// ChiV5 is the built-in adapter for github.com/go-chi/chi/v5.
var ChiV5 Framework = routerFramework{
	name: "chi/v5", short: "chi", module: chiV5Pkg,
	spec: chiRouterSpec(chiV5Pkg), prims: chiPrimitives(chiV5Pkg),
}
