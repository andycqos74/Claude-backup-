package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"centralbackup/internal/proto"
)

// encodePowerShellCommand renders a script for PowerShell's -EncodedCommand
// (base64 of the UTF-16LE bytes). This is the reliable way to hand a
// multi-line script to powershell.exe from a service: no stdin, no argument
// quoting, and no dependence on -Command reading standard input.
func encodePowerShellCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[i*2:], r)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Scheduled-task management. The server can list a client's Windows
// scheduled tasks and perform a deliberately small set of operations on
// them: run on demand, enable, disable, and change the schedule of an
// existing task. It can never change what a task *does* — buildActionScript
// only ever emits Start/Enable/Disable or a Set-ScheduledTask that rewrites
// -Trigger, so the task's actions/program are untouchable from the server.
//
// All PowerShell is generated and validated here (pure, testable); the
// Windows-only file actually runs it. Every caller-supplied value is either
// strictly validated to be numeric/enumerated or passed through psQuote, so
// nothing is interpolated into a script unescaped.

const schedActionTimeout = 30 * time.Second

// schedInventoryScript emits one compact JSON object per task, newline
// separated. Per-object ConvertTo-Json avoids PowerShell's habit of
// unwrapping a single-element array, which a top-level array would hit.
const schedInventoryScript = `try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}
$ErrorActionPreference='Stop'
Get-ScheduledTask | ForEach-Object {
  $t = $_
  $info = $null
  try { $info = $t | Get-ScheduledTaskInfo } catch {}
  [pscustomobject]@{
    name        = [string]$t.TaskName
    path        = [string]$t.TaskPath
    state       = [string]$t.State
    enabled     = [bool]$t.Settings.Enabled
    description = [string]$t.Description
    author      = [string]$t.Author
    actions     = @($t.Actions | ForEach-Object { if ($_.Execute) { (([string]$_.Execute) + ' ' + ([string]$_.Arguments)).Trim() } else { [string]$_.CimClass.CimClassName } })
    triggers    = @($t.Triggers | ForEach-Object { $s = ([string]$_.CimClass.CimClassName) -replace '^MSFT_Task','' -replace 'Trigger$',''; if ($_.StartBoundary) { $s += ' @ ' + ([string]$_.StartBoundary) }; $s })
    last_run    = if ($info) { [string]$info.LastRunTime } else { '' }
    next_run    = if ($info) { [string]$info.NextRunTime } else { '' }
    last_result = if ($info) { [int64]$info.LastTaskResult } else { [int64]0 }
  } | ConvertTo-Json -Depth 4 -Compress
}`

// parseSchedInventory turns the newline-delimited JSON from
// schedInventoryScript into tasks, skipping blank lines.
func parseSchedInventory(stdout []byte) ([]proto.SchedTask, error) {
	var tasks []proto.SchedTask
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var t proto.SchedTask
		if err := json.Unmarshal(line, &t); err != nil {
			return nil, fmt.Errorf("parse task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, sc.Err()
}

var (
	timeOfDayRe = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
	// Accepts "2006-01-02T15:04" and "2006-01-02T15:04:05" (optional secs).
	onceAtRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T([01]\d|2[0-3]):([0-5]\d)(?::([0-5]\d))?`)
)

// psDayOfWeek maps accepted day inputs to the literal PowerShell day name.
var psDayOfWeek = map[string]string{
	"sun": "Sunday", "sunday": "Sunday",
	"mon": "Monday", "monday": "Monday",
	"tue": "Tuesday", "tues": "Tuesday", "tuesday": "Tuesday",
	"wed": "Wednesday", "weds": "Wednesday", "wednesday": "Wednesday",
	"thu": "Thursday", "thur": "Thursday", "thurs": "Thursday", "thursday": "Thursday",
	"fri": "Friday", "friday": "Friday",
	"sat": "Saturday", "saturday": "Saturday",
}

// psQuote renders s as a PowerShell single-quoted literal. Inside single
// quotes only the quote itself is special, escaped by doubling.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// buildActionScript validates a SchedAction and returns the PowerShell that
// performs it. Unknown actions and invalid schedules are rejected before
// anything runs.
func buildActionScript(a proto.SchedAction) (string, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return "", fmt.Errorf("task name is required")
	}
	path := a.Path
	if path == "" {
		path = `\`
	}
	// Reject newlines: harmless inside a PS single-quoted string, but there
	// is never a legitimate task name/path containing one, and refusing
	// keeps the generated script a tidy single statement.
	if strings.ContainsAny(name+path, "\r\n") {
		return "", fmt.Errorf("task name or path contains an invalid character")
	}
	ref := fmt.Sprintf("-TaskName %s -TaskPath %s", psQuote(name), psQuote(path))

	switch a.Action {
	case proto.SchedActionRun:
		return "$ErrorActionPreference='Stop'\nStart-ScheduledTask " + ref + " | Out-Null", nil
	case proto.SchedActionEnable:
		return "$ErrorActionPreference='Stop'\nEnable-ScheduledTask " + ref + " | Out-Null", nil
	case proto.SchedActionDisable:
		return "$ErrorActionPreference='Stop'\nDisable-ScheduledTask " + ref + " | Out-Null", nil
	case proto.SchedActionSetSchedule:
		if a.Schedule == nil {
			return "", fmt.Errorf("a schedule is required")
		}
		trig, err := buildTriggerScript(*a.Schedule)
		if err != nil {
			return "", err
		}
		// Only -Trigger is replaced; the task's actions and principal are
		// left exactly as they were. This is what "lock the actions" means.
		return "$ErrorActionPreference='Stop'\n" + trig +
			"\nSet-ScheduledTask " + ref + " -Trigger $t | Out-Null", nil
	default:
		return "", fmt.Errorf("unsupported action %q", a.Action)
	}
}

// buildTriggerScript validates spec and returns PowerShell that assigns the
// new trigger to $t. Times are emitted as numeric Get-Date arguments rather
// than parsed strings, so there is no locale dependence and no string
// interpolation of caller input.
func buildTriggerScript(spec proto.ScheduleSpec) (string, error) {
	switch spec.Kind {
	case proto.SchedKindOnStart:
		return "$t = New-ScheduledTaskTrigger -AtStartup", nil
	case proto.SchedKindOnLogon:
		return "$t = New-ScheduledTaskTrigger -AtLogOn", nil

	case proto.SchedKindOnce:
		m := onceAtRe.FindStringSubmatch(spec.At)
		if m == nil {
			return "", fmt.Errorf("once schedule needs a date/time like 2006-01-02T15:04")
		}
		sec := m[6]
		if sec == "" {
			sec = "0"
		}
		return fmt.Sprintf("$t = New-ScheduledTaskTrigger -Once -At (Get-Date -Year %s -Month %s -Day %s -Hour %s -Minute %s -Second %s)",
			m[1], m[2], m[3], m[4], m[5], sec), nil

	case proto.SchedKindMinutes, proto.SchedKindHourly:
		h, min, err := parseTimeOfDay(spec.At)
		if err != nil {
			return "", err
		}
		unit, max := "Minutes", 1440
		if spec.Kind == proto.SchedKindHourly {
			unit, max = "Hours", 24
		}
		if spec.Interval < 1 || spec.Interval > max {
			return "", fmt.Errorf("interval must be between 1 and %d", max)
		}
		// -RepetitionDuration of ~10 years stands in for "indefinitely",
		// which older builds will not accept as an empty value.
		return fmt.Sprintf("$t = New-ScheduledTaskTrigger -Once -At (Get-Date -Hour %d -Minute %d -Second 0) -RepetitionInterval (New-TimeSpan -%s %d) -RepetitionDuration (New-TimeSpan -Days 3650)",
			h, min, unit, spec.Interval), nil

	case proto.SchedKindDaily:
		h, min, err := parseTimeOfDay(spec.At)
		if err != nil {
			return "", err
		}
		if spec.Interval < 1 || spec.Interval > 365 {
			return "", fmt.Errorf("interval must be between 1 and 365 days")
		}
		return fmt.Sprintf("$t = New-ScheduledTaskTrigger -Daily -At (Get-Date -Hour %d -Minute %d -Second 0) -DaysInterval %d",
			h, min, spec.Interval), nil

	case proto.SchedKindWeekly:
		h, min, err := parseTimeOfDay(spec.At)
		if err != nil {
			return "", err
		}
		if spec.Interval < 1 || spec.Interval > 52 {
			return "", fmt.Errorf("interval must be between 1 and 52 weeks")
		}
		days, err := weekdayList(spec.DaysOfWeek)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("$t = New-ScheduledTaskTrigger -Weekly -At (Get-Date -Hour %d -Minute %d -Second 0) -DaysOfWeek %s -WeeksInterval %d",
			h, min, days, spec.Interval), nil

	default:
		return "", fmt.Errorf("unsupported schedule kind %q", spec.Kind)
	}
}

func parseTimeOfDay(s string) (hour, min int, err error) {
	m := timeOfDayRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("time must be HH:MM (24-hour)")
	}
	// Regex already bounds the values; atoi is safe.
	hour = atoi(m[1])
	min = atoi(m[2])
	return hour, min, nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// weekdayList turns accepted day names into a comma-separated list of
// PowerShell day literals (constants, never interpolated input).
func weekdayList(in []string) (string, error) {
	if len(in) == 0 {
		return "", fmt.Errorf("weekly schedule needs at least one day of the week")
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		name, ok := psDayOfWeek[strings.ToLower(strings.TrimSpace(d))]
		if !ok {
			return "", fmt.Errorf("unrecognised day of week %q", d)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return strings.Join(out, ","), nil
}

// ---- message handlers (wired from agent.handleMessage) ----

func (a *Agent) handleListSched(cmd proto.ListSched) {
	ctx, cancel := context.WithTimeout(context.Background(), schedActionTimeout)
	defer cancel()
	inv := schedRunInventory(ctx)
	inv.RequestID = cmd.RequestID
	if err := a.send(proto.MsgSchedInventory, inv); err != nil {
		log.Printf("sched inventory reply: %v", err)
	}
}

func (a *Agent) handleSchedAction(cmd proto.SchedAction) {
	ctx, cancel := context.WithTimeout(context.Background(), schedActionTimeout)
	defer cancel()

	res := proto.SchedResult{RequestID: cmd.RequestID}
	script, err := buildActionScript(cmd)
	if err == nil {
		err = schedRunScript(ctx, script)
	}
	if err != nil {
		res.Error = err.Error()
		log.Printf("sched action %q on %q%q failed: %v", cmd.Action, cmd.Path, cmd.Name, err)
	} else {
		res.OK = true
		log.Printf("sched action %q on %q%q ok", cmd.Action, cmd.Path, cmd.Name)
	}
	if err := a.send(proto.MsgSchedResult, res); err != nil {
		log.Printf("sched action reply: %v", err)
	}
}
