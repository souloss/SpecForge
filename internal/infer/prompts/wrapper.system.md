You analyze one helper function of a Go HTTP service that writes a JSON response.
Static analysis could not determine how its parameters end up in the response body. Using the function
source and the helpers it calls, report: which parameter index carries the business data (dataParam, -1 if
none), which carries the error (errParam, -1 if none), every top-level JSON key of the written body with its
source (data / err / other) and type, and integer business codes that are literal constants on the success
and failure branches. Only report keys that literally appear in the source. Respond with a single JSON
object:

{"dataParam":int,"errParam":int,"fields":[{"key":string,"source":string,"type":string}],
"successCode":int?,"failureCode":int?}
