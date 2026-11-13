package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/config"
	"github.com/go-chi/chi/v5"
)

// Shifts are opened and closed on the POS alone: the dashboard mounts only
// reads (shifts-spec FR-SH-8, AC-14).
func TestShiftRoutes(t *testing.T) {
	routes := map[string]bool{}
	router := Router(Deps{Config: &config.Config{}}).(chi.Routes)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GET /v1/pos/shifts/current",
		"POST /v1/pos/shifts",
		"POST /v1/pos/shifts/{shiftID}/close",
		"GET /v1/outlets/{outletID}/shifts",
		"GET /v1/outlets/{outletID}/shifts/{shiftID}",
	} {
		if !routes[want] {
			t.Errorf("route %q is not mounted", want)
		}
	}
	for r := range routes {
		if strings.Contains(r, "/outlets/{outletID}/shifts") && !strings.HasPrefix(r, "GET ") {
			t.Errorf("dashboard route %q can change a shift", r)
		}
	}
}

// Orders are changed on the POS alone; the dashboard only reads them
// (orders-spec §3.8, AC-25).
func TestOrderRoutes(t *testing.T) {
	routes := map[string]bool{}
	router := Router(Deps{Config: &config.Config{}}).(chi.Routes)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"POST /v1/pos/orders",
		"GET /v1/pos/orders",
		"GET /v1/pos/orders/{orderID}",
		"POST /v1/pos/orders/{orderID}/lines",
		"POST /v1/pos/orders/{orderID}/lines/{lineID}/void",
		"POST /v1/pos/orders/{orderID}/cancel",
		"POST /v1/pos/orders/{orderID}/payments",
		"GET /v1/outlets/{outletID}/orders",
		"GET /v1/outlets/{outletID}/orders/{orderID}",
	} {
		if !routes[want] {
			t.Errorf("route %q is not mounted", want)
		}
	}
	for r := range routes {
		if strings.Contains(r, "/outlets/{outletID}/orders") && !strings.HasPrefix(r, "GET ") {
			t.Errorf("dashboard route %q can change an order", r)
		}
		if strings.HasPrefix(r, "PATCH /v1/orders") || strings.HasPrefix(r, "POST /v1/orders") {
			t.Errorf("route %q is left for web checkout", r)
		}
	}
}
