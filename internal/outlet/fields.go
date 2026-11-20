package outlet

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Field limits and defaults (outlet-crud-spec §3.1).
const (
	maxName    = 100
	maxAddress = 500
	maxPhone   = 30

	defaultTimezone = "Asia/Jakarta"
	defaultCutoff   = "04:00"
	defaultPrefix   = "POS"
	// webPrefix is the web order sequence's prefix (shifts-spec).
	webPrefix = "WEB"
)

var (
	cutoffPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	prefixPattern = regexp.MustCompile(`^[A-Z0-9]{1,8}$`)
)

// Opt is one body field: absent, null, or a value.
type Opt struct {
	Set  bool
	Null bool
	V    string
}

// Value is a present, non-null field.
func Value(v string) Opt { return Opt{Set: true, V: v} }

// Fields is a create or PATCH body. Absent fields keep their value (or
// take the default on create); null clears address and phone and is
// refused on the rest (FR-OTL-7).
type Fields struct {
	Name, Address, Phone, Timezone, BusinessDayCutoff, OrderPrefix Opt
}

// normalize trims and canonicalises every present field in place and
// returns the field errors, empty when the body is valid.
func (f *Fields) normalize(create bool) map[string]string {
	bad := map[string]string{}
	if create && !f.Name.Set {
		bad["name"] = "is required"
	}
	required(&f.Name, "name", bad, func(v string) (string, string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return v, "must not be empty"
		}
		return v, maxLen(v, maxName)
	})
	optional(&f.Address, "address", maxAddress, bad)
	optional(&f.Phone, "phone", maxPhone, bad)
	required(&f.Timezone, "timezone", bad, func(v string) (string, string) {
		v = strings.TrimSpace(v)
		if !validTimezone(v) {
			return v, "must be an IANA timezone name such as Asia/Jakarta"
		}
		return v, ""
	})
	required(&f.BusinessDayCutoff, "business_day_cutoff", bad, func(v string) (string, string) {
		v = strings.TrimSpace(v)
		if !cutoffPattern.MatchString(v) {
			return v, "must be a time as HH:MM"
		}
		return v, ""
	})
	required(&f.OrderPrefix, "order_prefix", bad, func(v string) (string, string) {
		v = strings.ToUpper(strings.TrimSpace(v))
		switch {
		case !prefixPattern.MatchString(v):
			return v, "must be 1 to 8 letters or digits"
		case v == webPrefix:
			return v, "WEB is reserved for web orders"
		}
		return v, ""
	})
	return bad
}

// required checks a non-nullable field with check, which returns the
// canonical value and an error message.
func required(o *Opt, name string, bad map[string]string, check func(string) (string, string)) {
	if !o.Set {
		return
	}
	if o.Null {
		bad[name] = "must not be null"
		return
	}
	var msg string
	if o.V, msg = check(o.V); msg != "" {
		bad[name] = msg
	}
}

// optional checks a nullable text field; an empty value clears it.
func optional(o *Opt, name string, limit int, bad map[string]string) {
	if !o.Set || o.Null {
		return
	}
	if o.V = strings.TrimSpace(o.V); o.V == "" {
		o.Null = true
		return
	}
	if msg := maxLen(o.V, limit); msg != "" {
		bad[name] = msg
	}
}

func maxLen(v string, limit int) string {
	if utf8.RuneCountInString(v) > limit {
		return "must be at most " + strconv.Itoa(limit) + " characters"
	}
	return ""
}

// validTimezone accepts IANA names only. LoadLocation also takes "" and
// "Local" (both meaning something else) and is given nothing else odd: it
// refuses offsets such as +07:00.
func validTimezone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// apply returns a copy of o with the present fields set. Call it only on
// normalized fields.
func (f *Fields) apply(o *Outlet) *Outlet {
	next := *o
	set := func(dst *string, v Opt) {
		if v.Set {
			*dst = v.V
		}
	}
	setNullable := func(dst **string, v Opt) {
		switch {
		case !v.Set:
		case v.Null:
			*dst = nil
		default:
			s := v.V
			*dst = &s
		}
	}
	set(&next.Name, f.Name)
	setNullable(&next.Address, f.Address)
	setNullable(&next.Phone, f.Phone)
	set(&next.Timezone, f.Timezone)
	set(&next.BusinessDayCutoff, f.BusinessDayCutoff)
	set(&next.OrderPrefix, f.OrderPrefix)
	return &next
}
