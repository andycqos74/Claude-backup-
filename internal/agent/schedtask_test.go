package agent

import (
	"strings"
	"testing"

	"centralbackup/internal/proto"
)

func TestBuildTriggerScript(t *testing.T) {
	cases := []struct {
		name string
		spec proto.ScheduleSpec
		want []string // substrings that must all appear
	}{
		{"daily", proto.ScheduleSpec{Kind: "daily", At: "09:30", Interval: 2},
			[]string{"New-ScheduledTaskTrigger -Daily", "Get-Date -Hour 9 -Minute 30 -Second 0", "-DaysInterval 2"}},
		{"weekly", proto.ScheduleSpec{Kind: "weekly", At: "23:05", Interval: 1, DaysOfWeek: []string{"mon", "Wednesday"}},
			[]string{"-Weekly", "-DaysOfWeek Monday,Wednesday", "-WeeksInterval 1", "Get-Date -Hour 23 -Minute 5"}},
		{"minutes", proto.ScheduleSpec{Kind: "minutes", At: "00:00", Interval: 15},
			[]string{"-Once", "RepetitionInterval (New-TimeSpan -Minutes 15)", "RepetitionDuration"}},
		{"hourly", proto.ScheduleSpec{Kind: "hourly", At: "06:00", Interval: 4},
			[]string{"RepetitionInterval (New-TimeSpan -Hours 4)"}},
		{"once", proto.ScheduleSpec{Kind: "once", At: "2026-01-02T15:04"},
			[]string{"-Once", "Get-Date -Year 2026 -Month 01 -Day 02 -Hour 15 -Minute 04 -Second 0"}},
		{"onstart", proto.ScheduleSpec{Kind: "onstart"}, []string{"-AtStartup"}},
		{"onlogon", proto.ScheduleSpec{Kind: "onlogon"}, []string{"-AtLogOn"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildTriggerScript(c.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, sub := range c.want {
				if !strings.Contains(got, sub) {
					t.Errorf("script missing %q\ngot: %s", sub, got)
				}
			}
		})
	}
}

func TestBuildTriggerScriptRejects(t *testing.T) {
	bad := []proto.ScheduleSpec{
		{Kind: "nope"},
		{Kind: "daily", At: "25:00", Interval: 1},
		{Kind: "daily", At: "09:00", Interval: 0},
		{Kind: "daily", At: "09:00", Interval: 999},
		{Kind: "minutes", At: "09:00", Interval: 5000},
		{Kind: "weekly", At: "09:00", Interval: 1},                             // no days
		{Kind: "weekly", At: "09:00", Interval: 1, DaysOfWeek: []string{"xx"}}, // bad day
		{Kind: "once", At: "not-a-date"},
	}
	for _, spec := range bad {
		if _, err := buildTriggerScript(spec); err == nil {
			t.Errorf("expected error for %+v", spec)
		}
	}
}

func TestBuildActionScript(t *testing.T) {
	for _, act := range []string{"run", "enable", "disable"} {
		s, err := buildActionScript(proto.SchedAction{Action: act, Name: "Backup", Path: `\MyTasks\`})
		if err != nil {
			t.Fatalf("%s: %v", act, err)
		}
		if !strings.Contains(s, `-TaskName 'Backup'`) || !strings.Contains(s, `-TaskPath '\MyTasks\'`) {
			t.Errorf("%s: missing quoted name/path: %s", act, s)
		}
	}
	cmdletFor := map[string]string{"run": "Start-ScheduledTask", "enable": "Enable-ScheduledTask", "disable": "Disable-ScheduledTask"}
	for act, cmdlet := range cmdletFor {
		s, _ := buildActionScript(proto.SchedAction{Action: act, Name: "T", Path: `\`})
		if !strings.Contains(s, cmdlet) {
			t.Errorf("%s should use %s: %s", act, cmdlet, s)
		}
	}
}

func TestBuildActionScriptSetScheduleOnlyTouchesTriggers(t *testing.T) {
	s, err := buildActionScript(proto.SchedAction{
		Action: "set_schedule", Name: "T", Path: `\`,
		Schedule: &proto.ScheduleSpec{Kind: "daily", At: "09:00", Interval: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "Set-ScheduledTask") || !strings.Contains(s, "-Trigger $t") {
		t.Errorf("set_schedule must apply -Trigger: %s", s)
	}
	// The action must never carry program/action definitions.
	for _, forbidden := range []string{"-Action", "New-ScheduledTaskAction", "-Execute", "Register-ScheduledTask", "Unregister-ScheduledTask"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("set_schedule unexpectedly contains %q: %s", forbidden, s)
		}
	}
}

func TestBuildActionScriptRejects(t *testing.T) {
	bad := []proto.SchedAction{
		{Action: "delete", Name: "T", Path: `\`},       // not allowed
		{Action: "create", Name: "T", Path: `\`},       // not allowed
		{Action: "run", Name: "", Path: `\`},           // no name
		{Action: "set_schedule", Name: "T", Path: `\`}, // no schedule
		{Action: "run", Name: "a\nb", Path: `\`},       // newline in name
	}
	for _, a := range bad {
		if _, err := buildActionScript(a); err == nil {
			t.Errorf("expected error for %+v", a)
		}
	}
}

// A task name crafted to break out of the single-quoted string must be
// neutralised by doubling the quote, never passed through raw.
func TestBuildActionScriptQuotesInjection(t *testing.T) {
	evil := `'; Remove-Item C:\ -Recurse; '`
	s, err := buildActionScript(proto.SchedAction{Action: "run", Name: evil, Path: `\`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "Remove-Item C:\\ -Recurse;") && !strings.Contains(s, "''") {
		t.Errorf("injection not escaped: %s", s)
	}
	if !strings.Contains(s, `'''; Remove-Item C:\ -Recurse; '''`) {
		t.Errorf("quotes not doubled as expected: %s", s)
	}
}

func TestParseSchedInventory(t *testing.T) {
	in := []byte("" +
		`{"name":"A","path":"\\","state":"Ready","enabled":true,"triggers":["Daily @ 2026-01-01T09:00:00"],"last_result":0}` + "\n" +
		"\n" + // blank line skipped
		`{"name":"B","path":"\\Microsoft\\","state":"Disabled","enabled":false,"last_result":267011}` + "\n")
	tasks, err := parseSchedInventory(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Name != "A" || !tasks[0].Enabled || len(tasks[0].Triggers) != 1 {
		t.Errorf("bad task[0]: %+v", tasks[0])
	}
	if tasks[1].Name != "B" || tasks[1].Enabled || tasks[1].LastResult != 267011 {
		t.Errorf("bad task[1]: %+v", tasks[1])
	}
}
