package deepdive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/template"
)

// TraceEvent 对应 JSONL 中的单行数据
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

// RunDeepDive 必须首字母大写，暴露给 main.go 调用
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
# 🔬 coroTracer 深度诊断报告 (DeepDive)

## 📊 概览 (Overview)
* **总追踪协程数**: {{.TotalCoroutines}}
* **总状态切换数**: {{.TotalEvents}}
* **录制总时长**: {{printf "%.2f" .DurationMs}} ms

---

## 🚨 致命风险：疑似 SIGBUS / 内存损坏
*算法判定：协程操作了 0x0 或异常地址。*

{{if .SigbusRisks}}
| Probe ID | 触发时间戳 (TS) | 异常地址 |
| :--- | :--- | :--- |
{{range .SigbusRisks}}| #{{.ProbeID}} | {{.LastTS}} | **{{.LastAddr}}** |
{{end}}
{{else}}
✅ 未检测到明显的地址异常。
{{end}}

---

## 🧟‍♂️ 幽灵协程：丢失唤醒 / 疑似死锁 (Lost Wakeup)
*算法判定：协程陷入挂起状态 (is_active=false)，直到程序结束都未被调度器重新唤醒。*

{{if .LostWakeups}}
| Probe ID | 最后活跃时间 (TS) | 挂起前最后线程 (TID) | 挂起前指令地址 |
| :--- | :--- | :--- | :--- |
{{range .LostWakeups}}| #{{.ProbeID}} | {{.LastTS}} | {{.LastTID}} | {{.LastAddr}} |
{{end}}
{{else}}
✅ 未检测到丢失唤醒，所有协程均完美闭环！
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
