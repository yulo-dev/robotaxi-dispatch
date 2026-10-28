package sim

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func pm(s Stat, prec int) string {
	return fmt.Sprintf("%.*f ± %.*f", prec, s.Mean, prec, s.Std)
}

// WriteText prints an aligned summary table for a terminal.
func WriteText(w io.Writer, exp Experiment) {
	fmt.Fprintf(w, "%-20s %14s %14s %14s %14s %14s %14s %14s %12s\n",
		"strategy", "match_rate", "wait_p50_s", "wait_p95_s", "wait_avg_s", "match_p95_s", "empty_km_pct", "pickup_km", "hit_rate")
	for _, a := range exp.Aggregates {
		fmt.Fprintf(w, "%-20s %14s %14s %14s %14s %14s %14s %14s %12s\n",
			a.Strategy, pm(a.MatchRate, 3), pm(a.PickupWaitP50, 0), pm(a.PickupWaitP95, 0),
			pm(a.PickupWaitAvg, 0), pm(a.TimeToMatchP95, 0), pm(a.EmptyKmPct, 1),
			pm(a.AvgPickupKm, 2), pm(a.CacheHitRate, 2))
	}
}

// WriteMainTable writes the default-configuration table as Markdown.
func WriteMainTable(w io.Writer, exp Experiment) {
	fmt.Fprintln(w, "| strategy | requested | match rate | pickup wait p50 (s) | pickup wait p95 (s) | pickup wait avg (s) | time to match p95 (s) | empty km % | avg pickup km | A* cache hit rate | runtime (ms) |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|---|")
	for _, a := range exp.Aggregates {
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			a.Strategy, pm(a.Requested, 0), pm(a.MatchRate, 3), pm(a.PickupWaitP50, 0),
			pm(a.PickupWaitP95, 0), pm(a.PickupWaitAvg, 0), pm(a.TimeToMatchP95, 0),
			pm(a.EmptyKmPct, 1), pm(a.AvgPickupKm, 2), pm(a.CacheHitRate, 2), pm(a.RuntimeMs, 0))
	}
}

// SweepPoint is one fleet size in a sweep.
type SweepPoint struct {
	Vehicles   int        `json:"vehicles"`
	Experiment Experiment `json:"experiment"`
}

// WriteSweepTable writes the fleet-size sweep as Markdown.
func WriteSweepTable(w io.Writer, pts []SweepPoint) {
	fmt.Fprintln(w, "| vehicles | strategy | match rate | pickup wait p50 (s) | pickup wait p95 (s) | time to match p95 (s) | empty km % |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|")
	for _, p := range pts {
		for _, a := range p.Experiment.Aggregates {
			fmt.Fprintf(w, "| %d | %s | %s | %s | %s | %s | %s |\n",
				p.Vehicles, a.Strategy, pm(a.MatchRate, 3), pm(a.PickupWaitP50, 0),
				pm(a.PickupWaitP95, 0), pm(a.TimeToMatchP95, 0), pm(a.EmptyKmPct, 1))
		}
	}
}

// WriteGoBenchTable converts `go test -bench` output into a Markdown table.
func WriteGoBenchTable(w io.Writer, r io.Reader) error {
	fmt.Fprintln(w, "| benchmark | iterations | ns/op | ms/op |")
	fmt.Fprintln(w, "|---|---|---|---|")
	sc := bufio.NewScanner(r)
	rows := 0
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") || f[3] != "ns/op" {
			continue
		}
		var ns float64
		if _, err := fmt.Sscanf(f[2], "%g", &ns); err != nil {
			continue
		}
		name := f[0]
		if i := strings.LastIndex(name, "-"); i > 0 {
			name = name[:i] // drop GOMAXPROCS suffix
		}
		fmt.Fprintf(w, "| %s | %s | %.0f | %.3f |\n", name, f[1], ns, ns/1e6)
		rows++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("no benchmark lines found")
	}
	return nil
}
