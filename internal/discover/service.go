package discover

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

// Discoverer owns one discovery stream and the channel it closes on return.
type Discoverer struct {
	// tool is the only boundary allowed to invoke the Go command.
	tool gotool.Tool
	// module scopes user patterns to the current module contract.
	module gotool.Module
	// patternDir preserves the caller's working directory for relative patterns.
	patternDir string
	// patterns are the user package patterns whose matches become selected.
	patterns []string
	// out is owned by Run so downstream services have one close signal.
	out chan<- gotool.Package
	// rand is injected in tests to prove shuffling without disabling it.
	rand *rand.Rand
}

// New returns a discoverer that emits package metadata to out.
func New(
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
	out chan<- gotool.Package,
) *Discoverer {
	assert.NotNil(tool, "discovery needs a go tool")
	assert.NotNil(out, "discovery needs a package channel")
	return newWithRand(tool, module, patternDir, patterns, out, randomRand())
}

// newWithRand preserves a deterministic test seam for always-on shuffling.
func newWithRand(
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
	out chan<- gotool.Package,
	rng *rand.Rand,
) *Discoverer {
	assert.NotNil(rng, "discovery needs a random source")
	return &Discoverer{
		tool:       tool,
		module:     module,
		patternDir: patternDir,
		patterns:   patterns,
		out:        out,
		rand:       rng,
	}
}

// Run discovers packages, emits them with cancellation, and closes out.
func (d *Discoverer) Run(ctx context.Context) error {
	defer close(d.out)
	packages, err := d.discover()
	if err != nil {
		return err
	}
	shufflePackages(packages, d.rand)
	for _, p := range packages {
		select {
		case d.out <- p:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// randomRand returns a process-local source so every run shuffles by default.
func randomRand() *rand.Rand {
	seed := uint64(time.Now().UnixNano())
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// shufflePackages changes only channel emission order, never closure contents.
func shufflePackages(packages []gotool.Package, rng *rand.Rand) {
	rng.Shuffle(len(packages), func(i int, j int) {
		packages[i], packages[j] = packages[j], packages[i]
	})
}
