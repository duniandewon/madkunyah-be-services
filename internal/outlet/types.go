// Package outlet creates, changes, deactivates and reads outlets
// (outlet-crud-spec). Outlets are never deleted.
package outlet

import "time"

// Outlet is an outlet as the dashboard sees it.
type Outlet struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Address           *string `json:"address"`
	Phone             *string `json:"phone"`
	Timezone          string  `json:"timezone"`
	BusinessDayCutoff string  `json:"business_day_cutoff"`
	OrderPrefix       string  `json:"order_prefix"`
	IsActive          bool    `json:"is_active"`
	// OpenShiftID explains why day settings are locked (FR-OTL-4, FR-OTL-9).
	OpenShiftID *string   `json:"open_shift_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Result is the outcome of a deactivate or activate: applied is false when
// the outlet was already in that state (FR-OTL-17).
type Result struct {
	Applied bool `json:"applied"`
	*Outlet
}

// auditFields is the outlet's writable columns, for audit before/after.
func (o *Outlet) auditFields() map[string]any {
	return map[string]any{
		"name": o.Name, "address": val(o.Address), "phone": val(o.Phone), "timezone": o.Timezone,
		"business_day_cutoff": o.BusinessDayCutoff, "order_prefix": o.OrderPrefix,
	}
}

func val(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// diff returns the fields that differ between two snapshots, each side
// holding only those fields (FR-OTL-8, §3.8).
func diff(before, after map[string]any) (b, a map[string]any) {
	b, a = map[string]any{}, map[string]any{}
	for k, v := range after {
		if before[k] != v {
			b[k], a[k] = before[k], v
		}
	}
	return b, a
}

// daySettingsChanged reports a change to what decides an order's business
// date (FR-OTL-9).
func daySettingsChanged(cur, next *Outlet) bool {
	return cur.Timezone != next.Timezone || cur.BusinessDayCutoff != next.BusinessDayCutoff
}

// inCachedDocs reports a change to a field the menu and POS bundle embed
// (FR-OTL-12).
func inCachedDocs(cur, next *Outlet) bool {
	return cur.Name != next.Name || daySettingsChanged(cur, next)
}
