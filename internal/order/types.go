// Package order takes POS orders from placement to payment while the device
// is online: open bills, line voids, cancellation, payment, reads, and the
// shift close guard (orders-spec).
package order

import (
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

// Order statuses. Only placed, completed and cancelled are set in this
// slice; the rest are allowed by the schema for later specs (FR-ORD-1).
const (
	StatusDraft          = "draft"
	StatusPendingPayment = "pending_payment"
	StatusPlaced         = "placed"
	StatusPreparing      = "preparing"
	StatusReady          = "ready"
	StatusCompleted      = "completed"
	StatusCancelled      = "cancelled"
	StatusExpired        = "expired"
	StatusRefunded       = "refunded"
)

// allStatuses is every status the schema allows.
var allStatuses = []string{
	StatusDraft, StatusPendingPayment, StatusPlaced, StatusPreparing, StatusReady,
	StatusCompleted, StatusCancelled, StatusExpired, StatusRefunded,
}

const (
	ChannelPOS = "pos"
	ChannelWeb = "web"

	FulfillmentDineIn   = "dine_in"
	FulfillmentTakeaway = "takeaway"
	FulfillmentMixed    = "mixed"

	LineActive = "active"
	LineVoided = "voided"

	MethodQRISStatic = "qris_static"
	MethodCardEDC    = "card_edc"

	PaymentSettled = "settled"
)

// maxLines caps an order's lines, voided ones included (FR-PLC-2, FR-ADD-2).
const maxLines = 100

// StaffRef names a staff member.
type StaffRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Order is an order's header, as lists return it (FR-RD-5).
type Order struct {
	ID                 string     `json:"id"`
	OutletID           string     `json:"outlet_id"`
	ShiftID            string     `json:"shift_id"`
	DeviceID           *string    `json:"device_id"`
	OrderNumber        string     `json:"order_number"`
	Channel            string     `json:"channel"`
	Status             string     `json:"status"`
	FulfillmentSummary string     `json:"fulfillment_summary"`
	BusinessDate       string     `json:"business_date"`
	TableNumber        *string    `json:"table_number"`
	Note               *string    `json:"note"`
	Subtotal           int64      `json:"subtotal"`
	DiscountTotal      int64      `json:"discount_total"`
	ServiceCharge      int64      `json:"service_charge"`
	PackagingFee       int64      `json:"packaging_fee"`
	TaxTotal           int64      `json:"tax_total"`
	GrandTotal         int64      `json:"grand_total"`
	PlacedAt           *time.Time `json:"placed_at"`
	PaidAt             *time.Time `json:"paid_at"`
	CreatedBy          *StaffRef  `json:"created_by"`
	CreatedAt          time.Time  `json:"-"`
}

// Detail is one order with everything on it, as single reads return it.
// unit_cost is stored but never returned (FR-RD-6).
type Detail struct {
	Order
	Lines    []Line    `json:"lines"`
	Payments []Payment `json:"payments"`
	Events   []Event   `json:"events"`
}

type Line struct {
	ID              string     `json:"id"`
	ProductID       string     `json:"product_id"`
	VariantID       *string    `json:"variant_id"`
	ProductName     string     `json:"product_name"`
	VariantName     *string    `json:"variant_name"`
	CategoryID      string     `json:"category_id"`
	CategoryName    string     `json:"category_name"`
	UnitPrice       int64      `json:"unit_price"`
	Quantity        int        `json:"quantity"`
	DiscountAmount  int64      `json:"discount_amount"`
	LineTotal       int64      `json:"line_total"`
	FulfillmentType string     `json:"fulfillment_type"`
	Note            *string    `json:"note"`
	Modifiers       []Modifier `json:"modifiers"`
	Status          string     `json:"status"`
	CreatedBy       *StaffRef  `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	VoidedBy        *StaffRef  `json:"voided_by"`
	VoidedAt        *time.Time `json:"voided_at"`
	VoidReason      *string    `json:"void_reason"`
}

type Modifier struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceDelta int64  `json:"price_delta"`
}

type Payment struct {
	ID          string     `json:"id"`
	Method      string     `json:"method"`
	Amount      int64      `json:"amount"`
	Status      string     `json:"status"`
	ExternalRef *string    `json:"external_ref"`
	RecordedBy  *StaffRef  `json:"recorded_by"`
	SettledAt   *time.Time `json:"settled_at"`
}

// Event is one status change (FR-ORD-4).
type Event struct {
	From   *string   `json:"from"`
	To     string    `json:"to"`
	Reason *string   `json:"reason"`
	Staff  *StaffRef `json:"staff"`
	At     time.Time `json:"at"`
}

// Result is the outcome of a change. Applied is false for a retry that
// changed nothing.
type Result struct {
	Applied bool `json:"applied"`
	*Detail
}

// LineInput is one line as the POS sends it. Prices never come from the
// client (FR-TOT-6).
type LineInput struct {
	ID              string
	ProductID       uuid.UUID
	VariantID       *uuid.UUID
	ModifierIDs     []uuid.UUID
	Quantity        int
	FulfillmentType string
	Note            *string
}

// Actor is the device and the staff member operating it, from the token
// and X-Acting-Staff-Id. Outlet and device never come from the body
// (NFR-SEC-1).
type Actor struct {
	OutletID string
	DeviceID string
	Acting   *domain.StaffAccess
}

// PlaceInput is one placement (FR-PLC-1).
type PlaceInput struct {
	Actor
	ID          string
	TableNumber *string
	Note        *string
	Lines       []LineInput
}

// AddLinesInput adds lines to an open bill (FR-ADD-1).
type AddLinesInput struct {
	Actor
	OrderID string
	Lines   []LineInput
}

// VoidInput voids one line of an open bill (FR-VOID-1).
type VoidInput struct {
	Actor
	OrderID string
	LineID  string
	Reason  string
}

// CancelInput cancels an open bill (FR-CAN-1).
type CancelInput struct {
	Actor
	OrderID string
	Reason  string
}

// PayInput records a payment (FR-PAY-1).
type PayInput struct {
	Actor
	OrderID     string
	PaymentID   string
	Method      string
	Amount      int64
	ExternalRef *string
}

// ListFilter selects an outlet's orders, newest first. Empty fields match
// everything.
type ListFilter struct {
	OutletID    string
	From, To    string // inclusive YYYY-MM-DD
	Status      string
	ShiftID     string
	OrderNumber string
	Limit       int
	After       *cursor
}

// Page is one page of an order listing.
type Page struct {
	Orders     []Order `json:"orders"`
	NextCursor *string `json:"next_cursor"`
}

// LineProblem is one reason a line was refused, in a lines_unavailable
// error (FR-PLC-5). TargetID names the offending variant, modifier or
// modifier group, when there is one.
type LineProblem struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	TargetID *string `json:"target_id,omitempty"`
}

// LinesUnavailable is the detail of a lines_unavailable error.
type LinesUnavailable struct {
	Lines []LineProblem `json:"lines"`
}
