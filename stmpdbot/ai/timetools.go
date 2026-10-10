package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/milindmadhukar/STMPDBot/utils"
)

// timeTools lets the model turn "8pm CEST" or "2026-12-31 23:00 EST" into a
// Discord <t:epoch:style> tag, which every reader sees in their own zone. The
// model cannot do this arithmetic reliably, nor does it know what "now" is.
func timeTools() []Tool {
	return []Tool{
		{Type: "function", Function: ToolFunction{
			Name:        "get_current_time",
			Description: "Get the current date and time (UTC, and optionally in a given timezone) plus the current unix epoch. Call this whenever \"today\", \"tomorrow\", \"tonight\", a weekday or how long until something matters.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"timezone": {"type": "string", "description": "optional IANA zone (\"Europe/Amsterdam\") or abbreviation (\"CEST\", \"EST\", \"IST\")"}
				}
			}`),
		}},
		{Type: "function", Function: ToolFunction{
			Name:        "discord_timestamp",
			Description: "Convert a time someone mentioned into a Discord timestamp tag like <t:1760205600:F>, which shows in every reader's own timezone. Use this whenever someone gives a time with a timezone, or asks for a time to be converted/shared. Without a date it picks the NEXT time that clock time comes around (later today if still ahead, otherwise tomorrow). Paste the returned \"display\" into your reply exactly as given -- it shows the rendered time followed by the raw tag in backticks so people can copy it. Never compute an epoch yourself.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"time": {"type": "string", "description": "clock time, 24h \"20:00\" or 12h \"8pm\" / \"8:30 PM\". Omit for midnight when only a date was given."},
					"date": {"type": "string", "description": "optional date as YYYY-MM-DD. Omit to use the next occurrence of time."},
					"timezone": {"type": "string", "description": "IANA zone (\"Europe/Amsterdam\") or abbreviation (\"CEST\", \"PST\", \"IST\"). Defaults to UTC."},
					"style": {"type": "string", "enum": ["t", "T", "d", "D", "f", "F", "R"], "description": "Discord display style: t short time, T long time, d short date, D long date, f date+time, F weekday+date+time, R relative (\"in 3 hours\"). Defaults to F."}
				}
			}`),
		}},
	}
}

func dispatchTimeTool(name, argsJSON string) (string, error) {
	return dispatchTimeToolAt(time.Now(), name, argsJSON)
}

func dispatchTimeToolAt(now time.Time, name, argsJSON string) (string, error) {
	switch name {
	case "get_current_time":
		var args struct {
			Timezone string `json:"timezone"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("get_current_time: bad arguments: %w", err)
		}
		out := map[string]any{
			"utc":   now.UTC().Format("Monday 2006-01-02 15:04:05 MST"),
			"epoch": now.Unix(),
		}
		if args.Timezone != "" {
			loc, err := resolveZone(args.Timezone)
			if err != nil {
				return marshal(map[string]string{"error": err.Error()})
			}
			out["local"] = now.In(loc).Format("Monday 2006-01-02 15:04:05 MST (-07:00)")
		}
		return marshal(out)

	case "discord_timestamp":
		var args struct {
			Time     string `json:"time"`
			Date     string `json:"date"`
			Timezone string `json:"timezone"`
			Style    string `json:"style"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("discord_timestamp: bad arguments: %w", err)
		}
		t, err := resolveTimestamp(now, args.Time, args.Date, args.Timezone)
		if err != nil {
			// Returned to the model rather than failed, so it can ask the
			// user to clarify instead of the whole reply erroring out.
			return marshal(map[string]string{"error": err.Error()})
		}
		style := args.Style
		if !strings.Contains("tTdDfFR", style) || len(style) != 1 {
			style = "F"
		}
		tag := fmt.Sprintf("<t:%d:%s>", t.Unix(), style)
		return marshalRaw(map[string]any{
			"display":  fmt.Sprintf("%s (`%s`)", tag, tag),
			"tag":      tag,
			"relative": fmt.Sprintf("<t:%d:R>", t.Unix()),
			"epoch":    t.Unix(),
			"resolved": t.Format("Monday 2006-01-02 15:04 MST (-07:00)"),
		})
	}
	return "", fmt.Errorf("ai: unknown tool %q", name)
}

// marshalRaw is marshal without HTML escaping, so the tag reaches the model
// as a literal <t:...> it can copy rather than \u003ct:...\u003e.
func marshalRaw(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// resolveTimestamp builds the instant for clock/date in zone. With no date it
// is the next time that clock time occurs strictly after now.
func resolveTimestamp(now time.Time, clock, date, zone string) (time.Time, error) {
	loc := time.UTC
	if zone != "" {
		var err error
		if loc, err = resolveZone(zone); err != nil {
			return time.Time{}, err
		}
	}
	if clock == "" && date == "" {
		return time.Time{}, fmt.Errorf("give at least a time or a date")
	}

	hour, minute := 0, 0
	if clock != "" {
		var err error
		if hour, minute, err = parseClock(clock); err != nil {
			return time.Time{}, err
		}
	}

	if date != "" {
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("date %q is not YYYY-MM-DD", date)
		}
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, loc), nil
	}

	local := now.In(loc)
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = time.Date(local.Year(), local.Month(), local.Day()+1, hour, minute, 0, 0, loc)
	}
	return t, nil
}

func parseClock(s string) (int, int, error) {
	norm := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	norm = strings.ReplaceAll(norm, ".", ":")
	for _, layout := range []string{"15:04", "15", "3:04PM", "3PM", "15:04:05"} {
		if t, err := time.Parse(layout, norm); err == nil {
			return t.Hour(), t.Minute(), nil
		}
	}
	return 0, 0, fmt.Errorf("time %q not understood; use \"20:00\" or \"8pm\"", s)
}

// zoneAbbreviations maps the abbreviations people actually type to fixed
// offsets. They are fixed on purpose: "8pm CEST" means UTC+2 even in winter.
// Ambiguous ones (IST, CST) take the reading most common in this community.
var zoneAbbreviations = map[string]int{
	"UTC": 0, "GMT": 0, "Z": 0,
	"BST": 1, "WET": 0, "WEST": 1,
	"CET": 1, "CEST": 2, "MET": 1, "MEST": 2,
	"EET": 2, "EEST": 3, "MSK": 3, "TRT": 3,
	"GST": 4, "PKT": 5,
	"WIB": 7, "ICT": 7, "SGT": 8, "HKT": 8, "PHT": 8, "AWST": 8,
	"JST": 9, "KST": 9, "AEST": 10, "AEDT": 11, "NZST": 12, "NZDT": 13,
	"EST": -5, "EDT": -4, "CST": -6, "CDT": -5, "MST": -7, "MDT": -6,
	"PST": -8, "PDT": -7, "AKST": -9, "AKDT": -8, "HST": -10,
	"BRT": -3, "ART": -3, "AST": -4, "ADT": -3,
}

// halfHourZones covers the abbreviations whose offset is not whole hours.
var halfHourZones = map[string]int{"IST": 5*3600 + 1800, "NPT": 5*3600 + 2700, "ACST": 9*3600 + 1800, "ACDT": 10*3600 + 1800}

func resolveZone(name string) (*time.Location, error) {
	n := strings.TrimSpace(name)
	up := strings.ToUpper(n)
	if secs, ok := halfHourZones[up]; ok {
		return time.FixedZone(up, secs), nil
	}
	if h, ok := zoneAbbreviations[up]; ok {
		return time.FixedZone(up, h*3600), nil
	}
	// "UTC+2", "GMT-5:30"
	for _, prefix := range []string{"UTC", "GMT"} {
		if rest, ok := strings.CutPrefix(up, prefix); ok && rest != "" {
			sign, hh, mm := 1, 0, 0
			switch rest[0] {
			case '+':
			case '-':
				sign = -1
			default:
				return nil, fmt.Errorf("unknown timezone %q", name)
			}
			if _, err := fmt.Sscanf(strings.ReplaceAll(rest[1:], ":", " "), "%d %d", &hh, &mm); err != nil {
				if _, err := fmt.Sscanf(rest[1:], "%d", &hh); err != nil {
					return nil, fmt.Errorf("unknown timezone %q", name)
				}
			}
			return time.FixedZone(up, sign*(hh*3600+mm*60)), nil
		}
	}
	if loc, ok := utils.ValidateTimezone(n); ok {
		return loc, nil
	}
	return nil, fmt.Errorf("unknown timezone %q; use an IANA name like Europe/Amsterdam", name)
}
