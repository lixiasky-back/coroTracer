package export

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type TraceEvent struct {
	ProbeID  uint64 `json:"probe_id"`
	TID      uint64 `json:"tid"`
	Addr     string `json:"addr"`
	Seq      uint64 `json:"seq"`
	IsActive bool   `json:"is_active"`
	TS       uint64 `json:"ts"`
}

func GenerateHTML(jsonlPath string, htmlPath string) error {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Println("🏗️  [Export] Deep Topology Reconstruction Started...")

	coroMap := make(map[uint64][]TraceEvent)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var ev TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil {
			coroMap[ev.ProbeID] = append(coroMap[ev.ProbeID], ev)
		}
	}

	var ids []uint64
	for id := range coroMap {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var sbNav strings.Builder
	var sbContent strings.Builder

	for _, id := range ids {
		events := coroMap[id]
		sort.Slice(events, func(i, j int) bool { return events[i].TS < events[j].TS })

		startTime := events[0].TS
		duration := float64(events[len(events)-1].TS-startTime) / 1000000.0
		threadMap := make(map[uint64]bool)
		for _, e := range events {
			threadMap[e.TID] = true
		}

		isCorrupt := id == 0 || duration > 1000000
		statusTag := ""
		if isCorrupt {
			statusTag = " <span style='color:#f85149;font-weight:bold'>[CORRUPTED]</span>"
		}

		// Left menu item
		sbNav.WriteString(fmt.Sprintf(
			`<div class="nav-item" onclick="openCoro('%d')">
				<div class="nav-id">Instance #%d%s</div>
				<div class="nav-meta">%d Steps | %d Threads</div>
			</div>`, id, id, statusTag, len(events), len(threadMap)))

		var dataPoints []string
		var markPoints []string
		lastTid := uint64(0)
		for _, e := range events {
			localTime := float64(e.TS-startTime) / 1000000.0
			state := 0
			if e.IsActive {
				state = 1
			}
			dataPoints = append(dataPoints, fmt.Sprintf("[%f, %d]", localTime, state))

			if e.TID != lastTid {
				markPoints = append(markPoints, fmt.Sprintf("{xAxis:%f, yAxis:%d, value:'TID:%d'}", localTime, state, e.TID))
				lastTid = e.TID
			}
		}

		// Right Tab page (Note: Global configuration is registered here, but the chart is not rendered immediately)
		sbContent.WriteString(fmt.Sprintf(`
			<div id="coro-%d" class="tab-pane">
				<div class="panel-header">
					<h1>Coroutine Journal: #%d</h1>
					<div class="info-bar">
						<div class="info-card"><h4>Events</h4><p>%d</p></div>
						<div class="info-card"><h4>Threads</h4><p>%d</p></div>
						<div class="info-card"><h4>Duration</h4><p>%.2f ms</p></div>
						<div class="info-card"><h4>Start TS</h4><p>%d</p></div>
					</div>
				</div>
				<div class="chart-area" id="dom-%d" style="width:100%%; min-height: 500px;"></div>
				<script>
					if (!window.chartConfigs) window.chartConfigs = {};
					window.chartConfigs['%d'] = {
						data: [%s],
						marks: [%s]
					};
				</script>
			</div>`, id, id, len(events), len(threadMap), duration, startTime, id, id, strings.Join(dataPoints, ","), strings.Join(markPoints, ",")))
	}

	fullHtml := fmt.Sprintf(htmlSkeleton, sbNav.String(), sbContent.String())
	return os.WriteFile(htmlPath, []byte(fullHtml), 0644)
}

const htmlSkeleton = `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>coroTracer Full Topology Dashboard</title>
    <script src="https://cdn.jsdelivr.net/npm/echarts@5.5.0/dist/echarts.min.js"></script>
    <style>
        body { margin: 0; background: #0d1117; color: #c9d1d9; font-family: -apple-system, sans-serif; display: flex; height: 100vh; overflow: hidden; }
        #sidebar { width: 350px; background: #161b22; border-right: 1px solid #30363d; display: flex; flex-direction: column; }
        .side-head { padding: 25px; font-size: 1.2rem; font-weight: bold; color: #58a6ff; border-bottom: 1px solid #30363d; background: #010409; }
        .nav-list { flex: 1; overflow-y: auto; }
        .nav-item { padding: 18px 25px; border-bottom: 1px solid #30363d; cursor: pointer; transition: 0.2s; }
        .nav-item:hover { background: #21262d; }
        .nav-item.active { background: #30363d; border-left: 5px solid #58a6ff; }
        .nav-id { font-family: monospace; font-weight: bold; margin-bottom: 5px; }
        .nav-meta { font-size: 0.8rem; color: #8b949e; }

        #viewport { flex: 1; position: relative; display: flex; flex-direction: column; background: #0d1117; }
        .tab-pane { display: none; height: 100%%; flex-direction: column; padding: 35px; box-sizing: border-box; overflow-y: auto; }
        .tab-pane.active { display: flex; }
        
        .panel-header { margin-bottom: 30px; }
        .info-bar { display: grid; grid-template-columns: repeat(4, 1fr); gap: 20px; margin-top: 20px; }
        .info-card { background: #161b22; padding: 15px; border-radius: 8px; border: 1px solid #30363d; }
        .info-card h4 { margin: 0; font-size: 0.75rem; color: #8b949e; text-transform: uppercase; }
        .info-card p { margin: 10px 0 0 0; font-family: monospace; color: #58a6ff; font-size: 1.1rem; }
        
        .chart-area { flex: 1; background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 20px; }
        .placeholder { position: absolute; top: 50%%; left: 50%%; transform: translate(-50%%, -50%%); color: #8b949e; text-align: center; }
    </style>
</head>
<body>
    <div id="sidebar">
        <div class="side-head">🔬 coroTracer Journals</div>
        <div class="nav-list">%s</div>
    </div>
    <div id="viewport">
        <div class="placeholder" id="init-msg"><h2>Select a Trace Instance</h2><p>Static topology reconstruction is ready.</p></div>
        %s
    </div>
    <script>
        var activeCharts = {};

        function openCoro(id) {
            document.getElementById('init-msg').style.display = 'none';
            document.querySelectorAll('.nav-item').forEach(el => el.classList.remove('active'));
            document.querySelectorAll('.tab-pane').forEach(el => el.classList.remove('active'));
            
            event.currentTarget.classList.add('active');
            var pane = document.getElementById('coro-' + id);
            pane.classList.add('active');

            if (!activeCharts[id] && window.chartConfigs && window.chartConfigs[id]) {
                var dom = document.getElementById('dom-' + id);
                var chart = echarts.init(dom, 'dark');
                var cfg = window.chartConfigs[id];
                
                chart.setOption({
                    backgroundColor: 'transparent',
                    tooltip: { trigger: 'axis' },
                    dataZoom: [{type:'inside'}, {type:'slider', bottom: 10}],
                    xAxis: { type: 'value', name: 'Offset (ms)', scale: true, splitLine: {lineStyle: {color: '#30363d'}} },
                    yAxis: { type: 'category', data: ['Suspend', 'Active'], splitLine: {show: true} },
                    series: [{
                        type: 'line', step: 'end', data: cfg.data,
                        lineStyle: { width: 3, color: '#58a6ff' },
                        itemStyle: { color: '#58a6ff' },
                        markPoint: { data: cfg.marks, symbolSize: 40 }
                    }]
                });
                activeCharts[id] = chart;
            } else if (activeCharts[id]) {
                activeCharts[id].resize();
            }
        }
        
        window.addEventListener('resize', function() {
            Object.values(activeCharts).forEach(chart => chart.resize());
        });
    </script>
</body>
</html>
`
