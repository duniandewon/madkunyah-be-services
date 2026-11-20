package outlet

import (
	"strings"
	"testing"
)

// AC-3 (unit): every field rule in spec §3.1.
func TestNormalize(t *testing.T) {
	null := Opt{Set: true, Null: true}
	for _, tt := range []struct {
		name   string
		f      Fields
		create bool
		field  string // the field refused, "" when valid
	}{
		{"name required on create", Fields{}, true, "name"},
		{"name optional on patch", Fields{}, false, ""},
		{"blank name", Fields{Name: Value("   ")}, false, "name"},
		{"null name", Fields{Name: null}, false, "name"},
		{"name at the limit", Fields{Name: Value(strings.Repeat("é", 100))}, false, ""},
		{"name too long", Fields{Name: Value(strings.Repeat("a", 101))}, false, "name"},
		{"null address clears", Fields{Address: null}, false, ""},
		{"address too long", Fields{Address: Value(strings.Repeat("a", 501))}, false, "address"},
		{"phone too long", Fields{Phone: Value(strings.Repeat("1", 31))}, false, "phone"},
		{"IANA timezone", Fields{Timezone: Value("Asia/Makassar")}, false, ""},
		{"UTC", Fields{Timezone: Value("UTC")}, false, ""},
		{"Local refused", Fields{Timezone: Value("Local")}, false, "timezone"},
		{"empty timezone", Fields{Timezone: Value("")}, false, "timezone"},
		{"offset refused", Fields{Timezone: Value("+07:00")}, false, "timezone"},
		{"unknown zone", Fields{Timezone: Value("Asia/Atlantis")}, false, "timezone"},
		{"null timezone", Fields{Timezone: null}, false, "timezone"},
		{"cutoff", Fields{BusinessDayCutoff: Value("23:59")}, false, ""},
		{"cutoff midnight", Fields{BusinessDayCutoff: Value("00:00")}, false, ""},
		{"cutoff 24:00", Fields{BusinessDayCutoff: Value("24:00")}, false, "business_day_cutoff"},
		{"cutoff without zero", Fields{BusinessDayCutoff: Value("4:00")}, false, "business_day_cutoff"},
		{"cutoff with seconds", Fields{BusinessDayCutoff: Value("04:00:00")}, false, "business_day_cutoff"},
		{"prefix", Fields{OrderPrefix: Value("KMG1")}, false, ""},
		{"prefix too long", Fields{OrderPrefix: Value("ABCDEFGHI")}, false, "order_prefix"},
		{"prefix with a dash", Fields{OrderPrefix: Value("POS-1")}, false, "order_prefix"},
		{"empty prefix", Fields{OrderPrefix: Value("")}, false, "order_prefix"},
		{"WEB reserved", Fields{OrderPrefix: Value("web")}, false, "order_prefix"},
	} {
		bad := tt.f.normalize(tt.create)
		switch {
		case tt.field == "" && len(bad) > 0:
			t.Errorf("%s: refused %v", tt.name, bad)
		case tt.field != "" && (len(bad) != 1 || bad[tt.field] == ""):
			t.Errorf("%s: want only %s refused, got %v", tt.name, tt.field, bad)
		}
	}
}

// E-2: values are canonicalised before they are compared or stored.
func TestNormalizeCanonical(t *testing.T) {
	f := Fields{Name: Value("  Kemang "), Address: Value("  "), Phone: Value(" 021 "),
		Timezone: Value(" Asia/Jakarta "), OrderPrefix: Value(" pos ")}
	if bad := f.normalize(true); len(bad) > 0 {
		t.Fatal(bad)
	}
	addr := "old"
	o := f.apply(&Outlet{Address: &addr, OrderPrefix: "X"})
	if o.Name != "Kemang" || o.Address != nil || o.Phone == nil || *o.Phone != "021" ||
		o.Timezone != "Asia/Jakarta" || o.OrderPrefix != "POS" {
		t.Fatalf("outlet = %+v", o)
	}
}

func TestApplyKeepsAbsentFields(t *testing.T) {
	phone := "021"
	cur := &Outlet{Name: "A", Phone: &phone, Timezone: "Asia/Jakarta", BusinessDayCutoff: "04:00", OrderPrefix: "POS"}
	next := (&Fields{Name: Value("B")}).apply(cur)
	if next.Name != "B" || next.Phone != cur.Phone || next.Timezone != cur.Timezone || cur.Name != "A" {
		t.Fatalf("next = %+v, cur = %+v", next, cur)
	}
	b, a := diff(cur.auditFields(), next.auditFields())
	if len(a) != 1 || a["name"] != "B" || b["name"] != "A" {
		t.Fatalf("diff = %v → %v", b, a)
	}
}
