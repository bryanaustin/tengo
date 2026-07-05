package tengo

import (
	"context"
	"fmt"

	"github.com/bryanaustin/tengo/parser"
)

// CompiledAST is the hand-built-AST counterpart to Compiled. Where Script and
// Compiled accept source text that is parsed before compilation, CompiledAST
// accepts a *parser.File whose nodes were constructed directly in Go — no
// source text is involved.
//
// Create one with NewCompiledAST, then call Run (or RunContext) as many times
// as needed with different inputs. Each call is independent: it uses a fresh
// globals slice and VM, so a CompiledAST is safe to call from multiple
// goroutines provided that the bytecode's constant pool is not mutated between
// calls (value-type programs — ints, strings, bools — are always safe; programs
// with shared array/map literals may race if those literals are mutated).
//
// Inputs must be read via = assignment or used read-only inside the program;
// they must not be redefined with :=, which the compiler treats as a
// redeclaration error.
//
// The globals slice used internally is bounded by GlobalsSize (1024). Programs
// that define more than 1024 global variables will panic at run time.
type CompiledAST struct {
	bytecode  *Bytecode
	inputs    map[string]int // input name -> global slot
	outputs   map[string]int // output name -> global slot
	maxAllocs int64
	maxTicks  int64
}

// NewCompiledAST compiles a hand-built AST into a CompiledAST ready to Run.
//
// file must have InputFile set (see parser.NewSourceFile). The input names are
// declared as globals before compilation so the compiler resolves references to
// them as external inputs rather than failing with "unresolved reference". The
// output names are resolved after compilation to find the global slots to read
// results from; naming an output the program never defines is an error.
//
// Returns an error if any input name collides with a tengo builtin (e.g. "len",
// "string", "int") — such inputs would be silently ignored at run time because
// builtins shadow user globals.
func NewCompiledAST(file *parser.File, inputs, outputs []string) (c *CompiledAST, err error) {
	defer func() {
		if r := recover(); r != nil {
			c = nil
			err = fmt.Errorf("tengo: compile panic: %v", r)
		}
	}()

	if file == nil || file.InputFile == nil {
		return nil, fmt.Errorf("tengo: file and file.InputFile must be non-nil (see parser.NewSourceFile)")
	}

	symbols := NewSymbolTable()
	inIdx := make(map[string]int, len(inputs))
	for _, name := range inputs {
		inIdx[name] = symbols.Define(name).Index
	}

	comp := NewCompiler(file.InputFile, symbols, nil, nil, nil)

	// NewCompiler injects builtins via DefineBuiltin, which overwrites any
	// same-named user global in the symbol table. Re-resolve each input to
	// detect silent shadowing before it causes a confusing run-time failure.
	for name, expectedIdx := range inIdx {
		sym, _, ok := symbols.Resolve(name, false)
		if !ok || sym.Scope != ScopeGlobal || sym.Index != expectedIdx {
			return nil, fmt.Errorf("tengo: input %q conflicts with a tengo builtin", name)
		}
	}

	if err := comp.Compile(file); err != nil {
		return nil, fmt.Errorf("tengo: compile: %w", err)
	}

	outIdx := make(map[string]int, len(outputs))
	for _, name := range outputs {
		sym, _, ok := symbols.Resolve(name, false)
		if !ok {
			return nil, fmt.Errorf("tengo: output %q is not defined by the program", name)
		}
		outIdx[name] = sym.Index
	}

	return &CompiledAST{
		bytecode:  comp.Bytecode(),
		inputs:    inIdx,
		outputs:   outIdx,
		maxAllocs: -1,
		maxTicks:  -1,
	}, nil
}

// SetMaxAllocs sets the maximum number of object allocations per Run. A
// negative value (the default) means unlimited.
func (c *CompiledAST) SetMaxAllocs(n int64) { c.maxAllocs = n }

// SetMaxTicks sets the maximum number of VM instructions per Run. A negative
// value (the default) means unlimited. When the limit is reached Run returns
// ErrStepLimit (wrapped in a runtime error).
func (c *CompiledAST) SetMaxTicks(n int64) { c.maxTicks = n }

// Run executes the program with the given inputs and returns the declared
// outputs. Values cross the boundary as ordinary Go values (int/int64, float64,
// string, bool, []interface{}, map[string]interface{}) via
// FromInterface / ToInterface. A missing input key is treated as undefined. Each
// call uses a fresh globals slice and VM.
func (c *CompiledAST) Run(args map[string]interface{}) (results map[string]interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			results = nil
			err = fmt.Errorf("tengo: run panic: %v", r)
		}
	}()

	globals, err := c.buildGlobals(args)
	if err != nil {
		return nil, err
	}

	vm := NewVM(c.bytecode, globals, c.maxAllocs)
	vm.SetMaxTicks(c.maxTicks)
	if err := vm.Run(); err != nil {
		return nil, err
	}

	return c.extractOutputs(globals), nil
}

// RunContext is like Run but accepts a context. If the context is cancelled
// before the program completes, the VM is aborted and RunContext returns
// ctx.Err().
func (c *CompiledAST) RunContext(ctx context.Context, args map[string]interface{}) (results map[string]interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			results = nil
			err = fmt.Errorf("tengo: run panic: %v", r)
		}
	}()

	globals, err := c.buildGlobals(args)
	if err != nil {
		return nil, err
	}

	vm := NewVM(c.bytecode, globals, c.maxAllocs)
	vm.SetMaxTicks(c.maxTicks)

	ch := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				switch e := r.(type) {
				case string:
					ch <- fmt.Errorf(e)
				case error:
					ch <- e
				default:
					ch <- fmt.Errorf("tengo: run panic: %v", r)
				}
			}
		}()
		ch <- vm.Run()
	}()

	select {
	case <-ctx.Done():
		vm.Abort()
		<-ch
		return nil, ctx.Err()
	case runErr := <-ch:
		if runErr != nil {
			return nil, runErr
		}
	}

	return c.extractOutputs(globals), nil
}

// Bytecode returns the compiled bytecode (for disassembly or inspection).
func (c *CompiledAST) Bytecode() *Bytecode { return c.bytecode }

// Disassemble returns a human-readable listing of the compiled instructions.
func (c *CompiledAST) Disassemble() []string {
	return FormatInstructions(c.bytecode.MainFunction.Instructions, 0)
}

func (c *CompiledAST) buildGlobals(args map[string]interface{}) ([]Object, error) {
	globals := make([]Object, GlobalsSize)
	for name, idx := range c.inputs {
		obj, err := FromInterface(args[name])
		if err != nil {
			return nil, fmt.Errorf("tengo: input %q: %w", name, err)
		}
		globals[idx] = obj
	}
	return globals, nil
}

func (c *CompiledAST) extractOutputs(globals []Object) map[string]interface{} {
	results := make(map[string]interface{}, len(c.outputs))
	for name, idx := range c.outputs {
		results[name] = ToInterface(globals[idx])
	}
	return results
}
