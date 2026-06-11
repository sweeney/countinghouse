package energy

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// realTariff is the live electricity tariff from PLAN §7.
var realTariff = config.Tariff{
	UnitRate:            0.2089,
	DailyStandingCharge: 0.5294,
	Unit:                "kWh",
	VATRate:             0.05,
}

const eps = 1e-9

func TestDeviceCostFor(t *testing.T) {
	tests := []struct {
		name string
		kwh  float64
		want float64
	}{
		{"ten kwh", 10, 10 * 0.2089 * 1.05},
		{"zero kwh", 0, 0},
		{"fractional", 3.5, 3.5 * 0.2089 * 1.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeviceCostFor(tc.kwh, realTariff)
			if math.Abs(got-tc.want) > eps {
				t.Fatalf("DeviceCostFor(%v) = %v, want %v", tc.kwh, got, tc.want)
			}
			// Sanity: VAT really is applied (gross > ex-VAT) for positive kWh.
			if tc.kwh > 0 {
				exVAT := tc.kwh * realTariff.UnitRate
				if got <= exVAT {
					t.Fatalf("DeviceCostFor(%v) = %v not greater than ex-VAT %v", tc.kwh, got, exVAT)
				}
			}
		})
	}
}

func TestStandingChargeFor(t *testing.T) {
	tests := []struct {
		name string
		days float64
		want float64
	}{
		{"one day", 1, 1 * 0.5294 * 1.05},
		{"integer days", 30, 30 * 0.5294 * 1.05},
		{"fractional day", 0.5, 0.5 * 0.5294 * 1.05},
		{"partial period-to-date", 11.5, 11.5 * 0.5294 * 1.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StandingChargeFor(tc.days, realTariff)
			if math.Abs(got-tc.want) > eps {
				t.Fatalf("StandingChargeFor(%v) = %v, want %v", tc.days, got, tc.want)
			}
		})
	}
}

// dayWindow builds a window whose Days() is exactly d days.
func dayWindow(label string, d float64) Window {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stop := start.Add(time.Duration(d * float64(24*time.Hour)))
	return Window{Start: start, Stop: stop, Label: label}
}

func TestAssembleBill(t *testing.T) {
	mul := realTariff.Multiplier()

	tests := []struct {
		name     string
		window   Window
		devices  []DeviceCost
		meterKWh float64

		wantEnergy      float64
		wantStanding    float64
		wantTotal       float64
		wantMonitored   float64
		wantUnmonitored float64
		wantCoverage    float64
	}{
		{
			name:   "unmonitored remainder",
			window: dayWindow("month", 10),
			devices: []DeviceCost{
				{DeviceID: "a", KWh: 5},
				{DeviceID: "b", KWh: 3},
			},
			meterKWh:        20,
			wantEnergy:      (5 + 3) * 0.2089 * mul,
			wantStanding:    10 * 0.5294 * mul,
			wantMonitored:   8,
			wantUnmonitored: 12,
			wantCoverage:    8.0 / 20.0,
		},
		{
			name:   "coverage over 1 (export)",
			window: dayWindow("week", 7),
			devices: []DeviceCost{
				{DeviceID: "a", KWh: 10},
			},
			meterKWh:        4, // solar/battery export → meter < monitored
			wantEnergy:      10 * 0.2089 * mul,
			wantStanding:    7 * 0.5294 * mul,
			wantMonitored:   10,
			wantUnmonitored: 4 - 10,
			wantCoverage:    10.0 / 4.0,
		},
		{
			name:   "meter zero guard",
			window: dayWindow("today", 0.5),
			devices: []DeviceCost{
				{DeviceID: "a", KWh: 2},
			},
			meterKWh:        0,
			wantEnergy:      2 * 0.2089 * mul,
			wantStanding:    0.5 * 0.5294 * mul,
			wantMonitored:   2,
			wantUnmonitored: -2,
			wantCoverage:    0, // guarded, no NaN/Inf
		},
		{
			name:            "no devices",
			window:          dayWindow("month", 3),
			devices:         nil,
			meterKWh:        15,
			wantEnergy:      0,
			wantStanding:    3 * 0.5294 * mul,
			wantMonitored:   0,
			wantUnmonitored: 15,
			wantCoverage:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bill := AssembleBill(tc.window, tc.devices, tc.meterKWh, realTariff)

			if bill.Currency != "GBP" {
				t.Errorf("Currency = %q, want GBP", bill.Currency)
			}
			if bill.Window != tc.window.Label {
				t.Errorf("Window = %q, want %q", bill.Window, tc.window.Label)
			}
			if math.Abs(bill.EnergyCost-tc.wantEnergy) > eps {
				t.Errorf("EnergyCost = %v, want %v", bill.EnergyCost, tc.wantEnergy)
			}
			if math.Abs(bill.StandingCharge-tc.wantStanding) > eps {
				t.Errorf("StandingCharge = %v, want %v", bill.StandingCharge, tc.wantStanding)
			}
			wantTotal := tc.wantEnergy + tc.wantStanding
			if math.Abs(bill.Total-wantTotal) > eps {
				t.Errorf("Total = %v, want %v", bill.Total, wantTotal)
			}

			r := bill.Reconciliation
			if math.Abs(r.MonitoredKWh-tc.wantMonitored) > eps {
				t.Errorf("MonitoredKWh = %v, want %v", r.MonitoredKWh, tc.wantMonitored)
			}
			if math.Abs(r.MeterKWh-tc.meterKWh) > eps {
				t.Errorf("MeterKWh = %v, want %v", r.MeterKWh, tc.meterKWh)
			}
			if math.Abs(r.UnmonitoredKWh-tc.wantUnmonitored) > eps {
				t.Errorf("UnmonitoredKWh = %v, want %v", r.UnmonitoredKWh, tc.wantUnmonitored)
			}
			if math.IsNaN(r.Coverage) || math.IsInf(r.Coverage, 0) {
				t.Fatalf("Coverage is non-finite: %v", r.Coverage)
			}
			if math.Abs(r.Coverage-tc.wantCoverage) > eps {
				t.Errorf("Coverage = %v, want %v", r.Coverage, tc.wantCoverage)
			}

			// Per-device costs filled in.
			for _, d := range bill.Devices {
				want := DeviceCostFor(d.KWh, realTariff)
				if math.Abs(d.Cost-want) > eps {
					t.Errorf("device %s Cost = %v, want %v", d.DeviceID, d.Cost, want)
				}
			}
		})
	}
}
