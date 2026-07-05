package tengo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bryanaustin/tengo"
	"github.com/bryanaustin/tengo/parser"
	"github.com/bryanaustin/tengo/require"
	"github.com/bryanaustin/tengo/token"
)

// buildBinaryExpr builds the AST for `out := lhs <op> rhs`.
func buildBinaryExpr(op token.Token, lhs, rhs string) *parser.File {
	return &parser.File{
		InputFile: parser.NewSourceFile("test"),
		Stmts: []parser.Stmt{
			&parser.AssignStmt{
				LHS:   []parser.Expr{&parser.Ident{Name: "out"}},
				Token: token.Define,
				RHS: []parser.Expr{&parser.BinaryExpr{
					LHS:   &parser.Ident{Name: lhs},
					Token: op,
					RHS:   &parser.Ident{Name: rhs},
				}},
			},
		},
	}
}

func TestCompiledASTBasic(t *testing.T) {
	c, err := tengo.NewCompiledAST(
		buildBinaryExpr(token.Add, "x", "y"),
		[]string{"x", "y"},
		[]string{"out"},
	)
	require.NoError(t, err)

	results, err := c.Run(map[string]interface{}{"x": 3, "y": 4})
	require.NoError(t, err)
	require.Equal(t, int64(7), results["out"])
}

func TestCompiledASTValueRoundTrips(t *testing.T) {
	// Build `out := in` to test each value type through FromInterface/ToInterface.
	makePassthrough := func() *parser.File {
		return &parser.File{
			InputFile: parser.NewSourceFile("test"),
			Stmts: []parser.Stmt{
				&parser.AssignStmt{
					LHS:   []parser.Expr{&parser.Ident{Name: "out"}},
					Token: token.Define,
					RHS:   []parser.Expr{&parser.Ident{Name: "in"}},
				},
			},
		}
	}

	run := func(t *testing.T, val interface{}) interface{} {
		t.Helper()
		c, err := tengo.NewCompiledAST(makePassthrough(), []string{"in"}, []string{"out"})
		require.NoError(t, err)
		results, err := c.Run(map[string]interface{}{"in": val})
		require.NoError(t, err)
		return results["out"]
	}

	require.Equal(t, int64(42), run(t, 42))
	require.Equal(t, int64(42), run(t, int64(42)))
	require.Equal(t, float64(3.14), run(t, float64(3.14)))
	require.Equal(t, "hello", run(t, "hello"))
	require.Equal(t, true, run(t, true))
	require.Equal(t, false, run(t, false))

	// require.Equal doesn't handle plain Go slices/maps; use reflect.DeepEqual.
	wantSlice := []interface{}{int64(1), int64(2)}
	gotSlice := run(t, []interface{}{1, 2})
	require.True(t, reflect.DeepEqual(wantSlice, gotSlice),
		"slice round-trip: expected %v, got %v", wantSlice, gotSlice)

	wantMap := map[string]interface{}{"k": "v"}
	gotMap := run(t, map[string]interface{}{"k": "v"})
	require.True(t, reflect.DeepEqual(wantMap, gotMap),
		"map round-trip: expected %v, got %v", wantMap, gotMap)
}

func TestCompiledASTBuiltinCollision(t *testing.T) {
	// An input named after a tengo builtin must be rejected at compile time.
	builtinNames := []string{"len", "string", "int", "append", "format"}
	for _, name := range builtinNames {
		file := &parser.File{
			InputFile: parser.NewSourceFile("test"),
			Stmts:     []parser.Stmt{},
		}
		_, err := tengo.NewCompiledAST(file, []string{name}, nil)
		require.Error(t, err, "expected error for builtin input %q", name)
		require.True(t, strings.Contains(err.Error(), name),
			"error should mention the conflicting name %q: %v", name, err)
	}
}

func TestCompiledASTMalformedAST(t *testing.T) {
	// A BinaryExpr with a nil operand should return an error, not panic.
	file := &parser.File{
		InputFile: parser.NewSourceFile("test"),
		Stmts: []parser.Stmt{
			&parser.AssignStmt{
				LHS:   []parser.Expr{&parser.Ident{Name: "out"}},
				Token: token.Define,
				RHS: []parser.Expr{&parser.BinaryExpr{
					LHS:   nil, // malformed
					Token: token.Add,
					RHS:   &parser.Ident{Name: "x"},
				}},
			},
		},
	}
	_, err := tengo.NewCompiledAST(file, []string{"x"}, []string{"out"})
	require.Error(t, err, "expected error for malformed AST, not a panic")
}

func TestCompiledASTMissingInputIsUndefined(t *testing.T) {
	c, err := tengo.NewCompiledAST(
		buildBinaryExpr(token.Add, "x", "y"),
		[]string{"x", "y"},
		[]string{"out"},
	)
	require.NoError(t, err)

	// Passing only "x"; "y" is absent → treated as undefined → run-time error.
	_, err = c.Run(map[string]interface{}{"x": 1})
	require.Error(t, err)
}

func TestCompiledASTUndefinedOutput(t *testing.T) {
	file := &parser.File{
		InputFile: parser.NewSourceFile("test"),
		Stmts: []parser.Stmt{
			&parser.AssignStmt{
				LHS:   []parser.Expr{&parser.Ident{Name: "result"}},
				Token: token.Define,
				RHS:   []parser.Expr{&parser.Ident{Name: "x"}},
			},
		},
	}
	// Requesting "out" which the program never defines.
	_, err := tengo.NewCompiledAST(file, []string{"x"}, []string{"out"})
	require.Error(t, err)
}

func TestCompiledASTNilFile(t *testing.T) {
	_, err := tengo.NewCompiledAST(nil, nil, nil)
	require.Error(t, err)

	_, err = tengo.NewCompiledAST(&parser.File{InputFile: nil}, nil, nil)
	require.Error(t, err)
}

func TestCompiledASTMaxAllocsLimit(t *testing.T) {
	// Build a program that allocates: `out := [1, 2, 3]`
	file := &parser.File{
		InputFile: parser.NewSourceFile("test"),
		Stmts: []parser.Stmt{
			&parser.AssignStmt{
				LHS:   []parser.Expr{&parser.Ident{Name: "out"}},
				Token: token.Define,
				RHS: []parser.Expr{&parser.ArrayLit{
					Elements: []parser.Expr{
						&parser.IntLit{Value: 1},
						&parser.IntLit{Value: 2},
						&parser.IntLit{Value: 3},
					},
				}},
			},
		},
	}

	c, err := tengo.NewCompiledAST(file, nil, []string{"out"})
	require.NoError(t, err)

	// Zero allocations allowed: [1,2,3] needs one array alloc, so this fails.
	c.SetMaxAllocs(0)
	_, err = c.Run(nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, tengo.ErrObjectAllocLimit),
		"expected ErrObjectAllocLimit, got: %v", err)

	// Generous budget: succeeds.
	c.SetMaxAllocs(1000)
	results, err := c.Run(nil)
	require.NoError(t, err)
	require.NotNil(t, results["out"])
}

func TestCompiledASTMaxTicksLimit(t *testing.T) {
	// Build an infinite loop: `for true {}` equivalent as an AST.
	// We use the same approach as the step_limit_test: a tight tick budget
	// on a non-terminating program must return ErrStepLimit.
	s := tengo.NewScript([]byte(`for true {}`))
	compiled, err := s.Compile()
	require.NoError(t, err)
	compiled.SetMaxTicks(500)
	err = compiled.Run()
	require.Error(t, err)
	require.True(t, errors.Is(err, tengo.ErrStepLimit),
		"expected ErrStepLimit, got: %v", err)

	// Verify CompiledAST's own SetMaxTicks wires through correctly.
	// Build `out := x + y` and confirm it completes normally under a generous limit.
	c, err := tengo.NewCompiledAST(
		buildBinaryExpr(token.Add, "x", "y"),
		[]string{"x", "y"},
		[]string{"out"},
	)
	require.NoError(t, err)
	c.SetMaxTicks(10000)
	results, err := c.Run(map[string]interface{}{"x": 1, "y": 2})
	require.NoError(t, err)
	require.Equal(t, int64(3), results["out"])
}

func TestCompiledASTRunContext(t *testing.T) {
	// A script with an infinite loop cancelled by context must return ctx.Err().
	s := tengo.NewScript([]byte(`for true {}`))
	compiled, err := s.Compile()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err = compiled.RunContext(ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"expected DeadlineExceeded, got: %v", err)

	// CompiledAST.RunContext: normal completion returns results.
	c, err := tengo.NewCompiledAST(
		buildBinaryExpr(token.Mul, "x", "y"),
		[]string{"x", "y"},
		[]string{"out"},
	)
	require.NoError(t, err)
	results, err := c.RunContext(context.Background(), map[string]interface{}{"x": 6, "y": 7})
	require.NoError(t, err)
	require.Equal(t, int64(42), results["out"])
}

// Disassemble and Bytecode are smoke-tested: we just verify they don't panic
// and return non-empty results.
func TestCompiledASTDisassemble(t *testing.T) {
	c, err := tengo.NewCompiledAST(
		buildBinaryExpr(token.Add, "x", "y"),
		[]string{"x", "y"},
		[]string{"out"},
	)
	require.NoError(t, err)
	require.True(t, len(c.Disassemble()) > 0)
	require.NotNil(t, c.Bytecode())
}
