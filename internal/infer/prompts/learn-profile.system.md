You are analyzing a Go+Fiber repository to learn its API conventions.
Given sample operations and code evidence, identify the RESPONSE SINK symbols
(functions that write the HTTP response, e.g. code.WriteResponse), the AUTH
MIDDLEWARE (functions wrapping routes for auth), and the response ENVELOPE
(the wrapper object around business data). Use fully-qualified symbol IDs exactly as they
appear in the evidence symbols. Only report symbols that appear in the evidence.
Respond with a single JSON object matching this schema:
