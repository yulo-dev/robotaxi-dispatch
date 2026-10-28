package sim

import (
	"sync"

	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
)

// Experiment is a set of runs over several seeds and all strategies.
type Experiment struct {
	Config     Config      `json:"-"`
	Seeds      []int64     `json:"seeds"`
	Results    []Result    `json:"results"`    // seed-major, strategy order
	Aggregates []Aggregate `json:"aggregates"` // one per strategy
}

// RunExperiment generates demand once per seed and runs every strategy on it.
// Runs execute on up to parallel goroutines; results do not depend on
// parallelism, only RuntimeMs does.
func RunExperiment(n *geo.Network, cfg Config, seeds []int64, strategies []Strategy, parallel int) Experiment {
	if parallel < 1 {
		parallel = 1
	}
	scenarios := make([]Scenario, len(seeds))
	for i, s := range seeds {
		scenarios[i] = NewScenario(n, cfg, s)
	}
	results := make([]Result, len(seeds)*len(strategies))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				si, ti := j/len(strategies), j%len(strategies)
				results[j] = Run(n, scenarios[si], cfg, strategies[ti])
			}
		}()
	}
	for j := range results {
		jobs <- j
	}
	close(jobs)
	wg.Wait()

	exp := Experiment{Config: cfg, Seeds: seeds, Results: results}
	for ti, st := range strategies {
		var rs []Result
		for si := range seeds {
			rs = append(rs, results[si*len(strategies)+ti])
		}
		exp.Aggregates = append(exp.Aggregates, AggregateResults(st.Name, rs))
	}
	return exp
}

// SeedList returns seeds 1..n.
func SeedList(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}
