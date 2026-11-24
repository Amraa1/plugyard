package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/buildinfo"
	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// inspection is everything "plugyard inspect" shows about a charger.
type inspection struct {
	Site          string `json:"site"`
	Name          string `json:"name"`
	ChargePointID string `json:"chargePointId"`
	// Connection is "stopped" for a charger that is powered off.
	Connection string `json:"connection"`
	CSMSURL    string `json:"csmsUrl"`

	Model      inspectedModel       `json:"model"`
	Connectors []inspectedConnector `json:"connectors"`
	// Fault is a fault of the charger as a whole.
	Fault            *charger.Fault             `json:"fault,omitempty"`
	ChargingProfiles []charger.InstalledProfile `json:"chargingProfiles"`
	Configuration    []charger.ConfigKey        `json:"configuration"`
	// QueuedMessages lists the actions of the messages still owed to the
	// CSMS, in order.
	QueuedMessages []string `json:"queuedMessages"`
	// AuthorizationCache lists the tags the charger remembers, when the
	// cache is on.
	AuthorizationCache map[string]v16.IDTagInfo `json:"authorizationCache,omitempty"`
	MessagesSent       int64                    `json:"messagesSent"`
	MessagesReceived   int64                    `json:"messagesReceived"`
}

type inspectedModel struct {
	Vendor   string  `json:"vendor"`
	Model    string  `json:"model"`
	Firmware string  `json:"firmware"`
	Voltage  float64 `json:"voltage"`
	Phases   int     `json:"phases"`
	OnEvFull string  `json:"onEvFull"`
	StartOn  string  `json:"startOn"`
}

type inspectedConnector struct {
	ID        int     `json:"id"`
	Type      string  `json:"type"`
	MaxPowerW float64 `json:"maxPowerW"`
	Status    string  `json:"status"`
	ErrorCode string  `json:"errorCode,omitempty"`
	PowerW    float64 `json:"powerW"`
	MeterWh   float64 `json:"meterWh"`
	// Unavailable says the CSMS made the connector unavailable.
	Unavailable bool                  `json:"unavailable,omitempty"`
	Fault       *charger.Fault        `json:"fault,omitempty"`
	EV          *inspectedEV          `json:"ev,omitempty"`
	Transaction *inspectedTransaction `json:"transaction,omitempty"`
}

type inspectedEV struct {
	// SoC and TargetSoC run from 0 to 1.
	SoC       float64 `json:"soc"`
	TargetSoC float64 `json:"targetSoc"`
	BatteryWh float64 `json:"batteryWh"`
	// MaxPowerW is zero for an EV with no limit of its own.
	MaxPowerW float64 `json:"maxPowerW"`
}

type inspectedTransaction struct {
	// ID is zero until the CSMS has confirmed the transaction.
	ID          int    `json:"id"`
	IDTag       string `json:"idTag"`
	ParentIDTag string `json:"parentIdTag,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
}

func newInspect() *cobra.Command {
	var (
		siteName string
		format   string
	)

	cmd := &cobra.Command{
		Use:   "inspect <charger>",
		Short: "Show everything about one charger",
		Long: "Show the full state of one charger, named by its charge point ID or its\n" +
			"name: its model, connectors, EVs and transactions, the charging profiles\n" +
			"the CSMS installed, every configuration key, and the messages it still\n" +
			"owes the CSMS. It works for a stopped charger too, from its saved state.\n" +
			"A password is never shown.",
		Example: "  plugyard inspect CP-1\n" +
			"  plugyard inspect brisk_volt --format json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			if isPattern(args[0]) {
				return &UsageError{Err: fmt.Errorf("inspect shows one charger, so %q cannot be a pattern", args[0])}
			}
			w, err := loadWorld(cmd.Context(), siteName)
			if err != nil {
				return err
			}
			defer w.close()
			targets, err := w.match(args[0], nil)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return fmt.Errorf("no charger matches %q", args[0])
			}
			t := targets[0]

			var details control.Inspection
			if t.client != nil {
				if details, err = t.client.Inspect(cmd.Context(), t.state.ChargePointID); err != nil {
					return fmt.Errorf("site %q did not answer: %w", t.state.Site, err)
				}
			} else {
				for _, s := range w.saved {
					for _, c := range s.Chargers {
						if s.Meta.Name == t.state.Site && c.ID == t.state.ChargePointID {
							details = control.Inspection{Model: c.Spec.Model, CSMSURL: c.Spec.CSMSURL, State: c.State}
						}
					}
				}
			}
			if details.Model == nil {
				return fmt.Errorf("charger %s has no saved state to inspect", t.state.ChargePointID)
			}

			view := inspect(t.state, details)
			out := cmd.OutOrStdout()
			if format == formatJSON {
				return json.NewEncoder(out).Encode(view)
			}
			writeInspection(out, view)
			return nil
		},
	}

	cmd.Flags().StringVarP(&siteName, "site", "s", "", "only look at chargers of this site")
	addFormatFlag(cmd, &format)
	return cmd
}

// inspect puts together what is known about a charger: what it shows right
// now, and the state behind that.
func inspect(now control.ChargerState, details control.Inspection) inspection {
	m := details.Model
	voltage, phases := m.Supply()
	view := inspection{
		Site:          now.Site,
		Name:          now.Name,
		ChargePointID: now.ChargePointID,
		Connection:    now.Connection,
		CSMSURL:       details.CSMSURL,
		Model: inspectedModel{
			Vendor: m.Vendor, Model: m.Name, Firmware: m.Firmware,
			Voltage: voltage, Phases: phases, OnEvFull: m.OnEvFull, StartOn: m.StartOn,
		},
		ChargingProfiles: []charger.InstalledProfile{},
		Configuration:    charger.Configuration(m, details.State),
		QueuedMessages:   []string{},
		MessagesSent:     now.MessagesSent,
		MessagesReceived: now.MessagesReceived,
	}

	saved := map[int]charger.ConnectorState{}
	if st := details.State; st != nil {
		view.Fault = st.Fault
		view.ChargingProfiles = append(view.ChargingProfiles, st.Profiles...)
		view.AuthorizationCache = st.AuthCache
		for _, q := range st.Outbox {
			view.QueuedMessages = append(view.QueuedMessages, q.Action)
		}
		for _, k := range st.Connectors {
			saved[k.ID] = k
		}
	}
	shown := map[int]charger.ConnectorSnapshot{}
	for _, k := range now.Connectors {
		shown[k.ID] = k
	}

	for _, mc := range m.Connectors {
		k := inspectedConnector{ID: mc.ID, Type: mc.Type, MaxPowerW: float64(mc.MaxPower), Status: v16.StatusAvailable}
		if snap, ok := shown[mc.ID]; ok {
			k.Status, k.ErrorCode, k.PowerW, k.MeterWh = snap.Status, snap.ErrorCode, snap.PowerW, snap.MeterWh
		}
		if ks, ok := saved[mc.ID]; ok {
			k.Unavailable, k.Fault = ks.Inoperative, ks.Fault
			if ks.EV != nil {
				k.EV = &inspectedEV{
					SoC:       ks.EV.SoC,
					TargetSoC: ks.EV.Params.TargetSoC,
					BatteryWh: ks.EV.Params.BatteryWh,
					MaxPowerW: ks.EV.Params.MaxPowerW,
				}
				if snap, ok := shown[mc.ID]; ok && snap.SoC != nil {
					k.EV.SoC = *snap.SoC
				}
			}
			// A transaction in the saved state of a stopped charger was cut
			// short; it ends when the charger starts.
			if ks.Tx != nil && now.Powered {
				k.Transaction = &inspectedTransaction{ID: ks.Tx.ID, IDTag: ks.Tx.IDTag, ParentIDTag: ks.Tx.ParentIDTag}
				if !ks.Tx.StartedAt.IsZero() {
					k.Transaction.StartedAt = ks.Tx.StartedAt.Format(time.RFC3339)
				}
			}
		}
		view.Connectors = append(view.Connectors, k)
	}
	return view
}

func writeInspection(w io.Writer, v inspection) {
	p := newPalette(w)
	section := func(title string) { fmt.Fprintf(w, "\n%s\n", p.bold(title)) }

	fmt.Fprintf(w, "%s\n", p.bold(v.ChargePointID))
	head := [][]cell{
		{plain("  Name"), plain(v.Name)},
		{plain("  Site"), plain(v.Site)},
		{plain("  Connection"), {plain: v.Connection, rendered: p.connection(v.Connection)}},
		{plain("  CSMS"), plain(v.CSMSURL)},
		{plain("  Model"), plain(fmt.Sprintf("%s %s, firmware %s", v.Model.Vendor, v.Model.Model, v.Model.Firmware))},
		{plain("  Supply"), plain(fmt.Sprintf("%s V, %s", strconv.FormatFloat(v.Model.Voltage, 'f', -1, 64), countPhases(v.Model.Phases)))},
		{plain("  Behavior"), plain(fmt.Sprintf("starts on %s; when the EV is full: %s", v.Model.StartOn, v.Model.OnEvFull))},
		{plain("  Messages"), plain(fmt.Sprintf("%d sent, %d received since power-on", v.MessagesSent, v.MessagesReceived))},
	}
	if v.Fault != nil {
		head = append(head, []cell{plain("  Fault"), {plain: describeFault(v.Fault), rendered: p.red(describeFault(v.Fault))}})
	}
	writeTable(w, head)

	section("Connectors")
	for _, k := range v.Connectors {
		status := k.Status
		if k.ErrorCode != "" {
			status += "(" + k.ErrorCode + ")"
		}
		fmt.Fprintf(w, "  %d  %s, %s  %s  %s now, meter %d Wh\n", k.ID, k.Type, model.Power(k.MaxPowerW), p.status(status), formatKilo(k.PowerW, "kW"), int(k.MeterWh))
		if k.Fault != nil {
			fmt.Fprintf(w, "       fault: %s\n", describeFault(k.Fault))
		}
		if k.Unavailable {
			fmt.Fprintf(w, "       made unavailable by the CSMS\n")
		}
		if ev := k.EV; ev != nil {
			limit := "no power limit of its own"
			if ev.MaxPowerW > 0 {
				limit = "accepts up to " + model.Power(ev.MaxPowerW).String()
			}
			fmt.Fprintf(w, "       EV: %.0f%% of %s kWh, charging to %.0f%%, %s\n", ev.SoC*100,
				strconv.FormatFloat(ev.BatteryWh/1000, 'f', -1, 64), ev.TargetSoC*100, limit)
		}
		if tx := k.Transaction; tx != nil {
			id := "not yet confirmed by the CSMS"
			if tx.ID != 0 {
				id = strconv.Itoa(tx.ID)
			}
			line := fmt.Sprintf("       transaction: %s, tag %s", id, tx.IDTag)
			if tx.StartedAt != "" {
				line += ", started " + tx.StartedAt
			}
			fmt.Fprintln(w, line)
		}
	}

	section("Charging profiles")
	if len(v.ChargingProfiles) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, ip := range v.ChargingProfiles {
		prof := ip.Profile
		where := "connector " + strconv.Itoa(ip.ConnectorID)
		if ip.ConnectorID == 0 {
			where = "whole charger"
		}
		kind := prof.ChargingProfileKind
		if prof.RecurrencyKind != "" {
			kind += " " + prof.RecurrencyKind
		}
		var periods []string
		for _, period := range prof.ChargingSchedule.ChargingSchedulePeriod {
			periods = append(periods, fmt.Sprintf("%s %s from %ds",
				strconv.FormatFloat(period.Limit, 'f', -1, 64), prof.ChargingSchedule.ChargingRateUnit, period.StartPeriod))
		}
		fmt.Fprintf(w, "  %d  %s, %s, stack level %d, %s: %s\n", prof.ChargingProfileID, where,
			prof.ChargingProfilePurpose, prof.StackLevel, kind, strings.Join(periods, ", "))
	}

	section("Queued for the CSMS")
	if len(v.QueuedMessages) == 0 {
		fmt.Fprintln(w, "  nothing")
	} else {
		fmt.Fprintf(w, "  %s\n", strings.Join(v.QueuedMessages, ", "))
	}

	if len(v.AuthorizationCache) > 0 {
		section("Authorization cache")
		var rows [][]cell
		for _, tag := range sortedKeys(v.AuthorizationCache) {
			rows = append(rows, []cell{plain("  " + tag), plain(v.AuthorizationCache[tag].Status)})
		}
		writeTable(w, rows)
	}

	section("Configuration")
	var rows [][]cell
	for _, key := range v.Configuration {
		row := []cell{plain("  " + key.Key), plain(key.Value)}
		if key.Readonly {
			row = append(row, plain("read-only"))
		}
		rows = append(rows, row)
	}
	writeTable(w, rows)
}

func describeFault(f *charger.Fault) string {
	text := f.Code
	if f.Info != "" {
		text += ", " + f.Info
	}
	if f.VendorCode != "" {
		text += " (vendor code " + f.VendorCode + ")"
	}
	return text
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func countPhases(n int) string {
	if n == 1 {
		return "1 phase"
	}
	return fmt.Sprintf("%d phases", n)
}

// infoView is what "plugyard info" shows.
type infoView struct {
	Version       string     `json:"version"`
	OCPP          []string   `json:"ocpp"`
	DataDirectory string     `json:"dataDirectory"`
	Sites         []infoSite `json:"sites"`
}

type infoSite struct {
	siteRow
	// Powered is the number of chargers that are powered on.
	Powered int `json:"powered"`
}

func newInfo() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show the sites on this machine and where plugyard keeps its data",
		Long: "Show every site on this machine, running or not, with its chargers and\n" +
			"the CSMS it connects to, and the directory that holds saved state and logs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			w, err := loadWorld(cmd.Context(), "")
			if err != nil {
				return err
			}
			defer w.close()

			view := infoView{Version: buildinfo.Version(), OCPP: buildinfo.OCPPVersions, DataDirectory: w.dir, Sites: []infoSite{}}
			powered := map[string]int{}
			for _, client := range w.clients {
				powered[client.Info.Site] = client.Info.Powered
			}
			counts := map[string]int{}
			for _, row := range siteRows(w, true) {
				view.Sites = append(view.Sites, infoSite{siteRow: row, Powered: powered[row.Site]})
				counts[row.Status]++
			}

			out := cmd.OutOrStdout()
			if format == formatJSON {
				return json.NewEncoder(out).Encode(view)
			}
			fmt.Fprintf(out, "Version:         %s\n", view.Version)
			fmt.Fprintf(out, "OCPP:            %s\n", strings.Join(view.OCPP, ", "))
			fmt.Fprintf(out, "Data directory:  %s\n", view.DataDirectory)
			fmt.Fprintf(out, "Sites:           %d running, %d stopped, %d down\n", counts[siteRunning], counts[siteStopped], counts[siteDown])
			if len(view.Sites) == 0 {
				return nil
			}
			fmt.Fprintln(out)
			p := newPalette(out)
			table := [][]cell{{plain("SITE"), plain("STATUS"), plain("CHARGERS"), plain("POWERED ON"), plain("PROCESS"), plain("CSMS")}}
			for _, s := range view.Sites {
				status := cell{plain: s.Status, rendered: s.Status}
				process := "-"
				if s.Status == siteRunning {
					status.rendered = p.green(s.Status)
					process = strconv.Itoa(s.PID)
				}
				table = append(table, []cell{
					plain(s.Site), status, plain(strconv.Itoa(s.Chargers)), plain(strconv.Itoa(s.Powered)),
					plain(process), plain(strings.Join(s.CSMS, ", ")),
				})
			}
			writeTable(out, table)
			return nil
		},
	}
	addFormatFlag(cmd, &format)
	return cmd
}
