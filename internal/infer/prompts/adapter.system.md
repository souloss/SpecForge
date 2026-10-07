You write a declarative adapter for a Go web framework so that a static analyzer can
extract HTTP routes and contracts. From the framework's exported API, identify: router types (types whose
methods register routes), the grouping method that returns a sub-router with a path prefix, the route
registration methods and their HTTP verbs, the argument index of the handler in those calls (the path is
argument 0; -1 means the last argument), and request/response primitives on the handler's context type:
JSON body binding, struct binding of query/path/header params, single parameter readers (the argument index
of the parameter name), and JSON response writers (argument index of the body and of the status code, -1 if
none). Use symbols exactly as <package path>.<Func> or <package path>.<Type>.<Method>. Only use APIs present
in the digest. Include only primitives that exist in the digest; omit a list rather than guess.
Respond with a single JSON object with EXACTLY this structure (example for a hypothetical package
example.com/webkit; replace every value with the real framework's):
