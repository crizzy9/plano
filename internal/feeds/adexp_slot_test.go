package feeds

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const adexpSlotTestScope = `
-ARCID TST101
-IFPLID AB12345678
-ADEP EGLL
-ADES LFPG
-EOBD 261106
-EOBT 2340`

func TestDecodeADEXPATFMSlot(t *testing.T) {
	now := time.Date(2026, time.November, 6, 23, 50, 0, 0, time.UTC)
	observed := now.Add(-2 * time.Minute)
	binding := ADEXPATFMSlotBinding{ProviderFlightID: "TST101-fixture", IFPLID: "AB12345678",
		ScheduledDeparture: now.Add(-15 * time.Minute), ObservedAt: observed, TakeoffDate: "2026-11-07"}
	for _, tc := range []struct {
		name, title, tail string
		context           ADEXPATFMSlotBinding
		status, reason    string
		takeoff           time.Time
		compact           bool
	}{
		{
			name: "SAM uses verified UTC day and first regulation cause", title: "SAM", context: binding,
			tail: "-CTOT 0012 -REGUL EXAMPLE1 -REGUL EXAMPLE2 -TTO -FL F300 -TO 0050 -PTID VEULE" +
				" -BEGIN ADDR -FAC LLEVZPZX -FAC LFFFZQZX -END ADDR -IOBD 261106 -IOBT 2320" +
				" -TAXITIME 0020 -REGCAUSE CE 81 -COMMENT PRIVATE DETAILS\n WRAPPED COMMENT",
			status: ATFMAssigned, reason: "Air traffic control capacity en route", takeoff: now.Add(22 * time.Minute),
		},
		{
			name: "SRM reads native timestamps and compact fields", title: "SRM",
			context: ADEXPATFMSlotBinding{ProviderFlightID: binding.ProviderFlightID, IFPLID: binding.IFPLID, ScheduledDeparture: binding.ScheduledDeparture},
			tail:    "- NEWCTOT 0015 -CTOD 261107 -TIMESTAMP 261106234800 -REGCAUSE WE 81",
			status:  ATFMAssigned, reason: "Weather en route", takeoff: now.Add(25 * time.Minute), compact: true,
		},
		{
			name: "SAM matching explicit date and native fields", title: "SAM", context: binding,
			tail:   "-CTOT 0012 -CTOD 261107 -TIMESTAMP 261106234800 -REGCAUSE ZA 81 -COMMENT UNKNOWN REASON",
			status: ATFMAssigned, takeoff: now.Add(22 * time.Minute),
		},
		{
			name: "SAM arrival capacity cause", title: "SAM", context: binding,
			tail:   "-CTOT 0012 -REGCAUSE GA 83 -TTO -TO 0050 -ADID LFPG",
			status: ATFMAssigned, reason: "Airport capacity during arrival", takeoff: now.Add(22 * time.Minute),
		},
		{
			name: "SAM omits unsupported airport capacity phase", title: "SAM", context: binding,
			tail: "-CTOT 0012 -REGCAUSE GE 83", status: ATFMAssigned, takeoff: now.Add(22 * time.Minute),
		},
		{
			name: "SLC withdraws without publishing operational comments", title: "SLC",
			context: ADEXPATFMSlotBinding{ProviderFlightID: binding.ProviderFlightID, IFPLID: binding.IFPLID,
				ScheduledDeparture: binding.ScheduledDeparture, ObservedAt: binding.ObservedAt},
			tail: "-REASON VOID -COMMENT FLIGHT CANCELLED -TAXITIME 0020", status: ATFMWithdrawn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := "-TITLE " + tc.title + adexpSlotTestScope + "\n" + tc.tail
			if tc.compact {
				message = strings.Join(strings.Fields(message), " ")
			}
			got, err := DecodeADEXPATFMSlot(strings.NewReader(message), tc.context, now)
			if err != nil || got.Version != 1 || got.Source != Eurocontrol || got.ProviderFlightID != binding.ProviderFlightID ||
				got.OriginICAO != "EGLL" || got.DestinationICAO != "LFPG" || !got.ScheduledDeparture.Equal(binding.ScheduledDeparture) ||
				!got.ObservedAt.Equal(observed) || got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("slot = %+v, error = %v", got, err)
			}
			if tc.takeoff.IsZero() && got.TakeoffAt != nil || !tc.takeoff.IsZero() && (got.TakeoffAt == nil || !got.TakeoffAt.Equal(tc.takeoff)) {
				t.Fatalf("takeoff = %v, want %v", got.TakeoffAt, tc.takeoff)
			}
		})
	}
}

func TestDecodeADEXPATFMSlotRejectsAmbiguity(t *testing.T) {
	now := time.Date(2026, time.November, 6, 23, 50, 0, 0, time.UTC)
	binding := ADEXPATFMSlotBinding{ProviderFlightID: "TST101-fixture", IFPLID: "AB12345678",
		ScheduledDeparture: now.Add(-15 * time.Minute), ObservedAt: now.Add(-2 * time.Minute), TakeoffDate: "2026-11-07"}
	valid := "-TITLE SAM" + adexpSlotTestScope + "\n-CTOT 0012"
	for _, tc := range []struct {
		name, message string
		edit          func(*ADEXPATFMSlotBinding)
	}{
		{name: "unverified IFPLID", message: valid, edit: func(b *ADEXPATFMSlotBinding) { b.IFPLID = "AB12345679" }},
		{name: "missing identity", message: strings.Replace(valid, "-IFPLID AB12345678", "", 1)},
		{name: "duplicate identity", message: valid + " -IFPLID AB12345678"},
		{name: "invalid callsign", message: strings.Replace(valid, "TST101", "TST 101", 1)},
		{name: "invalid route", message: strings.Replace(valid, "EGLL", "LHR", 1)},
		{name: "unknown aerodrome placeholder", message: strings.Replace(valid, "EGLL", "ZZZZ", 1)},
		{name: "air-filed flight plan", message: strings.Replace(valid, "EGLL", "AFIL", 1)},
		{name: "invalid off-block date", message: strings.Replace(valid, "261106", "261132", 1)},
		{name: "invalid off-block clock", message: strings.Replace(valid, "2340", "2400", 1)},
		{name: "invalid initial off-block date", message: valid + " -IOBD 261132"},
		{name: "invalid initial off-block clock", message: valid + " -IOBT 2400"},
		{name: "invalid takeoff clock", message: strings.Replace(valid, "0012", "2460", 1)},
		{name: "missing takeoff day", message: valid, edit: func(b *ADEXPATFMSlotBinding) { b.TakeoffDate = "" }},
		{name: "conflicting takeoff day", message: valid + " -CTOD 261106"},
		{name: "two takeoff clocks", message: valid + " -NEWCTOT 0015"},
		{name: "revision with assignment clock", message: strings.Replace(valid, "SAM", "SRM", 1)},
		{name: "withdrawal with takeoff", message: strings.Replace(valid, "SAM", "SLC", 1), edit: func(b *ADEXPATFMSlotBinding) { b.TakeoffDate = "" }},
		{name: "withdrawal with supplied date", message: "-TITLE SLC" + adexpSlotTestScope},
		{name: "proposal", message: "-TITLE SIP" + adexpSlotTestScope + " -NEWCTOT 0015"},
		{name: "unknown field", message: valid + " -UNSUPPORTED ANY"},
		{name: "incomplete TTO", message: valid + " -TTO -PTID VEULE"},
		{name: "two TTO points", message: valid + " -TTO -PTID VEULE -ADID LFPG -TO 2300"},
		{name: "nested message", message: "-BEGIN ADDR " + valid + " -END ADDR"},
		{name: "nested identity", message: strings.Replace(valid, "-IFPLID AB12345678", "-TTO -IFPLID AB12345678 -TO 2300", 1)},
		{name: "address list with nested identity", message: strings.Replace(valid, "-IFPLID AB12345678", "-BEGIN ADDR -FAC LLEVZPZX -IFPLID AB12345678 -END ADDR", 1)},
		{name: "missing address list end", message: valid + " -BEGIN ADDR -FAC LLEVZPZX"},
		{name: "invalid facility", message: valid + " -BEGIN ADDR -FAC " + strings.Repeat("A", 31) + " -END ADDR"},
		{name: "concatenated messages", message: valid + "\n" + valid},
		{name: "no original observation time", message: valid, edit: func(b *ADEXPATFMSlotBinding) { b.ObservedAt = time.Time{} }},
		{name: "conflicting native observation", message: valid + " -TIMESTAMP 261106234700"},
		{name: "malformed native observation", message: valid + " -TIMESTAMP 261106234800.1"},
		{name: "future original observation", message: valid, edit: func(b *ADEXPATFMSlotBinding) { b.ObservedAt = now.Add(time.Minute) }},
		{name: "stale original observation", message: valid, edit: func(b *ADEXPATFMSlotBinding) { b.ObservedAt = now.Add(-31 * time.Minute) }},
		{name: "duplicate regulation cause", message: valid + " -REGCAUSE CE 81 -REGCAUSE WE 81"},
		{name: "invalid transport byte", message: valid + "\x00"},
		{name: "oversized", message: valid + strings.Repeat(" ", MaxADEXPATFMSlotBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := binding
			if tc.edit != nil {
				tc.edit(&input)
			}
			if _, err := DecodeADEXPATFMSlot(strings.NewReader(tc.message), input, now); !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("error = %v, want invalid input", err)
			}
		})
	}
	if _, err := DecodeADEXPATFMSlot(iotest.ErrReader(errors.New("fixture read failure")), binding, now); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("read failure = %v, want unavailable", err)
	}
}
