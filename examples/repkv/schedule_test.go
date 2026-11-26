package main

import (
	"strings"
	"testing"
)

func TestScheduleWindows(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		schedule   string
		initial    string
		experiment string
		scheduleLn string
		history    string
	}{
		{
			name: "before",
			body: strings.Join([]string{
				`{"kind":"cut","target":"node_0","at":"2026-11-26T00:00:00Z"}`,
				`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T00:00:01Z"}`,
				`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"error","at":"2026-11-26T00:00:02Z"}`,
				`{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","at":"2026-11-26T00:00:03Z"}`,
				`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","missing":true,"at":"2026-11-26T00:00:04Z"}`,
			}, "\n"),
			schedule:   "before",
			initial:    "",
			experiment: "experiment pass",
			scheduleLn: "schedule before occurred",
			history:    "history legal",
		},
		{
			name: "pending",
			body: strings.Join([]string{
				`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T00:00:00Z"}`,
				`{"kind":"cut","target":"node_0","at":"2026-11-26T00:00:01Z"}`,
				`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"timeout","at":"2026-11-26T00:00:02Z"}`,
				`{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","at":"2026-11-26T00:00:03Z"}`,
				`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"v","at":"2026-11-26T00:00:04Z"}`,
			}, "\n"),
			schedule:   "pending",
			initial:    "",
			experiment: "experiment pass",
			scheduleLn: "schedule pending occurred",
			history:    "history legal",
		},
		{
			name: "after",
			body: strings.Join([]string{
				`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T00:00:00Z"}`,
				`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"ok","at":"2026-11-26T00:00:01Z"}`,
				`{"kind":"cut","target":"node_0","at":"2026-11-26T00:00:02Z"}`,
				`{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","at":"2026-11-26T00:00:03Z"}`,
				`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"v","at":"2026-11-26T00:00:04Z"}`,
			}, "\n"),
			schedule:   "after",
			initial:    "",
			experiment: "experiment pass",
			scheduleLn: "schedule after occurred",
			history:    "history legal",
		},
		{
			name: "legal history without a post-cut read",
			body: strings.Join([]string{
				`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T00:00:00Z"}`,
				`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"ok","at":"2026-11-26T00:00:01Z"}`,
				`{"kind":"cut","target":"node_0","at":"2026-11-26T00:00:02Z"}`,
			}, "\n"),
			schedule:   "after",
			initial:    "",
			experiment: "experiment unknown reason=no_post_cut_read",
			scheduleLn: "schedule after occurred",
			history:    "history legal",
		},
		{
			name: "read happened in the wrong window",
			body: strings.Join([]string{
				`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T00:00:00Z"}`,
				`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"ok","at":"2026-11-26T00:00:01Z"}`,
				`{"kind":"cut","target":"node_0","at":"2026-11-26T00:00:02Z"}`,
				`{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","at":"2026-11-26T00:00:03Z"}`,
				`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"v","at":"2026-11-26T00:00:04Z"}`,
			}, "\n"),
			schedule:   "pending",
			initial:    "",
			experiment: "experiment unknown reason=schedule_miss",
			scheduleLn: "schedule pending missed",
			history:    "history legal",
		},
		{
			name: "one order is illegal",
			body: strings.Join([]string{
				`{"kind":"client","id":"w1","phase":"start","op":"write","key":"x","value":"1","at":"2026-11-26T00:00:00Z"}`,
				`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"ok","at":"2026-11-26T00:00:01Z"}`,
				`{"kind":"cut","target":"node_0","at":"2026-11-26T00:00:02Z"}`,
				`{"kind":"client","id":"r1","phase":"start","op":"read","key":"x","at":"2026-11-26T00:00:03Z"}`,
				`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"0","at":"2026-11-26T00:00:04Z"}`,
			}, "\n"),
			schedule:   "after",
			initial:    "0",
			experiment: "experiment illegal",
			scheduleLn: "schedule after occurred",
			history:    "history illegal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := scheduleCheck([]byte(tc.body+"\n"), tc.initial, tc.schedule)
			first, _, _ := strings.Cut(text, "\n")
			if first != tc.experiment {
				t.Fatalf("experiment %q\n%s", first, text)
			}
			if !strings.Contains(text, tc.scheduleLn+"\n") {
				t.Fatalf("missing %q\n%s", tc.scheduleLn, text)
			}
			if !strings.Contains(text, tc.history+"\n") {
				t.Fatalf("missing %q\n%s", tc.history, text)
			}
		})
	}
}
