package authz

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
)

// matrix is the v1 permission matrix from auth-spec FR-AZ-1: for each
// permission, the roles that hold it.
var matrix = map[string][]string{
	"order.create":                {"cashier", "supervisor", "manager", "owner"},
	"payment.record":              {"cashier", "supervisor", "manager", "owner"},
	"order.void.unpaid":           {"cashier", "supervisor", "manager", "owner"},
	"product.sold_out.toggle":     {"cashier", "supervisor", "manager", "owner"},
	"order.void.paid":             {"supervisor", "manager", "owner"},
	"order.discount":              {"supervisor", "manager", "owner"},
	"order.price_override":        {"supervisor", "manager", "owner"},
	"shift.open":                  {"supervisor", "manager", "owner"},
	"shift.close":                 {"supervisor", "manager", "owner"},
	"shift.report.view":           {"supervisor", "manager", "owner"},
	"refund.request":              {"supervisor", "manager", "owner"},
	"refund.approve":              {"manager", "owner"},
	"product.availability.manage": {"manager", "owner"},
	"stock.manage":                {"manager", "owner"},
	"staff.manage":                {"manager", "owner"},
	"device.enroll":               {"manager", "owner"},
	"device.revoke":               {"manager", "owner"},
	"report.outlet.view":          {"manager", "owner"},
	"report.all.view":             {"owner"},
	"cost.view":                   {"owner"},
	"margin.view":                 {"owner"},
	"catalog.manage":              {"owner"},
	"outlet.manage":               {"owner"},
	"audit.view":                  {"owner"},
	"auth_events.view":            {"owner"},
}

// AC-26: every role × permission cell of the seeded roles table matches the
// spec, and no role carries a permission outside the matrix. Runs against a
// migrated database named by TEST_DATABASE_URL.
func TestPermissionMatrix(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := postgres.Connect(ctx, postgres.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Pool.Query(ctx, `SELECT code, permissions FROM roles`)
	if err != nil {
		t.Fatal(err)
	}
	seeded := map[string][]string{}
	for rows.Next() {
		var code string
		var perms []string
		if err := rows.Scan(&code, &perms); err != nil {
			t.Fatal(err)
		}
		seeded[code] = perms
	}
	rows.Close()

	roles := []string{"cashier", "supervisor", "manager", "owner"}
	for _, role := range roles {
		if _, ok := seeded[role]; !ok {
			t.Fatalf("role %q not seeded", role)
		}
		for _, p := range seeded[role] {
			if _, ok := matrix[p]; !ok {
				t.Errorf("%s has permission %q outside the matrix", role, p)
			}
		}
	}
	for perm, holders := range matrix {
		for _, role := range roles {
			want := slices.Contains(holders, role)
			if got := slices.Contains(seeded[role], perm); got != want {
				t.Errorf("%s × %s = %v, want %v", role, perm, got, want)
			}
		}
	}
}
