package deepdive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/template"
)

// TraceEvent corresponds to a single line of data in JSONL
type TraceEvent struct {
	ProbeID  uint64 `json:"probe_id"`
	TID      uint64 `json:"tid"`
	Addr     string `json:"addr"`
	Seq      uint64 `json:"seq"`
	IsActive bool   `json:"is_active"`
	TS       uint64 `json:"ts"`
}

type CoroState struct {
	ProbeID       uint64
	FirstTS       uint64
	LastTS        uint64
	LastActive    bool
	LastAddr      string
	EventCount    int
	TIDMigrations int
	LastTID       uint64
}

type Report struct {
	TotalCoroutines int
	TotalEvents     int
	DurationMs      float64
	SigbusRisks     []*CoroState
	LostWakeups     []*CoroState
}

// RunDeepDive must have an uppercase first letter to be exposed for calling in main.go
func RunDeepDive(jsonlPath string, outMdPath string) error {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return err
	}
	defer f.Close()

	coroMap := make(map[uint64]*CoroState)
	var globalMinTS, globalMaxTS uint64 = ^uint64(0), 0
	totalEvents := 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	fmt.Println("🔍 [DeepDive] Scanning trace file...")

	for scanner.Scan() {
		totalEvents++
		var ev TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}

		if ev.TS < globalMinTS {
			globalMinTS = ev.TS
		}
		if ev.TS > globalMaxTS {
			globalMaxTS = ev.TS
		}

		state, exists := coroMap[ev.ProbeID]
		if !exists {
			state = &CoroState{
				ProbeID: ev.ProbeID, FirstTS: ev.TS, LastTID: ev.TID,
			}
			coroMap[ev.ProbeID] = state
		}

		if exists && ev.TID != state.LastTID {
			state.TIDMigrations++
			state.LastTID = ev.TID
		}

		state.LastTS = ev.TS
		state.LastActive = ev.IsActive
		state.LastAddr = ev.Addr
		state.EventCount++
	}

	fmt.Println("🧠 [DeepDive] Applying heuristic algorithms...")

	report := Report{
		TotalCoroutines: len(coroMap),
		TotalEvents:     totalEvents,
		DurationMs:      float64(globalMaxTS-globalMinTS) / 1e6,
	}

	for _, state := range coroMap {
		if state.LastAddr == "0x0000000000000000" || len(state.LastAddr) <= 4 {
			report.SigbusRisks = append(report.SigbusRisks, state)
		}
		const OneSecondNs = 1_000_000_000
		if !state.LastActive && (globalMaxTS-state.LastTS) > OneSecondNs {
			report.LostWakeups = append(report.LostWakeups, state)
		}
	}

	sort.Slice(report.LostWakeups, func(i, j int) bool {
		return report.LostWakeups[i].LastTS < report.LostWakeups[j].LastTS
	})

	return renderMarkdown(outMdPath, report)
}

const mdTemplate = `
# 🔬 coroTracer Deep Diagnostic Report (DeepDive)

## 📊 Overview
* **Total Traced Coroutines**: {{.TotalCoroutines}}
* **Total State Transitions**: {{.TotalEvents}}
* **Total Recording Duration**: {{printf "%.2f" .DurationMs}} ms

---

## 🚨 Critical Risk: Suspected SIGBUS / Memory Corruption
*Algorithm: Coroutine accessed 0x0 or invalid address.*

{{if .SigbusRisks}}
| Probe ID | Trigger Timestamp (TS) | Abnormal Address |
| :--- | :--- | :--- |
{{range .SigbusRisks}}| #{{.ProbeID}} | {{.LastTS}} | **{{.LastAddr}}** |
{{end}}
{{else}}
✅ No obvious address anomalies detected.
{{end}}

---

## 🧟‍♂️ Phantom Coroutines: Lost Wakeup / Suspected Deadlock (Lost Wakeup)
*Algorithm: Coroutine entered suspended state (is_active=false) and was never reawakened by the scheduler until program exit.*

{{if .LostWakeups}}
| Probe ID | Last Active Time (TS) | Last Thread Before Suspend (TID) | Instruction Address Before Suspend |
| :--- | :--- | :--- | :--- |
{{range .LostWakeups}}| #{{.ProbeID}} | {{.LastTS}} | {{.LastTID}} | {{.LastAddr}} |
{{end}}
{{else}}
✅ No lost wakeups detected. All coroutines closed perfectly!
{{end}}
`

func renderMarkdown(path string, data Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	tmpl, err := template.New("report").Parse(mdTemplate)
	if err != nil {
		return err
	}

	fmt.Printf("📝 [DeepDive] Report generated: %s\n", path)
	return tmpl.Execute(f, data)
}
