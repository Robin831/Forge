package cost

import (
	"errors"
	"strings"
	"testing"
)

// fakeSink records what Record wrote and can be told to fail one table, so a
// test can assert both the fan-out and what happens when part of it breaks.
type fakeSink struct {
	daily    []call
	provider []call
	bead     []call
	failOn   string
}

type call struct {
	key                                  string
	input, output, cacheRead, cacheWrite int
	cost                                 float64
}

func (f *fakeSink) fail(table string) error {
	if f.failOn == table {
		return errors.New("disk on fire")
	}
	return nil
}

func (f *fakeSink) AddDailyCost(date string, in, out, cr, cw int, c float64) error {
	f.daily = append(f.daily, call{date, in, out, cr, cw, c})
	return f.fail("daily")
}

func (f *fakeSink) AddProviderDailyCost(date, prov string, in, out, cr, cw int, c float64) error {
	f.provider = append(f.provider, call{date + "/" + prov, in, out, cr, cw, c})
	return f.fail("provider")
}

func (f *fakeSink) AddBeadCost(beadID, anvil string, in, out, cr, cw int, c float64) error {
	f.bead = append(f.bead, call{beadID + "/" + anvil, in, out, cr, cw, c})
	return f.fail("bead")
}

// TestRecordFansOutToAllThreeTables pins the point of the helper: one session's
// usage reaches the daily aggregate, the per-provider daily aggregate and the
// bead's row, with the same counts in each — cache columns included, since
// those are the ones every call site used to pass as a literal 0.
func TestRecordFansOutToAllThreeTables(t *testing.T) {
	sink := &fakeSink{}
	u := Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 41500, CacheWriteTokens: 900, EstimatedCostUSD: 0.25}

	if err := Record(sink, "claude", "Forge-ogty", "forge", u); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if len(sink.daily) != 1 || len(sink.provider) != 1 || len(sink.bead) != 1 {
		t.Fatalf("want one write per table; got daily=%d provider=%d bead=%d",
			len(sink.daily), len(sink.provider), len(sink.bead))
	}
	want := call{"", 100, 50, 41500, 900, 0.25}
	for name, got := range map[string]call{"daily": sink.daily[0], "provider": sink.provider[0], "bead": sink.bead[0]} {
		got.key = ""
		if got != want {
			t.Errorf("%s write = %+v; want %+v", name, got, want)
		}
	}
	if sink.daily[0].key != Today() {
		t.Errorf("daily date = %q; want today", sink.daily[0].key)
	}
	if sink.provider[0].key != Today()+"/claude" {
		t.Errorf("provider key = %q; want today/claude", sink.provider[0].key)
	}
	if sink.bead[0].key != "Forge-ogty/forge" {
		t.Errorf("bead key = %q; want Forge-ogty/forge", sink.bead[0].key)
	}
}

// TestRecordAttemptsEveryTableWhenOneFails keeps a broken table from taking the
// other two with it: they are independent, and losing a bead's row because the
// daily aggregate failed would be a worse outcome than the failure itself. The
// error still names the table that broke.
func TestRecordAttemptsEveryTableWhenOneFails(t *testing.T) {
	sink := &fakeSink{failOn: "daily"}

	err := Record(sink, "claude", "Forge-ogty", "forge", Usage{InputTokens: 1})
	if err == nil {
		t.Fatal("Record succeeded; want the daily write's error")
	}
	if !strings.Contains(err.Error(), "daily_costs") {
		t.Errorf("err = %q; want it to name daily_costs", err.Error())
	}
	if len(sink.provider) != 1 || len(sink.bead) != 1 {
		t.Errorf("the other two tables were skipped: provider=%d bead=%d", len(sink.provider), len(sink.bead))
	}
}

// TestRecordSkipsZeroUsage keeps a stage that never reached a session — or one
// whose spawn was refused as rate limited — off the books entirely, rather than
// writing three rows of zeros.
func TestRecordSkipsZeroUsage(t *testing.T) {
	sink := &fakeSink{}
	if err := Record(sink, "claude", "Forge-ogty", "forge", Usage{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(sink.daily)+len(sink.provider)+len(sink.bead) != 0 {
		t.Error("a zero usage wrote something")
	}
	if err := Record(nil, "claude", "Forge-ogty", "forge", Usage{InputTokens: 1}); err != nil {
		t.Fatalf("nil sink should be a no-op, got %v", err)
	}
}

// TestUsageIsZeroCountsCacheTokens covers the session served almost entirely
// from a prompt cache: input, output and cost can all round to nothing while
// tens of thousands of tokens were read from the prefix, and treating that as
// empty would drop exactly the sessions the cache columns exist to show.
func TestUsageIsZeroCountsCacheTokens(t *testing.T) {
	if !(Usage{}).IsZero() {
		t.Error("the zero Usage is not zero")
	}
	for _, u := range []Usage{
		{CacheReadTokens: 32000},
		{CacheWriteTokens: 900},
		{InputTokens: 1},
		{OutputTokens: 1},
		{EstimatedCostUSD: 0.01},
	} {
		if u.IsZero() {
			t.Errorf("%+v reported zero", u)
		}
	}
}
