package httpapi

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// A whole-house fixture, served over a real socket, for consumer-side research.
//
// Everything else in this package tests one behaviour against a hand-built
// fixture. This builds the opposite: ONE plausible home with four months of
// telemetry and seven months of half-hourly prices behind the real mux, so that
// a question can be asked of the API the way a consumer asks it — over HTTP,
// with no access to the fixture that answers it.
//
// It exists because issue #36 is consumer feedback, and the only honest way to
// judge consumer feedback is to be a consumer. The scripts under
// scripts/consumers/ talk to this and to nothing else.
//
// It is skipped unless CH_HARNESS=1, so `make test` neither runs a server nor
// pays for the fixture. The fixture builder itself is exercised by
// TestConsumerFixtureIsPlausible below, which DOES run in CI — a fixture that
// has quietly stopped producing a priceable week would make every consumer
// finding worthless, and that should go red rather than go unnoticed.
// ---------------------------------------------------------------------------

// chNow is the harness's fixed present. Every window the consumer scripts ask
// for resolves against this, so their output is reproducible.
var chNow = time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC) // 09:00 BST

const (
	chTariffCode = "E-1R-AGILE-24-10-01-C"
	// chSwitch is when the home moved from the fixed tariff to the half-hourly
	// one. Before it there is no curve to join to, which is friction point #2.
	chSwitchDay = "2026-03-01"
	// chAutomation is when load-shifting automation was switched on. The
	// dishwasher, washer and immersion move to the cheapest overnight slots from
	// this day, so "has load shifted in response to price?" has a real answer in
	// the data rather than being undetectable either way.
	chAutomationDay = "2026-07-15"
	// chTelemetryDays is how much device history the fixture holds. 120 days
	// covers both sides of the automation change with room to spare.
	chTelemetryDays = 120
)

func chLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load Europe/London: %v", err)
	}
	return loc
}

func chDay(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// --- the home ---------------------------------------------------------------

func chDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device", Room: "ground.kitchen", Floor: "ground", DisplayName: "Wine Fridge"},
		"dishwasher": {Class: "cycle_power_device", Room: "ground.kitchen", Floor: "ground", DisplayName: "Dishwasher"},
		"washer":     {Class: "cycle_power_device", Room: "ground.utility", Floor: "ground", DisplayName: "Washing Machine"},
		"dryer":      {Class: "cycle_power_device", Room: "ground.utility", Floor: "ground", DisplayName: "Tumble Dryer"},
		"immersion": {Class: "continuous_power_device", Room: "ground.utility", Floor: "ground",
			DisplayName: "Immersion Heater", Covers: config.CoverageHouse},
		"media":      {Class: "media_power_device", Room: "ground.lounge", Floor: "ground", DisplayName: "Lounge Media"},
		"office-ups": {Class: "ups_sensor", Room: "first.office", Floor: "first", DisplayName: "Office UPS"},
		"electricity_meter": {Class: "energy_meter", Room: "ground.meter", Floor: "ground",
			DisplayName: "Electricity Meter", Covers: config.CoverageHouse},
	}
}

// chCounterDevices are the plug-class devices with a hardware counter, in the
// order they are registered (which is the order rows come back in).
var chCounterDevices = []string{"winefridge", "dishwasher", "washer", "dryer", "immersion", "media"}

// chWatts is the fixture's load model: what device draws at instant t.
//
// Deterministic and closed-form rather than sampled from anything, so the whole
// fixture is a pure function of the clock and a consumer finding can be traced
// back to a line here.
func chWatts(device string, t time.Time, loc *time.Location, cheapestHour int) float64 {
	lt := t.In(loc)
	hour := lt.Hour()
	min := lt.Minute()
	dow := int(lt.Weekday())
	doy := lt.YearDay()

	switch device {
	case "winefridge":
		// Compressor duty cycle: ~45 W average, on for 20 of every 60 minutes.
		if (min/20)%3 == 0 {
			return 135
		}
		return 2

	case "dishwasher":
		// One 2-hour cycle a day, 6 days in 7. Starts at 19:00 before the
		// automation, in the cheapest overnight slot after it.
		if dow == 0 {
			return 0
		}
		start := 19
		if cheapestHour >= 0 {
			start = cheapestHour
		}
		return chCycle(hour, min, start, 120, []float64{1900, 120, 90, 1700})

	case "washer":
		// Four cycles a week (Mon/Wed/Fri/Sat), 90 minutes.
		if dow != 1 && dow != 3 && dow != 5 && dow != 6 {
			return 0
		}
		start := 18
		if cheapestHour >= 0 {
			start = cheapestHour
		}
		return chCycle(hour, min, start, 90, []float64{2100, 200, 150})

	case "dryer":
		// Twice a week, unshifted throughout: it is run when someone is there to
		// unload it, which is what makes it the control in any shift analysis.
		if dow != 3 && dow != 6 {
			return 0
		}
		return chCycle(hour, min, 20, 60, []float64{2400, 2400})

	case "immersion":
		// An hour of hot water a day, 3 kW.
		start := 6
		if cheapestHour >= 0 {
			start = (cheapestHour + 2) % 24
		}
		return chCycle(hour, min, start, 60, []float64{3000, 3000})

	case "media":
		// Evening viewing, longer at weekends, plus standby.
		endHour := 23
		if dow == 0 || dow == 6 {
			endHour = 24
		}
		if hour >= 18 && hour < endHour {
			return 120
		}
		return 8

	case "office-ups":
		// Steady server load with a mild diurnal wobble — the power-only class.
		return 82 + 6*math.Sin(float64(hour)/24*2*math.Pi)

	case "unmonitored":
		// Everything with no plug: lighting, boiler pump, oven, kettle, standby.
		// Seasonal (darker days draw more) with morning and evening humps.
		base := 210 + 60*math.Cos(float64(doy)/365*2*math.Pi) // winter-heavy
		switch {
		case hour >= 7 && hour < 9:
			base += 900 // kettle, toaster, showers
		case hour >= 17 && hour < 20:
			base += 1400 // cooking
		case hour >= 0 && hour < 6:
			base -= 90
		}
		if dow == 0 || dow == 6 {
			base += 120
		}
		return base
	}
	return 0
}

// chCycle returns the wattage of an appliance cycle that starts at startHour and
// runs for durMin minutes, split evenly across the given phases.
func chCycle(hour, min, startHour, durMin int, phases []float64) float64 {
	elapsed := (hour-startHour)*60 + min
	if elapsed < 0 {
		elapsed += 24 * 60
	}
	if elapsed >= durMin {
		return 0
	}
	phase := elapsed * len(phases) / durMin
	if phase >= len(phases) {
		phase = len(phases) - 1
	}
	return phases[phase]
}

// --- the price curve --------------------------------------------------------

// chPrice returns the VAT-inclusive pence for the half hour starting at slot.
//
// Shaped like a real half-hourly import tariff — overnight trough, morning and
// evening peaks, occasional wind-driven plunge to negative — and deterministic in
// the slot's own timestamp so the same day always prices the same way.
func chPrice(slot time.Time, loc *time.Location) float64 {
	lt := slot.In(loc)
	halfHour := lt.Hour()*2 + lt.Minute()/30
	doy := lt.YearDay()

	// Diurnal shape: cheapest around 03:00, peak around 17:30.
	base := 19.0 +
		9.5*math.Cos(float64(halfHour-35)/48*2*math.Pi) +
		4.0*math.Cos(float64(halfHour-16)/48*4*math.Pi)

	// Seasonal: winter is dearer.
	base += 4.5 * math.Cos(float64(doy)/365*2*math.Pi)

	// Weekend demand is softer.
	if wd := lt.Weekday(); wd == time.Saturday || wd == time.Sunday {
		base -= 2.2
	}

	// Windy days: every 9th and 17th day of the year dips hard in the small
	// hours, going negative — a real feature of this tariff, and the case that
	// catches an abs() or a "costs are positive" assumption in a consumer.
	if doy%9 == 0 && halfHour >= 2 && halfHour <= 11 {
		base -= 17.0
	}
	if doy%17 == 0 && halfHour >= 24 && halfHour <= 30 {
		base -= 12.0 // a rarer daytime plunge
	}

	// Deterministic jitter so no two days are identical.
	j := math.Sin(float64(doy)*7.13+float64(halfHour)*1.77) * 2.4
	return math.Round((base+j)*1000) / 1000
}

// chCheapestHourFor returns the local hour whose two slots are cheapest on the
// day containing t — what a load-shifting automation would have picked, and so
// what the fixture's shifted appliances actually do.
func chCheapestHourFor(day time.Time, loc *time.Location) int {
	best, bestHour := math.Inf(1), 1
	midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	for h := 0; h < 24; h++ {
		a := chPrice(midnight.Add(time.Duration(h)*time.Hour), loc)
		b := chPrice(midnight.Add(time.Duration(h)*time.Hour+30*time.Minute), loc)
		if a+b < best {
			best, bestHour = a+b, h
		}
	}
	return bestHour
}

// chAgreements is the home's tariff history: fixed until the switch, then
// half-hourly — with the statutory zero rate of VAT (1 Oct 2026 – 31 Mar 2027)
// expressed as its own dated block under the SAME tariff code, which is the
// shape curveIdentity exists to see through.
func chAgreements(t *testing.T, loc *time.Location) config.EnergyAgreements {
	t.Helper()
	fixedFrom := chDay(t, loc, "2024-01-01")
	sw := chDay(t, loc, chSwitchDay)
	zeroFrom := chDay(t, loc, "2026-10-01")
	zeroTo := chDay(t, loc, "2027-04-01")

	return config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {
			{
				From: &fixedFrom, To: &sw,
				Name: "Flexible Octopus", Type: config.TariffTypeFixed,
				ID: "E-1R-VAR-22-11-01-C", Unit: "kWh",
				UnitRate: 0.2089, DailyStandingCharge: 0.5294, VATRate: 0.05,
			},
			{
				From: &sw, To: &zeroFrom,
				Name: "Agile Octopus", Type: config.TariffTypeVariable,
				ID: chTariffCode, Unit: "kWh",
				DailyStandingCharge: 0.591606, VATRate: 0.05,
			},
			{
				From: &zeroFrom, To: &zeroTo,
				Name: "Agile Octopus", Type: config.TariffTypeVariable,
				ID: chTariffCode, Unit: "kWh",
				DailyStandingCharge: 0.591606, VATRate: 0.0,
			},
			{
				From: &zeroTo,
				Name: "Agile Octopus", Type: config.TariffTypeVariable,
				ID: chTariffCode, Unit: "kWh",
				DailyStandingCharge: 0.591606, VATRate: 0.05,
			},
		},
	}}
}

// chConfig is the ConfigProvider for the harness.
type chConfig struct {
	devices    map[string]config.DeviceConfig
	agreements config.EnergyAgreements
}

func (c chConfig) Devices() map[string]config.DeviceConfig { return c.devices }
func (c chConfig) Tariffs() config.TariffSource            { return c.agreements }
func (c chConfig) Agreements() config.EnergyAgreements     { return c.agreements }
func (c chConfig) TariffNamespace() string                 { return "energy_tariffs" }

// chFloorplan is a two-storey floorplan covering the fixture's rooms.
type chFloorplan struct{}

func (chFloorplan) Floors() map[string]config.FloorConfig {
	return map[string]config.FloorConfig{
		"ground": {ID: "ground", Name: "Ground Floor", Order: chInt(0), Elevation: chFloat(0)},
		"first":  {ID: "first", Name: "First Floor", Order: chInt(1), Elevation: chFloat(3)},
	}
}

func (chFloorplan) Rooms() map[string]config.RoomConfig {
	return map[string]config.RoomConfig{
		"ground.kitchen": {ID: "ground.kitchen", Name: "Kitchen", Floor: "ground", Category: "kitchen", Area: chFloat(18)},
		"ground.utility": {ID: "ground.utility", Name: "Utility", Floor: "ground", Category: "plant", Area: chFloat(7)},
		"ground.lounge":  {ID: "ground.lounge", Name: "Lounge", Floor: "ground", Category: "living", Area: chFloat(26)},
		"first.office":   {ID: "first.office", Name: "Office", Floor: "first", Category: "work", Area: chFloat(11)},
		"ground.meter":   {ID: "ground.meter", Name: "Meter Cupboard", Floor: "ground", Category: "plant", Area: chFloat(1)},
	}
}

func chInt(v int) *int           { return &v }
func chFloat(v float64) *float64 { return &v }

// --- fixture assembly -------------------------------------------------------

// chFixture is everything the harness needs to serve: the wired Server plus the
// handles a Go-side test wants to assert against.
type chFixture struct {
	Server *Server
	Store  *prices.SQLiteStore
	From   time.Time // first telemetry sample
	To     time.Time // last telemetry sample
}

// chBuild assembles the whole fixture: telemetry into the two Influx sims, prices
// into a real SQLite archive, agreements and floorplan into config.
func chBuild(t *testing.T) *chFixture {
	t.Helper()
	loc := chLoc(t)

	from := chNow.In(loc).Truncate(24*time.Hour).AddDate(0, 0, -chTelemetryDays)
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	to := chNow.In(loc)

	counters := influx.NewCounterSim(loc)
	power := influx.NewPowerSim(loc)

	const cadence = 15 * time.Minute
	automation := chDay(t, loc, chAutomationDay)

	// Pre-resolve each day's cheapest hour once: chWatts is called ~92k times and
	// recomputing 48 prices inside it would dominate the build.
	cheapest := map[string]int{}
	cheapestOn := func(ts time.Time) int {
		if ts.Before(automation) {
			return -1 // no automation yet: fixed schedules
		}
		key := ts.In(loc).Format("2006-01-02")
		if h, ok := cheapest[key]; ok {
			return h
		}
		h := chCheapestHourFor(ts.In(loc), loc)
		cheapest[key] = h
		return h
	}

	// Walk the timeline once, accumulating every counter in step so the meter is
	// the sum of what the plugs did plus the unmonitored remainder — rather than
	// an independent series that happens to look close.
	var stamps []time.Time
	cum := map[string]float64{}
	series := map[string][]float64{}
	var meterCum float64
	var meter []float64
	upsAt := []time.Time{}
	upsW := []float64{}

	for ts := from; !ts.After(to); ts = ts.Add(cadence) {
		stamps = append(stamps, ts)
		ch := cheapestOn(ts)
		hours := cadence.Hours()

		var houseW float64
		for _, id := range chCounterDevices {
			w := chWatts(id, ts, loc, ch)
			cum[id] += w * hours / 1000
			series[id] = append(series[id], cum[id])
			houseW += w
		}
		upsW = append(upsW, chWatts("office-ups", ts, loc, ch))
		upsAt = append(upsAt, ts)
		houseW += chWatts("office-ups", ts, loc, ch)
		houseW += chWatts("unmonitored", ts, loc, ch)

		meterCum += houseW * hours / 1000
		meter = append(meter, meterCum)
	}

	for _, id := range chCounterDevices {
		counters.AddSamples(id, stamps, series[id])
		// The same devices also report instantaneous power, which is what the
		// avg_w column of every series comes from.
		w := make([]float64, len(stamps))
		for i, ts := range stamps {
			w[i] = chWatts(id, ts, loc, cheapestOn(ts))
		}
		power.AddSamples(id, stamps, w)
	}
	counters.AddSamples("electricity_meter", stamps, meter)
	power.AddSamples("office-ups", upsAt, upsW)

	// One Querier over both sims: each ignores the other's field, so answering
	// from both and concatenating is exactly "one Influx holding both series".
	q := &influx.FakeQuerier{PingOK: true}
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		a, err := counters.Answer(flux)
		if err != nil {
			return nil, err
		}
		b, err := power.Answer(flux)
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	}

	// The price archive: a real SQLite store, written through the real Put path,
	// from the tariff switch to a day and a half ahead of now (which is what the
	// supplier publishes).
	dir := t.TempDir()
	store, err := prices.Open(filepath.Join(dir, "prices.db"))
	if err != nil {
		t.Fatalf("open price store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	priceFrom := chDay(t, loc, chSwitchDay)
	priceTo := chNow.Add(36 * time.Hour)
	var slots []prices.Slot
	for ts := priceFrom; ts.Before(priceTo); ts = ts.Add(prices.SlotLength) {
		inc := chPrice(ts, loc)
		end := ts.Add(prices.SlotLength).UTC()
		slots = append(slots, prices.Slot{
			TariffCode:  chTariffCode,
			ValidFrom:   ts.UTC(),
			ValidTo:     &end,
			IncVATPence: inc,
			ExcVATPence: math.Round(inc/1.05*100000) / 100000,
			RetrievedAt: ts.UTC().Add(-24 * time.Hour),
		})
	}
	if _, err := store.Put(context.Background(), slots); err != nil {
		t.Fatalf("seed prices: %v", err)
	}

	// Archived standing charges, so /bill reports standing_charge_source=archive
	// rather than falling back to config.
	var charges []prices.DailyCharge
	for d := priceFrom; d.Before(priceTo); d = d.AddDate(0, 0, 1) {
		end := d.AddDate(0, 0, 1).UTC()
		charges = append(charges, prices.DailyCharge{
			TariffCode:  chTariffCode,
			ValidFrom:   d.UTC(),
			ValidTo:     &end,
			ExcVATPence: 56.343,
			IncVATPence: 59.16,
			RetrievedAt: d.UTC().Add(-24 * time.Hour),
		})
	}
	if _, err := store.PutStandingCharges(context.Background(), charges); err != nil {
		t.Fatalf("seed standing charges: %v", err)
	}

	s := New(":0", q, nil)
	s.Bucket = "statehouse"
	s.Clock = testutil.NewFakeClock(chNow)
	s.Loc = loc
	s.SiteID = "harness"
	s.DevicesNamespace = "devices_harness"
	s.FloorplanNamespace = "floorplan_harness"
	s.Version = "consumer-harness"
	s.Config = chConfig{devices: chDevices(), agreements: chAgreements(t, loc)}
	s.Floorplan = chFloorplan{}
	s.PriceReader = store

	return &chFixture{Server: s, Store: store, From: from, To: to}
}

// --- the harness itself -----------------------------------------------------

// TestConsumerHarness serves the fixture on a real port for the consumer scripts.
//
//	CH_HARNESS=1 CH_PORT=8787 CH_SECONDS=600 go test ./internal/httpapi -run TestConsumerHarness
//
// Skipped otherwise, so CI neither binds a port nor waits.
func TestConsumerHarness(t *testing.T) {
	if os.Getenv("CH_HARNESS") == "" {
		t.Skip("set CH_HARNESS=1 to serve the consumer fixture")
	}
	port := os.Getenv("CH_PORT")
	if port == "" {
		port = "8787"
	}
	seconds := 600
	if v := os.Getenv("CH_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("CH_SECONDS: %v", err)
		}
		seconds = n
	}

	fx := chBuild(t)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: fx.Server.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	fmt.Printf("HARNESS READY http://127.0.0.1:%s  now=%s  telemetry=%s..%s\n",
		port, chNow.Format(time.RFC3339),
		fx.From.Format("2006-01-02"), fx.To.Format("2006-01-02"))
	time.Sleep(time.Duration(seconds) * time.Second)
}

// TestConsumerFixtureIsPlausible is the guard on everything above. The consumer
// findings recorded in issue #36's thread are only worth as much as the fixture
// that produced them, so the properties they rest on are asserted here rather
// than eyeballed once: a bill that reconciles, a meter above monitored, a full
// priced curve, and a load shape that actually moved when the automation came on.
func TestConsumerFixtureIsPlausible(t *testing.T) {
	fx := chBuild(t)
	s := fx.Server

	// 1. A week's bill prices, reconciles, and leaves nothing unpriced.
	w := doGET(t, s, "/bill?window=7d")
	if w.Code != http.StatusOK {
		t.Fatalf("/bill: %d %s", w.Code, w.Body.String())
	}
	bill := decode(t, w)
	if bill["attribution"] != "counter_slot" {
		t.Errorf("attribution = %v, want counter_slot (the fixture is on a half-hourly tariff)", bill["attribution"])
	}
	if got := bill["unpriced_kwh"]; got != nil {
		t.Errorf("unpriced_kwh = %v, want absent: the archive covers the whole window", got)
	}
	if bill["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive", bill["standing_charge_source"])
	}
	rec := bill["reconciliation"].(map[string]any)
	meterKWh := rec["meter_kwh"].(float64)
	energyCost := bill["energy_cost"].(float64)
	if energyCost <= 0 {
		t.Errorf("energy_cost = %v, want positive", energyCost)
	}

	// 2. The meter sees materially more than the plugs, which is the whole reason
	//    the unmonitored decomposition exists. Coverage in a believable band.
	cov := rec["coverage"].(float64)
	if cov < 0.2 || cov > 0.75 {
		t.Errorf("coverage = %.3f, want a realistic partial-coverage home (0.2..0.75)", cov)
	}

	// 3. group_by=house sums: monitored + unmonitored == meter, and the meter
	//    total agrees with the bill's reconciliation.
	w = doGET(t, s, "/series?window=7d&group_by=house&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("/series house: %d %s", w.Code, w.Body.String())
	}
	hr := decodeSeries(t, w)
	if hr.StaleMonitoredCount == nil || *hr.StaleMonitoredCount != 0 {
		t.Errorf("stale_monitored_count = %v, want 0: every fixture device reports throughout", hr.StaleMonitoredCount)
	}
	tot := map[string]float64{}
	for _, ser := range hr.Series {
		tot[ser.Key] = ser.TotalKWh
	}
	if d := math.Abs(tot["monitored"] + tot["unmonitored"] - tot["meter"]); d > 0.5 {
		t.Errorf("monitored+unmonitored-meter = %.3f kWh, want ~0", d)
	}
	if d := math.Abs(tot["meter"] - meterKWh); d > 0.5 {
		t.Errorf("house meter %.3f vs bill reconciliation %.3f", tot["meter"], meterKWh)
	}

	// 4. A full, complete half-hourly curve for a past week.
	w = doGET(t, s, "/prices?window=custom&from=2026-09-07T00:00:00%2B01:00&to=2026-09-14T00:00:00%2B01:00")
	if w.Code != http.StatusOK {
		t.Fatalf("/prices: %d %s", w.Code, w.Body.String())
	}
	px := decode(t, w)
	if px["complete"] != true {
		t.Errorf("complete = %v, want true", px["complete"])
	}
	if n := len(px["slots"].([]any)); n != 7*48 {
		t.Errorf("slots = %d, want 336", n)
	}
	sum := px["summary"].(map[string]any)
	if sum["min"].(float64) >= sum["max"].(float64) {
		t.Errorf("summary min %v >= max %v: the curve is flat", sum["min"], sum["max"])
	}

	// 5. The curve goes negative somewhere in the archive, so a consumer that
	//    assumes costs are positive is genuinely caught out by this fixture.
	w = doGET(t, s, "/prices/stats?window=custom&from=2026-06-01T00:00:00%2B01:00&to=2026-09-01T00:00:00%2B01:00")
	if w.Code != http.StatusOK {
		t.Fatalf("/prices/stats: %d %s", w.Code, w.Body.String())
	}
	st := decode(t, w)
	var sawNegative bool
	for _, d := range st["days"].([]any) {
		if d.(map[string]any)["min"].(float64) < 0 {
			sawNegative = true
			break
		}
	}
	if !sawNegative {
		t.Error("no negative price anywhere in three months: the fixture cannot catch a sign assumption")
	}

	// 6. The automation is visible: the dishwasher's cheap-slot share of energy
	//    must rise after it came on, or "did load shift?" has no answer to find.
	before := chDishwasherNightShare(t, s, "2026-06-01", "2026-07-01")
	after := chDishwasherNightShare(t, s, "2026-08-01", "2026-09-01")
	if after <= before+0.3 {
		t.Errorf("dishwasher overnight share %.2f → %.2f: the load shift is not detectable", before, after)
	}
}

// chDishwasherNightShare is the fraction of the dishwasher's energy drawn
// between 00:00 and 06:00 local over [from, to).
func chDishwasherNightShare(t *testing.T, s *Server, from, to string) float64 {
	t.Helper()
	w := doGET(t, s, "/devices/dishwasher/series?window=custom&from="+from+
		"T00:00:00%2B01:00&to="+to+"T00:00:00%2B01:00&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("/devices/dishwasher/series: %d %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if len(r.Series) != 1 {
		t.Fatalf("want one series, got %d", len(r.Series))
	}
	var night, all float64
	for i, b := range r.Buckets {
		ts, err := time.Parse(time.RFC3339, b)
		if err != nil {
			t.Fatalf("bucket %q: %v", b, err)
		}
		kwh := r.Series[0].KWh[i]
		all += kwh
		if h := ts.Hour(); h < 6 {
			night += kwh
		}
	}
	if all == 0 {
		t.Fatalf("dishwasher drew nothing in %s..%s", from, to)
	}
	return night / all
}
