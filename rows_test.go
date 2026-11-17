package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func parseUsage(t *testing.T, body string) *UsageLimits {
	t.Helper()
	limits, err := serveBody(t, http.StatusOK, body).GetUsageLimits()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	return limits
}

func rowKeys(rows []usageRow) string {
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.key
	}
	return strings.Join(keys, " ")
}

// The menu lists what the server sent, in its order. Usage credits that are
// switched off still arrive at 0% and must stay off the menu: listed, they
// would read as a meter with headroom.
func TestUsageRowsFollowTheServer(t *testing.T) {
	rows := usageRows(parseUsage(t, liveShapeBody))
	if got, want := rowKeys(rows), "session weekly_all weekly_scoped:Fable"; got != want {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	wantLabels := []string{"5-Hour Session", "Weekly (All)", "Weekly (Fable)"}
	wantPercent := []float64{47, 9, 12}
	for i, row := range rows {
		if row.label != wantLabels[i] || row.limit == nil || row.limit.Utilization != wantPercent[i] {
			t.Errorf("row %d = %q %+v, want %q at %v%%", i, row.label, row.limit, wantLabels[i], wantPercent[i])
		}
	}
	if !rows[0].active || rows[1].active {
		t.Error("the server's pick (session) was not carried onto the rows")
	}
}

// Plans differ in which meters they have. Each shape must produce a usable
// menu and a menu bar number without a release that knows the plan.
func TestUsageRowsAcrossPlanShapes(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantRows     string
		wantHeadline string // "" when there is nothing to show
	}{
		{
			"5-hour limit lifted: weekly rows only, the server picks weekly",
			`{"limits": [
				{"kind": "weekly_all", "percent": 30, "resets_at": null, "scope": null, "is_active": true},
				{"kind": "weekly_scoped", "percent": 5, "resets_at": null,
				 "scope": {"model": {"display_name": "Fable"}}, "is_active": false}]}`,
			"weekly_all weekly_scoped:Fable", "weekly_all",
		},
		{
			"a meter this version has never seen is listed under the server's name",
			`{"limits": [
				{"kind": "monthly_spend", "percent": 62, "resets_at": null, "scope": null, "is_active": true},
				{"kind": "weekly_scoped", "percent": 1, "resets_at": null,
				 "scope": {"model": null, "surface": {"display_name": "Cowork"}}, "is_active": false}]}`,
			"monthly_spend weekly_scoped:Cowork", "monthly_spend",
		},
		{
			"usage-based seat: no plan limits, only credits",
			`{"limits": [],
			  "extra_usage": {"is_enabled": true, "monthly_limit": 50000, "used_credits": 12500, "utilization": 25, "currency": "USD"}}`,
			"extra_usage", "extra_usage",
		},
		{
			"nothing reported at all",
			`{"limits": [], "extra_usage": null}`,
			"", "",
		},
		{
			"idle seat: the session row stays listed at 0%, and with no pick the first row leads",
			idleSeatBody,
			"session weekly_all", "session",
		},
	}
	for _, tc := range cases {
		rows := usageRows(parseUsage(t, tc.body))
		if got := rowKeys(rows); got != tc.wantRows {
			t.Errorf("%s: rows = %q, want %q", tc.name, got, tc.wantRows)
		}
		head, _, ok := headlineRow(rows, autoKey)
		if ok != (tc.wantHeadline != "") || head.key != tc.wantHeadline {
			t.Errorf("%s: headline = %q (ok=%v), want %q", tc.name, head.key, ok, tc.wantHeadline)
		}
		if title := menuBarTitle(head, ok); (title == "⚪ --") != (tc.wantHeadline == "") {
			t.Errorf("%s: menu bar title = %q", tc.name, title)
		}
	}
}

func TestLimitLabelForUnknownKinds(t *testing.T) {
	cases := map[string]LimitEntry{
		"Monthly spend":          {Kind: "monthly_spend"},
		"Daily (Cowork)":         {Kind: "daily", Scope: &LimitScope{Surface: &LimitScopeName{DisplayName: "Cowork"}}},
		"Weekly (Scoped)":        {Kind: kindWeeklyScoped},
		"Limit":                  {},
		"Weekly (Fable)":         {Kind: kindWeeklyScoped, Scope: &LimitScope{Model: &LimitScopeName{DisplayName: "Fable"}}},
		"5-Hour Session":         {Kind: kindSession},
		"Weekly (All)":           {Kind: kindWeeklyAll},
		"Weekly opus (Opus 4.7)": {Kind: "weekly_opus", Scope: &LimitScope{Model: &LimitScopeName{DisplayName: "Opus 4.7"}}},
	}
	for want, entry := range cases {
		if got := limitLabel(entry); got != want {
			t.Errorf("limitLabel(%+v) = %q, want %q", entry, got, want)
		}
	}
}

// Usage credits are money: the row must say how much was spent against the
// limit, in the currency's own minor unit, and must not invent a percentage
// when spend is uncapped.
func TestExtraUsageRow(t *testing.T) {
	num := func(v float64) *float64 { return &v }
	zero := 0

	if _, ok := extraUsageRow(nil); ok {
		t.Error("no extra_usage object produced a row")
	}
	off := &ExtraUsageInfo{IsEnabled: false, MonthlyLimit: num(20000), UsedCredits: num(0), Utilization: num(0), Currency: "USD"}
	if _, ok := extraUsageRow(off); ok {
		t.Error("credits that are switched off produced a row")
	}

	capped, ok := extraUsageRow(&ExtraUsageInfo{IsEnabled: true, MonthlyLimit: num(20000), UsedCredits: num(2400), Utilization: num(12), Currency: "USD"})
	if !ok || capped.limit == nil || capped.limit.Utilization != 12 || capped.note != "24.00 of 200.00 USD" {
		t.Errorf("capped credits = %+v (limit %+v)", capped, capped.limit)
	}

	uncapped, ok := extraUsageRow(&ExtraUsageInfo{IsEnabled: true, UsedCredits: num(2400), Currency: "USD"})
	if !ok || uncapped.limit != nil || uncapped.note != "24.00 USD used" {
		t.Errorf("uncapped credits = %+v (limit %+v)", uncapped, uncapped.limit)
	}

	yen, _ := extraUsageRow(&ExtraUsageInfo{IsEnabled: true, MonthlyLimit: num(20000), UsedCredits: num(2400), Utilization: num(12), Currency: "JPY", DecimalPlaces: &zero})
	if yen.note != "2400 of 20000 JPY" {
		t.Errorf("zero-decimal currency note = %q", yen.note)
	}
}

// A pinned row is shown while the account has it. When it is gone — the plan
// changed, the model was renamed — the menu bar must follow the server's pick
// instead of sitting on "--" until the user notices.
func TestHeadlineRow(t *testing.T) {
	limit := &UsageLimit{Utilization: 1}
	rows := []usageRow{
		{key: "session", limit: limit},
		{key: "weekly_all", limit: limit, active: true},
		{key: "weekly_scoped:Fable", limit: limit},
	}
	cases := []struct {
		name     string
		rows     []usageRow
		pinned   string
		wantKey  string
		wantAuto bool
	}{
		{"auto follows the server's pick", rows, autoKey, "weekly_all", true},
		{"a pinned row wins over the pick", rows, "weekly_scoped:Fable", "weekly_scoped:Fable", false},
		{"a pinned row the account lacks falls back to the pick", rows, "weekly_scoped:Opus", "weekly_all", true},
		{"without a pick, the first row with a percentage", []usageRow{{key: "extra_usage"}, {key: "session", limit: limit}}, autoKey, "session", true},
	}
	for _, tc := range cases {
		row, auto, ok := headlineRow(tc.rows, tc.pinned)
		if !ok || row.key != tc.wantKey || auto != tc.wantAuto {
			t.Errorf("%s: got %q auto=%v ok=%v, want %q auto=%v", tc.name, row.key, auto, ok, tc.wantKey, tc.wantAuto)
		}
	}
	if _, _, ok := headlineRow(nil, "session"); ok {
		t.Error("no rows still produced a headline")
	}
}

// "no active session" explains a 5-hour window that has not opened. On any
// other row a missing reset time means nothing of the sort.
func TestFormatRow(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute)
	cases := []struct {
		name    string
		row     usageRow
		console bool
		want    string // "" to check wantHas only
		wantHas string
	}{
		{"idle session", usageRow{key: kindSession, label: "5-Hour Session", limit: &UsageLimit{}}, false, "5-Hour Session: 0% (no active session)", ""},
		{"weekly at 0% with no reset", usageRow{key: kindWeeklyAll, label: "Weekly (All)", limit: &UsageLimit{}}, false, "Weekly (All): 0%", ""},
		{"credits with spend", usageRow{key: extraUsageKey, label: "Usage Credits", limit: &UsageLimit{Utilization: 12}, note: "24.00 of 200.00 USD"}, false, "Usage Credits: 12% (24.00 of 200.00 USD)", ""},
		{"uncapped credits", usageRow{key: extraUsageKey, label: "Usage Credits", note: "24.00 USD used"}, false, "Usage Credits: -- (24.00 USD used)", ""},
		{"open window names its reset", usageRow{key: kindSession, label: "5-Hour Session", limit: &UsageLimit{Utilization: 41.6, ResetsAt: reset}}, false, "", "5-Hour Session: 42% (resets "},
		{"console pads the percentage", usageRow{key: kindWeeklyAll, label: "Weekly (All)", limit: &UsageLimit{Utilization: 6}}, true, "Weekly (All):    6%", ""},
	}
	for _, tc := range cases {
		got := formatRow(tc.row, tc.console)
		if tc.want != "" && got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if !strings.Contains(got, tc.wantHas) {
			t.Errorf("%s: got %q, want it to contain %q", tc.name, got, tc.wantHas)
		}
	}
}

// The status command shows what the daemon last fetched, read back from the
// usage cache; the round trip must keep everything the rows are built from,
// the server's pick included.
func TestUsageCacheRoundTrip(t *testing.T) {
	limits := parseUsage(t, liveShapeBody)
	data, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	var cached *UsageLimits
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatal(err)
	}
	render := func(u *UsageLimits) string {
		var b strings.Builder
		rows := usageRows(u)
		for _, row := range rows {
			b.WriteString(formatRow(row, true) + "\n")
		}
		head, auto, ok := headlineRow(rows, autoKey)
		b.WriteString(menuBarTitle(head, ok))
		if auto {
			b.WriteString(" auto " + head.key)
		}
		return b.String()
	}
	if got, want := render(cached), render(limits); got != want || !strings.Contains(got, "auto session") {
		t.Errorf("round trip changed the displayed usage:\n got %s\nwant %s", got, want)
	}
}
