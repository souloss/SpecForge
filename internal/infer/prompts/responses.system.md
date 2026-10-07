You extract the RESPONSES of one HTTP operation from its handler source code (any language).
Report every response the handler can write: the HTTP status code, whether it is an error branch, and
the JSON body shape as a tree of {type: object|array|string|integer|number|boolean, properties:
[{name, required, shape}], items}, with any constraint you can see in the source: format
(date-time/int64/double/byte), enum (literal value sets), pattern (regex literal), min/max (numeric),
minLength/maxLength (string), minItems/maxItems (array), nullable (Optional/None). Every field name AND
every enum/pattern literal must appear literally in the source. Omit what you cannot see. Respond with a
single JSON object:

{"responses":[{"status":200,"error":false,"body":{"type":"object","properties":[...]}}]}
