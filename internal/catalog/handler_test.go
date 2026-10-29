package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NFR-AV-1: with Postgres unreachable, catalog routes return 503
// catalog_unavailable.
func TestPostgresDownIsUnavailable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h := NewHandler(NewService(NewRepository(&postgres.DB{Pool: pool}), 100))

	rec := httptest.NewRecorder()
	h.ListCategories(rec, httptest.NewRequest(http.MethodGet, "/v1/categories", nil))

	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusServiceUnavailable || body.Error.Code != "catalog_unavailable" {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
}
