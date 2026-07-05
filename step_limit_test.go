package tengo_test

import (
	"errors"
	"testing"

	"github.com/bryanaustin/tengo"
	"github.com/bryanaustin/tengo/require"
)

// compileScript is a helper that compiles a source string and panics on error.
func compileScript(t *testing.T, src string) *tengo.Compiled {
	t.Helper()
	s := tengo.NewScript([]byte(src))
	compiled, err := s.Compile()
	require.NoError(t, err)
	return compiled
}

func TestStepLimitInfiniteLoop(t *testing.T) {
	compiled := compileScript(t, `for true {}`)
	compiled.SetMaxTicks(1000)
	err := compiled.Run()
	require.Error(t, err)
	require.True(t, errors.Is(err, tengo.ErrStepLimit),
		"expected ErrStepLimit, got: %v", err)
}

func TestStepLimitFiniteProgram(t *testing.T) {
	compiled := compileScript(t, `
x := 0
for i := 0; i < 10; i++ {
    x = x + i
}
out := x
`)
	compiled.SetMaxTicks(10000)
	require.NoError(t, compiled.Run())
}

func TestStepLimitUnlimitedDefault(t *testing.T) {
	// Without SetMaxTicks the program runs without a step limit. We can't run
	// an infinite loop here, so we just verify a normal program is unaffected.
	compiled := compileScript(t, `out := 1 + 2`)
	require.NoError(t, compiled.Run())
}

func TestStepLimitBoundary(t *testing.T) {
	// Count the exact number of ticks for a known program by finding the
	// threshold between success and failure.
	//
	// `out := 1` compiles to: OpConstant, OpSetGlobal, OpSuspend — 3 opcodes.
	src := `out := 1`

	// Run once with a very generous limit to confirm success.
	compiled := compileScript(t, src)
	compiled.SetMaxTicks(1000)
	require.NoError(t, compiled.Run())

	// Find the minimum ticks by scanning down from a small number.
	// The program should succeed at exactly N ticks and fail at N-1.
	var minTicks int64
	for n := int64(100); n >= 1; n-- {
		c := compileScript(t, src)
		c.SetMaxTicks(n)
		if err := c.Run(); err != nil {
			if errors.Is(err, tengo.ErrStepLimit) {
				minTicks = n + 1 // last success was n+1
			}
			break
		}
		minTicks = n
	}
	require.True(t, minTicks > 0, "could not determine minimum tick count")

	// Confirm boundary: minTicks succeeds, minTicks-1 fails.
	c1 := compileScript(t, src)
	c1.SetMaxTicks(minTicks)
	require.NoError(t, c1.Run())

	if minTicks > 1 {
		c2 := compileScript(t, src)
		c2.SetMaxTicks(minTicks - 1)
		err := c2.Run()
		require.Error(t, err)
		require.True(t, errors.Is(err, tengo.ErrStepLimit))
	}
}

func TestStepLimitDeterminism(t *testing.T) {
	// The same program must hit the step limit at the same tick count on every
	// run. We verify this by finding the threshold once and confirming it
	// across multiple repetitions.
	src := `x := 0; for i := 0; i < 5; i++ { x = x + i }`

	var threshold int64
	for n := int64(1000); n >= 1; n-- {
		c := compileScript(t, src)
		c.SetMaxTicks(n)
		if err := c.Run(); err != nil {
			if errors.Is(err, tengo.ErrStepLimit) {
				threshold = n + 1
			}
			break
		}
		threshold = n
	}
	require.True(t, threshold > 0)

	// Run 5 times at threshold-1: every run must produce ErrStepLimit.
	for i := 0; i < 5; i++ {
		c := compileScript(t, src)
		c.SetMaxTicks(threshold - 1)
		err := c.Run()
		require.Error(t, err, "run %d: expected error", i)
		require.True(t, errors.Is(err, tengo.ErrStepLimit),
			"run %d: expected ErrStepLimit, got: %v", i, err)
	}
}

func TestStepLimitOrthogonality(t *testing.T) {
	// maxAllocs and maxTicks are independent guards; the first to trip wins.

	// Zero allocations on a program that creates an array: maxAllocs fires.
	s := tengo.NewScript([]byte(`a := [1, 2, 3]`))
	s.SetMaxAllocs(0) // 0 allocations — array creation fails immediately
	compiled, err := s.Compile()
	require.NoError(t, err)
	compiled.SetMaxTicks(1_000_000)
	err = compiled.Run()
	require.Error(t, err)
	require.True(t, errors.Is(err, tengo.ErrObjectAllocLimit),
		"expected ErrObjectAllocLimit, got: %v", err)

	// Tight step budget on a non-allocating loop: maxTicks fires.
	compiled2 := compileScript(t, `for true {}`)
	compiled2.SetMaxTicks(500)
	err = compiled2.Run()
	require.Error(t, err)
	require.True(t, errors.Is(err, tengo.ErrStepLimit),
		"expected ErrStepLimit, got: %v", err)
}
