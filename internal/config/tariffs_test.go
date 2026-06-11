package config

import (
	"math"
	"testing"
	"time"
)

func TestEnergyTariffs_Electricity(t *testing.T) {
	elec := Tariff{UnitRate: 0.2089, DailyStandingCharge: 0.5294, Unit: "kWh", VATRate: 0.05}

	tests := []struct {
		name    string
		tariffs EnergyTariffs
		want    Tariff
		wantOK  bool
	}{
		{
			name:    "present",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{"electricity": elec, "gas": {UnitRate: 9.9}}},
			want:    elec,
			wantOK:  true,
		},
		{
			name:    "absent",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{"gas": {UnitRate: 9.9}}},
			want:    Tariff{},
			wantOK:  false,
		},
		{
			name:    "nil map",
			tariffs: EnergyTariffs{},
			want:    Tariff{},
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.tariffs.Electricity()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("tariff = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEnergyTariffs_TariffFor_IgnoresDate(t *testing.T) {
	elec := Tariff{UnitRate: 0.2089, DailyStandingCharge: 0.5294, Unit: "kWh", VATRate: 0.05}
	et := EnergyTariffs{Tariffs: map[string]Tariff{"electricity": elec}}

	// v1: regardless of the time supplied, TariffFor returns the current tariff.
	times := []time.Time{
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		time.Time{}, // zero
	}
	for _, when := range times {
		got, ok := et.TariffFor(when)
		if !ok {
			t.Fatalf("TariffFor(%v) ok = false, want true", when)
		}
		if got != elec {
			t.Fatalf("TariffFor(%v) = %+v, want %+v", when, got, elec)
		}
	}

	// Absent electricity → not ok.
	if _, ok := (EnergyTariffs{}).TariffFor(time.Now()); ok {
		t.Fatalf("TariffFor on empty tariffs ok = true, want false")
	}
}

func TestTariff_Multiplier(t *testing.T) {
	tests := []struct {
		name    string
		vatRate float64
		want    float64
	}{
		{"5pct VAT", 0.05, 1.05},
		{"zero VAT", 0.0, 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Tariff{VATRate: tc.vatRate}.Multiplier()
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Multiplier() = %v, want %v", got, tc.want)
			}
		})
	}
}
