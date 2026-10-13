# Go Coding Standards

## Architecture & File Organization

- Follow the Standard Go Project Layout.

- **`cmd/`**: Contains only the application entry points (e.g., `cmd/server/main.go`). This layer only wires up dependencies, configurations, and starts the server.

- **`internal/`**: Contains the actual private application code.

- **Package by Domain**: Group files by feature/domain (e.g., `orders`, `payments`, `auth`) rather than technical layers.

- **Pragmatic Layering**: Each domain should internally separate concerns:

  - `handler.go`: HTTP request parsing, input validation, and JSON response formatting.

  - `service.go`: Core business rules and logic.

  - `repository.go`: Raw database queries, scanning, and transaction management.

## Naming Conventions

- **Packages**: Short, single-word, lowercase names (e.g., `logistics`, not `logistics_service`).

- **Files**: `snake_case.go` (e.g., `work_orders.go`).

- **Variables & Functions**: `camelCase` for unexported (private) items, `PascalCase` for exported (public) items.

- **Receiver Variables**: Use a one-to-two letter abbreviation of the type (e.g., `func (s *Service) Create(...)` instead of `func (service *Service)`).

- **Interfaces**: Usually end in "er" if they represent an action (e.g., `Reader`, `Writer`).

## Interfaces & Types

- Define interfaces where they are _consumed_, not where they are implemented. Keep them small (1-3 methods).

- Avoid `interface{}` (or `any`) unless strictly necessary for generic data marshaling. Use strong, explicit typing.

- Accept interfaces, return structs.

- Use pointers for structs when passing them to functions to avoid unnecessary memory copying, unless the struct is very small.

## Database & Migrations

- **Driver/Tooling**: Use the standard library `database/sql` package paired with a basic PostgreSQL driver (e.g., `github.com/lib/pq`). No ORMs, no `pgx`, and no `sqlc`.

- **Raw SQL**: Write raw SQL queries as constants or string literals directly within the repository layer.

- **Security**: **Always** use parameterized queries (e.g., `$1, $2`) to prevent SQL injection. Never concatenate strings to build SQL queries.

- **Scanning**: Manually map rows to Go structs using `rows.Scan()`. Use standard `sql.NullString`, `sql.NullInt64`, etc., when dealing with nullable database columns.

- **Transactions**: For operations modifying multiple tables, pass down a context and explicitly manage database transactions (`*sql.Tx` with `BEGIN`, `COMMIT`, `ROLLBACK`) in the repository layer.

## HTTP Handlers & Routing

- Keep handlers incredibly thin. They have three jobs:

    1. Extract and validate input (from JSON body, URL params, or headers).

    2. Call the domain's `Service` layer.

    3. Format the result or error into a JSON HTTP response.

- Business logic never lives in the handler.

- Group routes by domain in a central router configuration within `cmd/server` or an `api` package.

## Error Handling

- **No Panics**: Never use `panic()` for standard control flow. Handle errors explicitly.

- **Return Errors**: Functions that can fail must return an `error` as their last return value.

- **Check Immediately**: Handle errors exactly where they occur using the standard `if err != nil` block.

- **Wrap Errors**: Add context to errors as they bubble up using `fmt.Errorf("scanning user row: %w", err)`. This creates a traceable path for debugging.

- **HTTP Errors**: Never expose raw database or internal system errors directly to the client. Map internal errors to standardized JSON responses with appropriate HTTP status codes (e.g., 400 Bad Request, 500 Internal Server Error).

## Data Validation

- Validate all incoming DTOs (Data Transfer Objects) at the handler layer.

- Use a validation library (like `go-playground/validator`) on struct tags to keep validation rules declarative.

- Ensure the Service layer performs a second pass for _domain-specific_ validation (e.g., checking if there is enough inventory stock before processing a work order).

## Code Quality & Tooling

- Format all code using `gofmt` or `goimports` before committing. No exceptions.

- Run `golangci-lint` to catch common static analysis issues, unused variables, and shadowed variables.

- Keep functions short and focused. If a function exceeds 50 lines, evaluate if it can be refactored into smaller helper functions.

- No unused imports or dead code. Let the Go compiler enforce this.
