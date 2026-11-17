// rows.go - The usage meters an account has, as the menu lists them.
//
// Which meters exist depends on the plan: Pro and Max have a 5-hour session
// and a weekly window, some plans add a weekly window for one model, an
// account whose 5-hour limit has been lifted has only weekly ones, and a
// usage-based seat has no plan limits at all. The usage endpoint says which
// apply in limits[], so the rows are built from what the server sent rather
// than from a fixed list — a plan or meter added later needs no release.

package main

import (
	"fmt"
	"math"
	"strings"
)

const (
	// autoKey is the saved selection that follows the server's pick.
	autoKey = "auto"

	// extraUsageKey identifies the usage credits row, which comes from
	// extra_usage rather than limits[].
	extraUsageKey = "extra_usage"

	// The limits[] kinds this monitor knows by name. The 5-hour and weekly
	// windows the status line reports are these two.
	kindSession      = "session"
	kindWeeklyAll    = "weekly_all"
	kindWeeklyScoped = "weekly_scoped"

	// defaultDecimalPlaces applies when extra_usage does not say how many
	// minor units make one unit of its currency.
	defaultDecimalPlaces = 2
)

// usageRow is one line of the menu.
type usageRow struct {
	key    string // identifies the row in the saved selection
	label  string
	limit  *UsageLimit // nil when the row has no percentage to show
	note   string      // extra detail, e.g. spend against the monthly limit
	active bool        // the server's pick for a single-value display
}

// usageRows lists the meters in a usage response, in the server's order, with
// usage credits last.
func usageRows(u *UsageLimits) []usageRow {
	var rows []usageRow
	seen := make(map[string]bool)
	for _, entry := range u.Limits {
		// A key selects one row, so a second entry with the same kind and
		// scope could never be told apart from the first.
		key := limitKey(entry)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, usageRow{
			key:    key,
			label:  limitLabel(entry),
			limit:  &UsageLimit{Utilization: entry.Percent, ResetsAt: entry.ResetsAt},
			active: entry.IsActive,
		})
	}
	if row, ok := extraUsageRow(u.ExtraUsage); ok {
		rows = append(rows, row)
	}
	return rows
}

func scopeName(scope *LimitScope) string {
	switch {
	case scope == nil:
		return ""
	case scope.Model != nil && scope.Model.DisplayName != "":
		return scope.Model.DisplayName
	case scope.Surface != nil:
		return scope.Surface.DisplayName
	}
	return ""
}

// limitKey identifies an entry by its kind and scope — never by its label,
// which is this monitor's wording and may change.
func limitKey(entry LimitEntry) string {
	if name := scopeName(entry.Scope); name != "" {
		return entry.Kind + ":" + name
	}
	return entry.Kind
}

// limitLabel names an entry. A kind this monitor has not seen is shown under
// the server's own name for it, so a new meter is listed rather than dropped.
func limitLabel(entry LimitEntry) string {
	name := scopeName(entry.Scope)
	switch entry.Kind {
	case kindSession:
		return "5-Hour Session"
	case kindWeeklyAll:
		return "Weekly (All)"
	case kindWeeklyScoped:
		if name == "" {
			name = "Scoped"
		}
		return "Weekly (" + name + ")"
	}
	label := strings.ReplaceAll(entry.Kind, "_", " ")
	if label == "" {
		label = "Limit"
	}
	label = strings.ToUpper(label[:1]) + label[1:]
	if name != "" {
		label += " (" + name + ")"
	}
	return label
}

// extraUsageRow reports usage credits while they are in use. With credits off
// or exhausted the endpoint still sends the object, at 0%; listing that would
// read as a meter with headroom.
func extraUsageRow(x *ExtraUsageInfo) (usageRow, bool) {
	if x == nil || !x.IsEnabled {
		return usageRow{}, false
	}
	row := usageRow{key: extraUsageKey, label: "Usage Credits", note: spendNote(x)}
	if x.Utilization != nil {
		row.limit = &UsageLimit{Utilization: *x.Utilization}
	}
	return row, true
}

// spendNote is the credits spent this month, against the monthly limit when
// there is one: "24.00 of 200.00 USD".
func spendNote(x *ExtraUsageInfo) string {
	if x.UsedCredits == nil {
		return ""
	}
	places := defaultDecimalPlaces
	if x.DecimalPlaces != nil && *x.DecimalPlaces >= 0 {
		places = *x.DecimalPlaces
	}
	amount := func(minor float64) string {
		return fmt.Sprintf("%.*f", places, minor/math.Pow10(places))
	}
	note := amount(*x.UsedCredits)
	if x.MonthlyLimit != nil {
		note += " of " + amount(*x.MonthlyLimit)
	}
	if x.Currency != "" {
		note += " " + x.Currency
	}
	if x.MonthlyLimit == nil {
		note += " used"
	}
	return note
}

// headlineRow picks the row the menu bar shows: the pinned one while the
// account has it, otherwise the server's pick. A pinned row can go away — the
// plan changed, a model was renamed — and following the server then beats a
// menu bar stuck on "--". auto reports that the server's pick is being
// followed; ok is false when there is no row to show.
func headlineRow(rows []usageRow, pinned string) (row usageRow, auto, ok bool) {
	if pinned != autoKey {
		for _, r := range rows {
			if r.key == pinned {
				return r, false, true
			}
		}
	}
	for _, r := range rows {
		if r.active {
			return r, true, true
		}
	}
	// No pick from the server: the first meter with a percentage.
	for _, r := range rows {
		if r.limit != nil {
			return r, true, true
		}
	}
	return usageRow{}, true, false
}
