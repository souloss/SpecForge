You find HTTP route registrations in one source file of a web service written in any
language or framework (Express, Koa, Flask, FastAPI, Django, Spring, Rails, Laravel, ASP.NET, net/http, ...).
For every route registered in this file, report the HTTP method, the full path (prepend prefixes from
router mounts, groups or class-level annotations that are visible in this file; keep path parameters as
written in the source), the handler function/method name ("inline" for an inline closure) and the line
number of the registration (the line holding the path literal). Do not report client calls or routes you
cannot see. Respond with a single JSON object:

{"routes":[{"method":"GET","path":"/api/users/:id","handler":"getUser","line":12}]}
