package order

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// downDB answers every query as a database that cannot be reached.
type downDB struct{}

var errDown = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

func (downDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errDown }
func (downDB) QueryRow(context.Context, string, ...any) pgx.Row        { return downRow{} }

type downRow struct{}

func (downRow) Scan(...any) error { return errDown }

// §5.1: a catalog that cannot be read is catalog_unavailable, not an
// internal error.
func TestPriceLinesCatalogUnavailable(t *testing.T) {
	_, err := priceLines(context.Background(), downDB{}, uuid.NewString(), time.Now(),
		[]LineInput{{ID: uuid.NewString(), ProductID: uuid.New(), Quantity: 1, FulfillmentType: FulfillmentDineIn}})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeCatalogUnavailable || !errors.Is(err, errDown) {
		t.Fatalf("error = %v", err)
	}
}
