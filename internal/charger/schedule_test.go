package charger

import (
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

func TestPeriodAt(t *testing.T) {
	t0 := time.Date(2026, 3, 2, 22, 0, 0, 0, time.UTC) // a Monday, 22:00
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	seconds := func(n int) *int { return &n }
	stamp := func(d time.Duration) *v16.Time { return &v16.Time{Time: at(d)} }

	// 10 from the start, 20 after 100 seconds.
	periods := []v16.ChargingSchedulePeriod{{StartPeriod: 0, Limit: 10}, {StartPeriod: 100, Limit: 20}}
	const none = -1.0

	for name, tc := range map[string]struct {
		profile   InstalledProfile
		relStart  time.Time
		at        time.Duration
		wantLimit float64
	}{
		"absolute, before its start": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			at: -time.Second, wantLimit: none,
		},
		"absolute, first period": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			at: 99 * time.Second, wantLimit: 10,
		},
		"absolute, second period, which lasts": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			at: 48 * time.Hour, wantLimit: 20,
		},
		"absolute, past its duration": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), Duration: seconds(200), ChargingSchedulePeriod: periods}}},
			at: 200 * time.Second, wantLimit: none,
		},
		"absolute with no start counts from when it was installed": {
			profile: InstalledProfile{Installed: at(time.Hour), Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute,
				ChargingSchedule: v16.ChargingSchedule{ChargingSchedulePeriod: periods}}},
			at: time.Hour + 150*time.Second, wantLimit: 20,
		},
		"relative counts from the start of the transaction": {
			profile: InstalledProfile{Installed: at(0), Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindRelative,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			relStart: at(time.Hour), at: time.Hour + 50*time.Second, wantLimit: 10,
		},
		"relative with no transaction counts from when it was installed": {
			profile: InstalledProfile{Installed: at(0), Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindRelative,
				ChargingSchedule: v16.ChargingSchedule{ChargingSchedulePeriod: periods}}},
			at: 150 * time.Second, wantLimit: 20,
		},
		"daily, the next night": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindRecurring, RecurrencyKind: v16.RecurrencyDaily,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), Duration: seconds(8 * 3600), ChargingSchedulePeriod: periods}}},
			at: 24*time.Hour + 30*time.Second, wantLimit: 10,
		},
		"daily, the next afternoon": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindRecurring, RecurrencyKind: v16.RecurrencyDaily,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), Duration: seconds(8 * 3600), ChargingSchedulePeriod: periods}}},
			at: 24*time.Hour + 16*time.Hour, wantLimit: none,
		},
		"weekly, a week later": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindRecurring, RecurrencyKind: v16.RecurrencyWeekly,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), Duration: seconds(8 * 3600), ChargingSchedulePeriod: periods}}},
			at: 7*24*time.Hour + 2*time.Hour, wantLimit: 20,
		},
		"weekly, the next day": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindRecurring, RecurrencyKind: v16.RecurrencyWeekly,
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), Duration: seconds(8 * 3600), ChargingSchedulePeriod: periods}}},
			at: 24*time.Hour + 2*time.Hour, wantLimit: none,
		},
		"before validFrom": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute, ValidFrom: stamp(time.Hour), ValidTo: stamp(2 * time.Hour),
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			at: 30 * time.Minute, wantLimit: none,
		},
		"between validFrom and validTo": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute, ValidFrom: stamp(time.Hour), ValidTo: stamp(2 * time.Hour),
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			at: 90 * time.Minute, wantLimit: 20,
		},
		"at validTo": {
			profile: InstalledProfile{Profile: v16.ChargingProfile{ChargingProfileKind: v16.KindAbsolute, ValidFrom: stamp(time.Hour), ValidTo: stamp(2 * time.Hour),
				ChargingSchedule: v16.ChargingSchedule{StartSchedule: stamp(0), ChargingSchedulePeriod: periods}}},
			at: 2 * time.Hour, wantLimit: none,
		},
	} {
		period, ok := tc.profile.periodAt(at(tc.at), tc.relStart)
		got := none
		if ok {
			got = period.Limit
		}
		if got != tc.wantLimit {
			t.Errorf("%s: limit = %v, want %v (-1 stands for no limit)", name, got, tc.wantLimit)
		}
	}
}
