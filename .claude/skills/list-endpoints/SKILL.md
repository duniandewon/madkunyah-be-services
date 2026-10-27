---
name: list-endpoints
description: List API endpoints with request headers and body
argument-hint: [route-prefix or subdirectory]
model: haiku
---

## Task

Find every API endpoint built in this project and list its method, path, headers and request body.

Routes are registered with chi in `internal/server/server.go` (follow its `Route`/`Mount`/`With` calls for prefixes and middleware); handlers live in `internal/*/handler.go`, and middleware that reads headers is in `internal/middleware/`. Start from `server.go` and read only the handler and middleware code it points to. Do not spawn subagents.

If a [route-prefix or subdirectory] is provided via $ARGUMENTS, only list endpoints whose path starts with that prefix or whose files are in that subdirectory.

For each endpoint, work out:

- **Method and full path**: include router/controller prefixes, mounted paths and any global API prefix
- **Headers**: headers read in the handler, plus those required by middleware, guards or interceptors applied to the route (e.g. `Authorization` from auth middleware, `Content-Type`, custom `x-*` headers)
- **Path and query params**: names, and types where declared
- **Request body**: fields with type and required/optional, taken from DTOs, validation schemas (zod, joi, yup, class-validator, pydantic, serializers), TypeScript types, or how the handler reads the body. Write "None" for endpoints that take no body

Only report what the code shows. If something can't be determined, write "Not determined" rather than guessing.

## Output Format

Group endpoints by resource or file. For each endpoint:

### N. `METHOD /full/path`

- **File:** relative/path/to/file:line
- **Description:** one line, inferred from the handler or route name
- **Headers:** list, or "None"
- **Path/Query params:** list, or "None"
- **Request body:**

```json
{
  "field": "type (required)",
  "otherField": "type (optional)"
}
```

End with a summary: total number of endpoints, and a count per HTTP method.

If no endpoints found, say "No endpoints found."
