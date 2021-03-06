// The compile package defines the Skylark bytecode compiler.
// It is an internal package of the Skylark interpreter and is not directly accessible to clients.
//
// The compiler generates byte code with optional uint32 operands for a
// virtual machine with the following components:
//   - a program counter, which is an index into the byte code array.
//   - an operand stack, whose maximum size is computed for each function by the compiler.
//   - an stack of active iterators.
//   - an array of local variables.
//     The number of local variables and their indices are computed by the resolver.
//   - an array of free variables, for nested functions.
//     As with locals, these are computed by the resolver.
//   - an array of global variables, shared among all functions in the same module.
//     All elements are initially nil.
//   - two maps of predeclared and universal identifiers.
//
// A line number table maps each program counter value to a source position;
// these source positions do not currently record column information.
//
// Operands, logically uint32s, are encoded using little-endian 7-bit
// varints, the top bit indicating that more bytes follow.
//
package compile

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/google/skylark/resolve"
	"github.com/google/skylark/syntax"
)

const debug = false // TODO(adonovan): use a bitmap of options; and regexp to match files

// Increment this to force recompilation of saved bytecode files.
const Version = 3

type Opcode uint8

// "x DUP x x" is a "stack picture" that describes the state of the
// stack before and after execution of the instruction.
//
// OP<index> indicates an immediate operand that is an index into the
// specified table: locals, names, freevars, constants.
const (
	NOP Opcode = iota // - NOP -

	// stack operations
	DUP  //   x DUP x x
	DUP2 // x y DUP2 x y x y
	POP  //   x POP -
	EXCH // x y EXCH y x

	// binary comparisons
	// (order must match Token)
	LT
	GT
	GE
	LE
	EQL
	NEQ

	// binary arithmetic
	// (order must match Token)
	PLUS
	MINUS
	STAR
	SLASH
	SLASHSLASH
	PERCENT
	AMP
	PIPE
	CIRCUMFLEX
	LTLT
	GTGT

	IN

	// unary operators
	UPLUS  // x UPLUS x
	UMINUS // x UMINUS -x
	TILDE  // x TILDE ~x

	NONE  // - NONE None
	TRUE  // - TRUE True
	FALSE // - FALSE False

	ITERPUSH    //       iterable ITERPUSH -     [pushes the iterator stack]
	ITERPOP     //              - ITERPOP -      [pops the iterator stack]
	NOT         //          value NOT bool
	RETURN      //          value RETURN -
	SETINDEX    //        a i new SETINDEX -
	INDEX       //            a i INDEX elem
	SETDICT     // dict key value SETDICT -
	SETDICTUNIQ // dict key value SETDICTUNIQ -
	APPEND      //      list elem APPEND -
	SLICE       //   x lo hi step SLICE slice
	INPLACE_ADD //            x y INPLACE_ADD z      where z is x+y or x.extend(y)
	MAKEDICT    //              - MAKEDICT dict
	MAKESET     //              - MAKESET set    (if sets are enabled)

	EXCEPTPOP //          eh EXCEPTPOP -  [pops the exception handler stack]
	ERROR     // extype <err>ERROR err    [pops the expected exception type;
	//									   pushes the current error (handled exception) onto the value stack]

	// --- opcodes with an argument must go below this line ---

	// control flow
	JMP     //            - JMP<addr>     -
	CJMP    //         cond CJMP<addr>    -
	ITERJMP //            - ITERJMP<addr> elem   (and fall through) [acts on topmost iterator]
	//       or:          - ITERJMP<addr> -      (and jump)

	CONSTANT    //                - CONSTANT<constant>  value
	MAKETUPLE   //        x1 ... xn MAKETUPLE<n>        tuple
	MAKELIST    //        x1 ... xn MAKELIST<n>         list
	MAKEFUNC    //      args kwargs MAKEFUNC<func>      fn
	LOAD        //  from1 ... fromN module LOAD<n>      v1 ... vN
	SETLOCAL    //            value SETLOCAL<local>     -
	SETGLOBAL   //            value SETGLOBAL<global>   -
	LOCAL       //                - LOCAL<local>        value
	FREE        //                - FREE<freevar>       value
	GLOBAL      //                - GLOBAL<global>      value
	PREDECLARED //                - PREDECLARED<name>   value
	UNIVERSAL   //                - UNIVERSAL<name>     value
	ATTR        //                x ATTR<name>          y           y = x.name
	SETFIELD    //              x y SETFIELD<name>      -           x.name = y
	UNPACK      //         iterable UNPACK<n>           vn ... v1

	EXCEPTPUSH // - EXCEPTPUSH<addr> -     [pushes the exception handler stack]

	// n>>8 is #positional args and n&0xff is #named args (pairs).
	CALL        // fn positional named                CALL<n>        result
	CALL_VAR    // fn positional named *args          CALL_VAR<n>    result
	CALL_KW     // fn positional named       **kwargs CALL_KW<n>     result
	CALL_VAR_KW // fn positional named *args **kwargs CALL_VAR_KW<n> result

	OpcodeArgMin = JMP
	OpcodeMax    = CALL_VAR_KW
)

var opcodeNames = [...]string{
	AMP:         "amp",
	APPEND:      "append",
	ATTR:        "attr",
	CALL:        "call",
	CALL_KW:     "call_kw ",
	CALL_VAR:    "call_var",
	CALL_VAR_KW: "call_var_kw",
	CIRCUMFLEX:  "circumflex",
	CJMP:        "cjmp",
	CONSTANT:    "constant",
	DUP2:        "dup2",
	DUP:         "dup",
	EQL:         "eql",
	ERROR:       "error",
	EXCEPTPOP:   "exceptpop",
	EXCEPTPUSH:  "exceptpush",
	EXCH:        "exch",
	FALSE:       "false",
	FREE:        "free",
	GE:          "ge",
	GLOBAL:      "global",
	GT:          "gt",
	GTGT:        "gtgt",
	IN:          "in",
	INDEX:       "index",
	INPLACE_ADD: "inplace_add",
	ITERJMP:     "iterjmp",
	ITERPOP:     "iterpop",
	ITERPUSH:    "iterpush",
	JMP:         "jmp",
	LE:          "le",
	LOAD:        "load",
	LOCAL:       "local",
	LT:          "lt",
	LTLT:        "ltlt",
	MAKEDICT:    "makedict",
	MAKEFUNC:    "makefunc",
	MAKELIST:    "makelist",
	MAKESET:     "makeset",
	MAKETUPLE:   "maketuple",
	MINUS:       "minus",
	NEQ:         "neq",
	NONE:        "none",
	NOP:         "nop",
	NOT:         "not",
	PERCENT:     "percent",
	PIPE:        "pipe",
	PLUS:        "plus",
	POP:         "pop",
	PREDECLARED: "predeclared",
	RETURN:      "return",
	SETDICT:     "setdict",
	SETDICTUNIQ: "setdictuniq",
	SETFIELD:    "setfield",
	SETGLOBAL:   "setglobal",
	SETINDEX:    "setindex",
	SETLOCAL:    "setlocal",
	SLASH:       "slash",
	SLASHSLASH:  "slashslash",
	SLICE:       "slice",
	STAR:        "star",
	TILDE:       "tilde",
	TRUE:        "true",
	UMINUS:      "uminus",
	UNIVERSAL:   "universal",
	UNPACK:      "unpack",
	UPLUS:       "uplus",
}

// StackEffect returns the number of stack pops and pushes executed during op.
func StackEffect(op Opcode) (pops, pushes int) {
	se := stackEffect[op]
	return int(se >> 4), int(se & 0xf)
}

func IsVariableStackEffect(op Opcode) bool {
	return stackEffect[op] == variableStackEffect
}

// poppush packs a stack-effect pair into a uint8
func poppush(pops, pushes int) uint8 {
	return uint8(pops<<4) | (uint8(pushes) & 0xf)
}

const (
	variableStackEffect = 0xEE // poppush(14, 14)
	invalidStackEffect  = 0xFF // poppush(15, 15)
)

// stackEffect records the effect on the size of the operand stack of
// each kind of instruction. For some instructions this requires computation.
var stackEffect [int(OpcodeMax) + 1]uint8

func init() {
	for op := Opcode(0); op <= OpcodeMax; op++ {
		if opcodeNames[op] == "" {
			var prev, next string
			if op > 0 {
				prev = opcodeNames[op-1]
			}
			if op < OpcodeMax {
				next = opcodeNames[op+1]
			}
			log.Fatalf("Compile: missing opcode name for %v; previous=%s next=%s", op, prev, next)
		}
	}

	for i, _ := range stackEffect {
		stackEffect[i] = invalidStackEffect
	}

	stackEffect[AMP] = poppush(2, 1)
	stackEffect[APPEND] = poppush(2, 0)
	stackEffect[ATTR] = poppush(1, 1)
	stackEffect[CALL] = variableStackEffect
	stackEffect[CALL_KW] = variableStackEffect
	stackEffect[CALL_VAR] = variableStackEffect
	stackEffect[CALL_VAR_KW] = variableStackEffect
	stackEffect[CIRCUMFLEX] = poppush(2, 1)
	stackEffect[CJMP] = poppush(1, 0)
	stackEffect[CONSTANT] = poppush(0, 1)
	stackEffect[DUP2] = poppush(2, 4)
	stackEffect[DUP] = poppush(1, 2)
	stackEffect[EQL] = poppush(2, 1)
	stackEffect[ERROR] = poppush(1, 1)
	stackEffect[EXCEPTPOP] = poppush(0, 0)
	stackEffect[EXCEPTPUSH] = poppush(0, 0)
	stackEffect[EXCH] = poppush(2, 2)
	stackEffect[FALSE] = poppush(0, 1)
	stackEffect[FREE] = poppush(0, 1)
	stackEffect[GE] = poppush(1, 0)
	stackEffect[GLOBAL] = poppush(0, 1)
	stackEffect[GT] = poppush(2, 1)
	stackEffect[GTGT] = poppush(2, 1)
	stackEffect[IN] = poppush(2, 1)
	stackEffect[INDEX] = poppush(2, 1)
	stackEffect[INPLACE_ADD] = poppush(2, 1)
	stackEffect[ITERJMP] = variableStackEffect
	stackEffect[ITERPOP] = poppush(0, 0)
	stackEffect[ITERPUSH] = poppush(1, 0)
	stackEffect[JMP] = poppush(0, 0)
	stackEffect[LE] = poppush(2, 1)
	stackEffect[LOAD] = variableStackEffect
	stackEffect[LOCAL] = poppush(0, 1)
	stackEffect[LT] = poppush(2, 1)
	stackEffect[LTLT] = poppush(2, 1)
	stackEffect[MAKEDICT] = poppush(0, 1)
	stackEffect[MAKEFUNC] = poppush(2, 1)
	stackEffect[MAKELIST] = variableStackEffect
	stackEffect[MAKETUPLE] = variableStackEffect
	stackEffect[MAKESET] = poppush(0, 1)
	stackEffect[MINUS] = poppush(2, 1)
	stackEffect[NEQ] = poppush(2, 1)
	stackEffect[NONE] = poppush(0, 1)
	stackEffect[NOP] = poppush(0, 0)
	stackEffect[NOT] = poppush(1, 1)
	stackEffect[PERCENT] = poppush(2, 1)
	stackEffect[PIPE] = poppush(2, 1)
	stackEffect[PLUS] = poppush(2, 1)
	stackEffect[POP] = poppush(1, 0)
	stackEffect[PREDECLARED] = poppush(0, 1)
	stackEffect[RETURN] = poppush(1, 0)
	stackEffect[SETDICT] = poppush(3, 0)
	stackEffect[SETDICTUNIQ] = poppush(3, 0)
	stackEffect[SETFIELD] = poppush(2, 0)
	stackEffect[SETGLOBAL] = poppush(1, 0)
	stackEffect[SETINDEX] = poppush(3, 0)
	stackEffect[SETLOCAL] = poppush(1, 0)
	stackEffect[SLASH] = poppush(2, 1)
	stackEffect[SLASHSLASH] = poppush(2, 1)
	stackEffect[SLICE] = poppush(4, 1)
	stackEffect[STAR] = poppush(2, 1)
	stackEffect[TILDE] = poppush(1, 1)
	stackEffect[TRUE] = poppush(0, 1)
	stackEffect[UMINUS] = poppush(1, 1)
	stackEffect[UNIVERSAL] = poppush(0, 1)
	stackEffect[UNPACK] = variableStackEffect
	stackEffect[UPLUS] = poppush(1, 1)

	for i, v := range stackEffect {
		if v == invalidStackEffect {
			log.Fatalf("Compile: missing opcode stack effect for %s", opcodeNames[Opcode(i)])
		}
	}
}

func DecodeOp(code []byte, pc uint32) (op Opcode, arg uint32, nextpc uint32, ok bool) {
	op = Opcode(code[pc])
	pc++
	if op >= OpcodeArgMin {
		// TODO(adonovan): opt: profile this.
		// Perhaps compiling big endian would be less work to decode?
		for s := uint(0); ; s += 7 {
			if int(pc) >= len(code) {
				return op, arg, pc, false
			}
			b := code[pc]
			pc++
			arg |= uint32(b&0x7f) << s
			if b < 0x80 {
				break
			}
		}
	}
	return op, arg, pc, true
}

func DecodeOpUnsafe(code []byte, pc uint32) (op Opcode, arg uint32, nextpc uint32) {
	op = Opcode(code[pc])
	pc++
	if op >= OpcodeArgMin {
		// TODO(adonovan): opt: profile this.
		// Perhaps compiling big endian would be less work to decode?
		for s := uint(0); ; s += 7 {
			b := code[pc]
			pc++
			arg |= uint32(b&0x7f) << s
			if b < 0x80 {
				break
			}
		}
	}
	return op, arg, pc
}

func (op Opcode) String() string {
	if op < OpcodeMax {
		return opcodeNames[op]
	}
	return fmt.Sprintf("illegal op (%d)", op)
}

// A Program is a Skylark file in executable form.
//
// Programs are serialized by the gobProgram function,
// which must be updated whenever this declaration is changed.
type Program struct {
	Loads     []Ident       // name (really, string) and position of each load stmt
	Names     []string      // names of attributes and predeclared variables
	Constants []interface{} // = string | int64 | float64 | *big.Int
	Functions []*Funcode
	Globals   []Ident  // for error messages and tracing
	Toplevel  *Funcode // module initialization function
}

// A Funcode is the code of a compiled Skylark function.
//
// Funcodes are serialized by the gobFunc function,
// which must be updated whenever this declaration is changed.
type Funcode struct {
	Prog                  *Program
	Pos                   syntax.Position // position of def or lambda token
	Name                  string          // name of this function
	Code                  []byte          // the byte code
	Pclinetab             []uint16        // mapping from pc to linenum
	Locals                []Ident         // for error messages and tracing
	Freevars              []Ident         // for tracing
	MaxStack              int
	NumParams             int
	HasVarargs, HasKwargs bool
}

// An Ident is the name and position of an identifier.
type Ident struct {
	Name string
	Pos  syntax.Position
}

// A pcomp holds the compiler state for a Program.
type pcomp struct {
	prog *Program // what we're building

	names     map[string]uint32
	constants map[interface{}]uint32
	functions map[*Funcode]uint32
}

// An fcomp holds the compiler state for a Funcode.
type fcomp struct {
	fn *Funcode // what we're building

	pcomp      *pcomp
	pos        syntax.Position // current position of generated code
	loops      []loop
	exhandlers []int // loop-stack index of enclosing loop for each active try/except statement
	block      *block
}

type loop struct {
	break_, continue_ *block
}

type block struct {
	insns []insn

	// If the last insn is a RETURN, jmp and cjmp are nil.
	// If the last insn is a CJMP or ITERJMP,
	//  cjmp and jmp are the "true" and "false" successors.
	// Otherwise, jmp is the sole successor.
	jmp, cjmp, except *block

	initialstack int // for stack depth computation

	// Used during encoding
	index int // -1 => not encoded yet
	addr  uint32
}

type insn struct {
	op   Opcode
	arg  uint32
	line int32
}

func (fn *Funcode) Position(pc uint32) syntax.Position {
	// Conceptually the table contains rows of the form (pc uint32,
	// line int32).  Since the pc always increases, usually by a
	// small amount, and the line number typically also does too
	// although it may decrease, again typically by a small amount,
	// we use delta encoding, starting from {pc: 0, line: 0}.
	//
	// Each entry is encoded in 16 bits.
	// The top 8 bits are the unsigned delta pc; the next 7 bits are
	// the signed line number delta; and the bottom bit indicates
	// that more rows follow because one of the deltas was maxed out.
	//
	// TODO(adonovan): opt: improve the encoding; include the column.

	pos := fn.Pos // copy the (annoyingly inaccessible) filename
	pos.Line = 0
	pos.Col = 0

	// Position returns the record for the
	// largest PC value not greater than 'pc'.
	var prevpc uint32
	complete := true
	for _, x := range fn.Pclinetab {
		nextpc := prevpc + uint32(x>>8)
		if complete && nextpc > pc {
			return pos
		}
		prevpc = nextpc
		pos.Line += int32(int8(x) >> 1) // sign extend Δline from 7 to 32 bits
		complete = (x & 1) == 0
	}
	return pos
}

func (fc *Funcode) Validate(isPredeclared, isUniversal func(name string) bool) error {
	if isPredeclared == nil {
		isPredeclared = func(name string) bool { return true }
	}
	if isUniversal == nil {
		isUniversal = func(name string) bool { return true }
	}
	code := fc.Code
	pc := uint32(0)
	for {
		op, arg, nextpc, opInBounds := DecodeOp(code, pc)
		if !opInBounds {
			return fmt.Errorf("program counter %v for op %s is out of bounds for code of length %v", pc, op.String(), len(code))
		}
		if int(nextpc) > len(code) {
			return fmt.Errorf("program counter %v after op %s is out of bounds for code of length %v", pc, op.String(), len(code))
		}
		if int(nextpc) == len(code) {
			return nil
		}
		if op > OpcodeMax {
			return fmt.Errorf("illegal op (%d)", op)
		}

		// See resolve/resolve.go:
		const doesnt = "this Skylark dialect does not "

		switch op {
		case ERROR, EXCEPTPUSH, EXCEPTPOP:
			if !resolve.AllowTryExcept {
				return fmt.Errorf(doesnt + "support try/except")
			}
		case AMP, PIPE, CIRCUMFLEX, TILDE, LTLT, GTGT:
			if !resolve.AllowBitwise {
				return fmt.Errorf(doesnt + "support bitwise operations")
			}
		case MAKESET:
			if !resolve.AllowSet {
				return fmt.Errorf(doesnt + "support sets")
			}
		}

		if op < OpcodeArgMin {
			pc = nextpc
			continue
		}

		switch op {
		case LOCAL, SETLOCAL:
			if int(arg) >= len(fc.Locals) {
				return fmt.Errorf("argument %v to op %s is out of bounds for locals of length %v", arg, op.String(), len(fc.Locals))
			}
		case FREE:
			if int(arg) >= len(fc.Freevars) {
				return fmt.Errorf("argument %v to op %s is out of bounds for free vars of length %v", arg, op.String(), len(fc.Freevars))
			}
		case GLOBAL, SETGLOBAL:
			if int(arg) >= len(fc.Prog.Globals) {
				return fmt.Errorf("argument %v to op %s is out of bounds for globals of length %v", arg, op.String(), len(fc.Prog.Globals))
			}
		case ATTR, SETFIELD, PREDECLARED, UNIVERSAL:
			if int(arg) >= len(fc.Prog.Names) {
				return fmt.Errorf("argument %v to op %s is out of bounds for names of length %v", arg, op.String(), len(fc.Prog.Names))
			}
			switch op {
			case PREDECLARED:
				if !isPredeclared(fc.Prog.Names[arg]) {
					return fmt.Errorf("non-predeclared argument %s to op %s", fc.Prog.Names[arg], op.String())
				}
			case UNIVERSAL:
				if !isUniversal(fc.Prog.Names[arg]) {
					return fmt.Errorf("non-universal argument %s to op %s", fc.Prog.Names[arg], op.String())
				}
			}
		case CONSTANT:
			if int(arg) >= len(fc.Prog.Constants) {
				return fmt.Errorf("argument %v to op %s is out of bounds for constants of length %v", arg, op.String(), len(fc.Prog.Constants))
			}
		case MAKEFUNC:
			if int(arg) >= len(fc.Prog.Functions) {
				return fmt.Errorf("argument %v to op %s is out of bounds for functions of length %v", arg, op.String(), len(fc.Prog.Functions))
			}
		case JMP, CJMP, ITERJMP, EXCEPTPUSH:
			if int(arg) >= len(code) {
				return fmt.Errorf("program counter target %v for op %s is out of bounds for code of length %v", arg, op.String(), len(code))
			}
		case LOAD, MAKELIST, MAKETUPLE, UNPACK:
			if op == LOAD {
				arg++
			}
			if int(arg) > fc.MaxStack {
				return fmt.Errorf("argument %v to op %s is out of bounds for max stack size of %v", arg, op.String(), fc.MaxStack)
			}
		}
		pc = nextpc
	}
	return nil
}

// idents convert syntactic identifiers to compiled form.
func idents(ids []*syntax.Ident) []Ident {
	res := make([]Ident, len(ids))
	for i, id := range ids {
		res[i].Name = id.Name
		res[i].Pos = id.NamePos
	}
	return res
}

// Expr compiles an expression to a program consisting of a single toplevel function.
func Expr(expr syntax.Expr, locals []*syntax.Ident) *Funcode {
	stmts := []syntax.Stmt{&syntax.ReturnStmt{Result: expr}}
	return File(stmts, locals, nil).Toplevel
}

// File compiles the statements of a file into a program.
func File(stmts []syntax.Stmt, locals, globals []*syntax.Ident) *Program {
	pcomp := &pcomp{
		prog: &Program{
			Globals: idents(globals),
		},
		names:     make(map[string]uint32),
		constants: make(map[interface{}]uint32),
		functions: make(map[*Funcode]uint32),
	}

	var pos syntax.Position
	if len(stmts) > 0 {
		pos = syntax.Start(stmts[0])
	}

	pcomp.prog.Toplevel = pcomp.function("<toplevel>", pos, stmts, locals, nil)

	return pcomp.prog
}

func (pcomp *pcomp) function(name string, pos syntax.Position, stmts []syntax.Stmt, locals, freevars []*syntax.Ident) *Funcode {
	fcomp := &fcomp{
		pcomp: pcomp,
		pos:   pos,
		fn: &Funcode{
			Prog:     pcomp.prog,
			Pos:      pos,
			Name:     name,
			Locals:   idents(locals),
			Freevars: idents(freevars),
		},
	}

	if debug {
		fmt.Fprintf(os.Stderr, "start function(%s @ %s)\n", name, pos)
	}

	// Convert AST to a CFG of instructions.
	entry := fcomp.newBlock()
	fcomp.block = entry
	fcomp.stmts(stmts)
	if fcomp.block != nil {
		fcomp.emit(NONE)
		fcomp.emit(RETURN)
	}

	var oops bool // something bad happened

	setinitialstack := func(b *block, depth int) {
		if b.initialstack == -1 {
			b.initialstack = depth
		} else if b.initialstack != depth {
			fmt.Fprintf(os.Stderr, "%d: setinitialstack: depth mismatch: %d vs %d\n",
				b.index, b.initialstack, depth)
			oops = true
		}
	}

	// Linearize the CFG:
	// compute order, address, and initial
	// stack depth of each reachable block.
	var pc uint32
	var blocks []*block
	var maxstack int
	var visit func(b *block)
	visit = func(b *block) {
		if b.index >= 0 {
			return // already visited
		}
		b.index = len(blocks)
		b.addr = pc
		blocks = append(blocks, b)

		stack := b.initialstack
		if debug {
			fmt.Fprintf(os.Stderr, "%s block %d: (stack = %d)\n", name, b.index, stack)
		}
		var cjmpAddr *uint32
		var exceptAddr *uint32
		var isiterjmp int
		for i, insn := range b.insns {
			pc++

			// Compute size of argument.
			if insn.op >= OpcodeArgMin {
				switch insn.op {
				case ITERJMP:
					isiterjmp = 1
					fallthrough
				case CJMP:
					cjmpAddr = &b.insns[i].arg
					pc += 4
				case EXCEPTPUSH:
					exceptAddr = &b.insns[i].arg
					pc += 4
				default:
					pc += uint32(argLen(insn.arg))
				}
			}

			// Compute effect on stack.
			se := insn.stackeffect()
			if debug {
				fmt.Fprintln(os.Stderr, "\t", insn.op, stack, stack+se)
			}
			stack += se
			if stack < 0 {
				fmt.Fprintf(os.Stderr, "After pc=%d: stack underflow\n", pc)
				oops = true
			}
			if stack+isiterjmp > maxstack {
				maxstack = stack + isiterjmp
			}
		}

		if debug {
			fmt.Fprintf(os.Stderr, "successors of block %d (start=%d):\n",
				b.addr, b.index)
			if b.jmp != nil {
				fmt.Fprintf(os.Stderr, "jmp to %d\n", b.jmp.index)
			}
			if b.cjmp != nil {
				fmt.Fprintf(os.Stderr, "cjmp to %d\n", b.cjmp.index)
			}
			if b.except != nil {
				fmt.Fprintf(os.Stderr, "except to %d\n", b.except.index)
			}
		}

		// Place the jmp block next.
		if b.jmp != nil {
			// jump threading (empty cycles are impossible)
			for b.jmp.insns == nil {
				b.jmp = b.jmp.jmp
			}

			setinitialstack(b.jmp, stack+isiterjmp)
			if b.jmp.index < 0 {
				// Successor is not yet visited:
				// place it next and fall through.
				visit(b.jmp)
			} else {
				// Successor already visited;
				// explicit backward jump required.
				pc += 5
			}
		}

		// Then the cjmp block.
		if b.cjmp != nil {
			// jump threading (empty cycles are impossible)
			for b.cjmp.insns == nil {
				b.cjmp = b.cjmp.jmp
			}

			setinitialstack(b.cjmp, stack)
			visit(b.cjmp)

			// Patch the CJMP/ITERJMP, if present.
			if cjmpAddr != nil {
				*cjmpAddr = b.cjmp.addr
			}
		}

		// Then the exception handler block:
		if b.except != nil {
			// jump threading (empty cycles are impossible)
			for b.except.insns == nil {
				b.except = b.except.jmp
			}

			setinitialstack(b.except, stack)
			visit(b.except)

			// Patch the EXCEPTPUSH, if present.
			if exceptAddr != nil {
				*exceptAddr = b.except.addr
			}
		}
	}
	setinitialstack(entry, 0)
	visit(entry)

	fn := fcomp.fn
	fn.MaxStack = maxstack

	// Emit bytecode (and position table).
	if debug {
		fmt.Fprintf(os.Stderr, "Function %s: (%d blocks, %d bytes)\n", name, len(blocks), pc)
	}
	fcomp.generate(blocks, pc)

	if debug {
		fmt.Fprintf(os.Stderr, "code=%d maxstack=%d\n", fn.Code, fn.MaxStack)
	}

	// Don't panic until we've completed printing of the function.
	if oops {
		panic("internal error")
	}

	if debug {
		fmt.Fprintf(os.Stderr, "end function(%s @ %s)\n", name, pos)
	}

	return fn
}

func (insn *insn) stackeffect() int {
	if !IsVariableStackEffect(insn.op) {
		pops, pushes := StackEffect(insn.op)
		return pushes - pops
	}
	arg := int(insn.arg)
	switch insn.op {
	case LOAD:
		return -1
	case CALL, CALL_KW, CALL_VAR, CALL_VAR_KW:
		effect := -int(2*(insn.arg&0xff) + insn.arg>>8)
		if insn.op != CALL {
			effect--
		}
		if insn.op == CALL_VAR_KW {
			effect--
		}
		return effect
	case ITERJMP:
		// Stack effect differs by successor:
		// +1 for jmp/false/ok
		//  0 for cjmp/true/exhausted
		// Handled specially in caller.
		return 0
	case MAKELIST, MAKETUPLE:
		return 1 - arg
	case UNPACK:
		return arg - 1
	default:
		panic(insn.op)
	}
}

// generate emits the linear instruction stream from the CFG,
// and builds the PC-to-line number table.
func (fcomp *fcomp) generate(blocks []*block, codelen uint32) {
	code := make([]byte, 0, codelen)
	var pcline []uint16
	var prev struct {
		pc   uint32
		line int32
	}

	for _, b := range blocks {
		if debug {
			fmt.Fprintf(os.Stderr, "%d:\n", b.index)
		}
		pc := b.addr
		for _, insn := range b.insns {
			if insn.line != 0 {
				// Instruction has a source position.  Delta-encode it.
				// See Funcode.Position for the encoding.
				for {
					var incomplete uint16

					deltapc := pc - prev.pc
					if deltapc > 0xff {
						deltapc = 0xff
						incomplete = 1
					}
					prev.pc += deltapc

					deltaline := insn.line - prev.line
					if deltaline > 0x3f {
						deltaline = 0x3f
						incomplete = 1
					} else if deltaline < -0x40 {
						deltaline = -0x40
						incomplete = 1
					}
					prev.line += deltaline

					entry := uint16(deltapc<<8) | uint16(uint8(deltaline<<1)) | incomplete
					pcline = append(pcline, entry)
					if incomplete == 0 {
						break
					}
				}

				if debug {
					fmt.Fprintf(os.Stderr, "\t\t\t\t\t; %s %d\n",
						filepath.Base(fcomp.fn.Pos.Filename()), insn.line)
				}
			}
			if debug {
				PrintOp(fcomp.fn, pc, insn.op, insn.arg)
			}
			code = append(code, byte(insn.op))
			pc++
			if insn.op >= OpcodeArgMin {
				if insn.op == CJMP || insn.op == EXCEPTPUSH || insn.op == ITERJMP {
					code = addUint32(code, insn.arg, 4) // pad arg to 4 bytes
				} else {
					code = addUint32(code, insn.arg, 0)
				}
				pc = uint32(len(code))
			}
		}

		if b.jmp != nil && b.jmp.index != b.index+1 {
			addr := b.jmp.addr
			if debug {
				fmt.Fprintf(os.Stderr, "\t%d\tjmp\t\t%d\t; block %d\n",
					pc, addr, b.jmp.index)
			}
			code = append(code, byte(JMP))
			code = addUint32(code, addr, 4)
		}
	}
	if len(code) != int(codelen) {
		panic(fmt.Sprintf("internal error: wrong code length %v; expected %v", len(code), codelen))
	}

	fcomp.fn.Pclinetab = pcline
	fcomp.fn.Code = code
}

// addUint32 encodes x as 7-bit little-endian varint.
// TODO(adonovan): opt: steal top two bits of opcode
// to encode the number of complete bytes that follow.
func addUint32(code []byte, x uint32, min int) []byte {
	end := len(code) + min
	for x >= 0x80 {
		code = append(code, byte(x)|0x80)
		x >>= 7
	}
	code = append(code, byte(x))
	// Pad the operand with NOPs to exactly min bytes.
	for len(code) < end {
		code = append(code, byte(NOP))
	}
	return code
}

func argLen(x uint32) int {
	n := 0
	for x >= 0x80 {
		n++
		x >>= 7
	}
	return n + 1
}

// PrintOp prints an instruction.
// It is provided for debugging.
func PrintOp(fn *Funcode, pc uint32, op Opcode, arg uint32) {
	if op < OpcodeArgMin {
		fmt.Fprintf(os.Stderr, "\t%d\t%s\n", pc, op)
		return
	}

	var comment string
	switch op {
	case CONSTANT:
		switch x := fn.Prog.Constants[arg].(type) {
		case string:
			comment = strconv.Quote(x)
		default:
			comment = fmt.Sprint(x)
		}
	case MAKEFUNC:
		comment = fn.Prog.Functions[arg].Name
	case SETLOCAL, LOCAL:
		comment = fn.Locals[arg].Name
	case SETGLOBAL, GLOBAL:
		comment = fn.Prog.Globals[arg].Name
	case ATTR, SETFIELD, PREDECLARED, UNIVERSAL:
		comment = fn.Prog.Names[arg]
	case FREE:
		comment = fn.Freevars[arg].Name
	case CALL, CALL_VAR, CALL_KW, CALL_VAR_KW:
		comment = fmt.Sprintf("%d pos, %d named", arg>>8, arg&0xff)
	default:
		// JMP, CJMP, ITERJMP, MAKETUPLE, MAKELIST, LOAD, UNPACK:
		// arg is just a number
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\t%d\t%-10s\t%d", pc, op, arg)
	if comment != "" {
		fmt.Fprint(&buf, "\t; ", comment)
	}
	fmt.Fprintln(&buf)
	os.Stderr.Write(buf.Bytes())
}

// newBlock returns a new block.
func (fcomp) newBlock() *block {
	return &block{index: -1, initialstack: -1}
}

// emit emits an instruction to the current block.
func (fcomp *fcomp) emit(op Opcode) {
	if op >= OpcodeArgMin {
		panic("missing arg: " + op.String())
	}
	insn := insn{op: op, line: fcomp.pos.Line}
	fcomp.block.insns = append(fcomp.block.insns, insn)
	fcomp.pos.Line = 0
}

// emit1 emits an instruction with an immediate operand.
func (fcomp *fcomp) emit1(op Opcode, arg uint32) {
	if op < OpcodeArgMin {
		panic("unwanted arg: " + op.String())
	}
	insn := insn{op: op, arg: arg, line: fcomp.pos.Line}
	fcomp.block.insns = append(fcomp.block.insns, insn)
	fcomp.pos.Line = 0
}

// jump emits a jump to the specified block.
// On return, the current block is unset.
func (fcomp *fcomp) jump(b *block) {
	if b == fcomp.block {
		panic("self-jump") // unreachable: Skylark has no arbitrary looping constructs
	}
	fcomp.block.jmp = b
	fcomp.block = nil
}

// condjump emits a conditional jump (CJMP or ITERJMP)
// to the specified true/false blocks.
// (For ITERJMP, the cases are jmp/f/ok and cjmp/t/exhausted.)
// On return, the current block is unset.
func (fcomp *fcomp) condjump(op Opcode, t, f *block) {
	if !(op == CJMP || op == ITERJMP) {
		panic("not a conditional jump: " + op.String())
	}
	fcomp.emit1(op, 0) // fill in address later
	fcomp.block.cjmp = t
	fcomp.jump(f)
}

// nameIndex returns the index of the specified name
// within the name pool, adding it if necessary.
func (pcomp *pcomp) nameIndex(name string) uint32 {
	index, ok := pcomp.names[name]
	if !ok {
		index = uint32(len(pcomp.prog.Names))
		pcomp.names[name] = index
		pcomp.prog.Names = append(pcomp.prog.Names, name)
	}
	return index
}

// constantIndex returns the index of the specified constant
// within the constant pool, adding it if necessary.
func (pcomp *pcomp) constantIndex(v interface{}) uint32 {
	index, ok := pcomp.constants[v]
	if !ok {
		index = uint32(len(pcomp.prog.Constants))
		pcomp.constants[v] = index
		pcomp.prog.Constants = append(pcomp.prog.Constants, v)
	}
	return index
}

// functionIndex returns the index of the specified function
// AST the nestedfun pool, adding it if necessary.
func (pcomp *pcomp) functionIndex(fn *Funcode) uint32 {
	index, ok := pcomp.functions[fn]
	if !ok {
		index = uint32(len(pcomp.prog.Functions))
		pcomp.functions[fn] = index
		pcomp.prog.Functions = append(pcomp.prog.Functions, fn)
	}
	return index
}

// string emits code to push the specified string.
func (fcomp *fcomp) string(s string) {
	fcomp.emit1(CONSTANT, fcomp.pcomp.constantIndex(s))
}

// setPos sets the current source position.
// It should be called prior to any operation that can fail dynamically.
// All positions are assumed to belong to the same file.
func (fcomp *fcomp) setPos(pos syntax.Position) {
	fcomp.pos = pos
}

// set emits code to store the top-of-stack value
// to the specified local or global variable.
func (fcomp *fcomp) set(id *syntax.Ident) {
	switch resolve.Scope(id.Scope) {
	case resolve.Local:
		fcomp.emit1(SETLOCAL, uint32(id.Index))
	case resolve.Global:
		fcomp.emit1(SETGLOBAL, uint32(id.Index))
	default:
		log.Fatalf("%s: set(%s): neither global nor local (%d)", id.NamePos, id.Name, id.Scope)
	}
}

// lookup emits code to push the value of the specified variable.
func (fcomp *fcomp) lookup(id *syntax.Ident) {
	switch resolve.Scope(id.Scope) {
	case resolve.Local:
		fcomp.setPos(id.NamePos)
		fcomp.emit1(LOCAL, uint32(id.Index))
	case resolve.Free:
		fcomp.emit1(FREE, uint32(id.Index))
	case resolve.Global:
		fcomp.setPos(id.NamePos)
		fcomp.emit1(GLOBAL, uint32(id.Index))
	case resolve.Predeclared:
		fcomp.setPos(id.NamePos)
		fcomp.emit1(PREDECLARED, fcomp.pcomp.nameIndex(id.Name))
	case resolve.Universal:
		fcomp.emit1(UNIVERSAL, fcomp.pcomp.nameIndex(id.Name))
	default:
		log.Fatalf("%s: compiler.lookup(%s): scope = %d", id.NamePos, id.Name, id.Scope)
	}
}

func (fcomp *fcomp) stmts(stmts []syntax.Stmt) {
	for _, stmt := range stmts {
		fcomp.stmt(stmt)
	}
}

func (fcomp *fcomp) stmt(stmt syntax.Stmt) {
	switch stmt := stmt.(type) {
	case *syntax.ExprStmt:
		if _, ok := stmt.X.(*syntax.Literal); ok {
			// Opt: don't compile doc comments only to pop them.
			return
		}
		fcomp.expr(stmt.X)
		fcomp.emit(POP)

	case *syntax.BranchStmt:
		// Resolver invariant: break/continue appear only within loops.
		switch stmt.Token {
		case syntax.PASS:
			// no-op
		case syntax.BREAK:
			innerLoop := len(fcomp.loops) - 1
			b := fcomp.loops[innerLoop].break_
			for _, enclosingLoop := range fcomp.exhandlers {
				if enclosingLoop >= innerLoop {
					fcomp.emit(EXCEPTPOP)
				}
			}
			fcomp.jump(b)
			fcomp.block = fcomp.newBlock() // dead code
		case syntax.CONTINUE:
			innerLoop := len(fcomp.loops) - 1
			b := fcomp.loops[innerLoop].continue_
			for _, enclosingLoop := range fcomp.exhandlers {
				if enclosingLoop >= innerLoop {
					fcomp.emit(EXCEPTPOP)
				}
			}
			fcomp.jump(b)
			fcomp.block = fcomp.newBlock() // dead code
		}

	case *syntax.IfStmt:
		// Keep consistent with CondExpr.
		t := fcomp.newBlock()
		f := fcomp.newBlock()
		done := fcomp.newBlock()

		fcomp.ifelse(stmt.Cond, t, f)

		fcomp.block = t
		fcomp.stmts(stmt.True)
		fcomp.jump(done)

		fcomp.block = f
		fcomp.stmts(stmt.False)
		fcomp.jump(done)

		fcomp.block = done

	case *syntax.AssignStmt:
		switch stmt.Op {
		case syntax.EQ:
			// simple assignment: x = y
			fcomp.expr(stmt.RHS)
			fcomp.assign(stmt.OpPos, stmt.LHS)

		case syntax.PLUS_EQ,
			syntax.MINUS_EQ,
			syntax.STAR_EQ,
			syntax.SLASH_EQ,
			syntax.SLASHSLASH_EQ,
			syntax.PERCENT_EQ,
			syntax.AMP_EQ,
			syntax.PIPE_EQ,
			syntax.CIRCUMFLEX_EQ,
			syntax.LTLT_EQ,
			syntax.GTGT_EQ:
			// augmented assignment: x += y

			var set func()

			// Evaluate "address" of x exactly once to avoid duplicate side-effects.
			switch lhs := stmt.LHS.(type) {
			case *syntax.Ident:
				// x = ...
				fcomp.lookup(lhs)
				set = func() {
					fcomp.set(lhs)
				}

			case *syntax.IndexExpr:
				// x[y] = ...
				fcomp.expr(lhs.X)
				fcomp.expr(lhs.Y)
				fcomp.emit(DUP2)
				fcomp.setPos(lhs.Lbrack)
				fcomp.emit(INDEX)
				set = func() {
					fcomp.setPos(lhs.Lbrack)
					fcomp.emit(SETINDEX)
				}

			case *syntax.DotExpr:
				// x.f = ...
				fcomp.expr(lhs.X)
				fcomp.emit(DUP)
				name := fcomp.pcomp.nameIndex(lhs.Name.Name)
				fcomp.setPos(lhs.Dot)
				fcomp.emit1(ATTR, name)
				set = func() {
					fcomp.setPos(lhs.Dot)
					fcomp.emit1(SETFIELD, name)
				}

			default:
				panic(lhs)
			}

			fcomp.expr(stmt.RHS)

			if stmt.Op == syntax.PLUS_EQ {
				// Allow the runtime to optimize list += iterable.
				fcomp.setPos(stmt.OpPos)
				fcomp.emit(INPLACE_ADD)
			} else {
				fcomp.binop(stmt.OpPos, stmt.Op-syntax.PLUS_EQ+syntax.PLUS)
			}
			set()
		}

	case *syntax.DefStmt:
		fcomp.function(stmt.Def, stmt.Name.Name, &stmt.Function)
		fcomp.set(stmt.Name)

	case *syntax.ForStmt:
		// Keep consistent with ForClause.
		head := fcomp.newBlock()
		body := fcomp.newBlock()
		tail := fcomp.newBlock()

		fcomp.expr(stmt.X)
		fcomp.setPos(stmt.For)
		fcomp.emit(ITERPUSH)
		fcomp.jump(head)

		fcomp.block = head
		fcomp.condjump(ITERJMP, tail, body)

		fcomp.block = body
		fcomp.assign(stmt.For, stmt.Vars)
		fcomp.loops = append(fcomp.loops, loop{break_: tail, continue_: head})
		fcomp.stmts(stmt.Body)
		fcomp.loops = fcomp.loops[:len(fcomp.loops)-1]
		fcomp.jump(head)

		fcomp.block = tail
		fcomp.emit(ITERPOP)

	case *syntax.TryStmt:
		fallback := fcomp.newBlock()
		done := fcomp.newBlock()

		if fcomp.block != nil && fcomp.block.except != nil {
			body := fcomp.newBlock()
			fcomp.jump(body)
			fcomp.block = body
		}
		fcomp.exhandlers = append(fcomp.exhandlers, len(fcomp.loops)-1)
		fcomp.block.except = fallback
		fcomp.emit1(EXCEPTPUSH, 0) // the exception-handler address is filled in later
		fcomp.stmts(stmt.Body)
		fcomp.emit(EXCEPTPOP)
		fcomp.jump(done)

		if stmt.ExceptionType != nil && stmt.ExceptionName != nil {
			fcomp.block = fallback
			fcomp.lookup(stmt.ExceptionType)
			fcomp.emit(ERROR)
			fcomp.exhandlers = fcomp.exhandlers[:len(fcomp.exhandlers)-1]
			fcomp.assign(stmt.ExceptionName.NamePos, stmt.ExceptionName)
			fcomp.stmts(stmt.Fallback)
			fcomp.emit(NONE)
			fcomp.assign(stmt.ExceptionName.NamePos, stmt.ExceptionName)
			fcomp.jump(done)
		} else {
			fcomp.block = fallback
			fcomp.emit(EXCEPTPOP)
			fcomp.exhandlers = fcomp.exhandlers[:len(fcomp.exhandlers)-1]
			fcomp.stmts(stmt.Fallback)
			fcomp.jump(done)
		}

		fcomp.block = done

	case *syntax.ReturnStmt:
		if stmt.Result != nil {
			fcomp.expr(stmt.Result)
		} else {
			fcomp.emit(NONE)
		}
		fcomp.emit(RETURN)
		fcomp.block = fcomp.newBlock() // dead code

	case *syntax.LoadStmt:
		for i := range stmt.From {
			fcomp.string(stmt.From[i].Name)
		}
		module := stmt.Module.Value.(string)
		fcomp.pcomp.prog.Loads = append(fcomp.pcomp.prog.Loads, Ident{
			Name: module,
			Pos:  stmt.Module.TokenPos,
		})
		fcomp.string(module)
		fcomp.setPos(stmt.Load)
		fcomp.emit1(LOAD, uint32(len(stmt.From)))
		for i := range stmt.To {
			fcomp.emit1(SETGLOBAL, uint32(stmt.To[len(stmt.To)-1-i].Index))
		}

	default:
		start, _ := stmt.Span()
		log.Fatalf("%s: exec: unexpected statement %T", start, stmt)
	}
}

// assign implements lhs = rhs for arbitrary expressions lhs.
// RHS is on top of stack, consumed.
func (fcomp *fcomp) assign(pos syntax.Position, lhs syntax.Expr) {
	switch lhs := lhs.(type) {
	case *syntax.ParenExpr:
		// (lhs) = rhs
		fcomp.assign(pos, lhs.X)

	case *syntax.Ident:
		// x = rhs
		fcomp.set(lhs)

	case *syntax.TupleExpr:
		// x, y = rhs
		fcomp.assignSequence(pos, lhs.List)

	case *syntax.ListExpr:
		// [x, y] = rhs
		fcomp.assignSequence(pos, lhs.List)

	case *syntax.IndexExpr:
		// x[y] = rhs
		fcomp.expr(lhs.X)
		fcomp.emit(EXCH)
		fcomp.expr(lhs.Y)
		fcomp.emit(EXCH)
		fcomp.setPos(lhs.Lbrack)
		fcomp.emit(SETINDEX)

	case *syntax.DotExpr:
		// x.f = rhs
		fcomp.expr(lhs.X)
		fcomp.emit(EXCH)
		fcomp.setPos(lhs.Dot)
		fcomp.emit1(SETFIELD, fcomp.pcomp.nameIndex(lhs.Name.Name))

	default:
		panic(lhs)
	}
}

func (fcomp *fcomp) assignSequence(pos syntax.Position, lhs []syntax.Expr) {
	fcomp.setPos(pos)
	fcomp.emit1(UNPACK, uint32(len(lhs)))
	for i := range lhs {
		fcomp.assign(pos, lhs[i])
	}
}

func (fcomp *fcomp) expr(e syntax.Expr) {
	switch e := e.(type) {
	case *syntax.ParenExpr:
		fcomp.expr(e.X)

	case *syntax.Ident:
		fcomp.lookup(e)

	case *syntax.Literal:
		// e.Value is int64, float64, *bigInt, or string.
		fcomp.emit1(CONSTANT, fcomp.pcomp.constantIndex(e.Value))

	case *syntax.ListExpr:
		for _, x := range e.List {
			fcomp.expr(x)
		}
		fcomp.emit1(MAKELIST, uint32(len(e.List)))

	case *syntax.CondExpr:
		// Keep consistent with IfStmt.
		t := fcomp.newBlock()
		f := fcomp.newBlock()
		done := fcomp.newBlock()

		fcomp.ifelse(e.Cond, t, f)

		fcomp.block = t
		fcomp.expr(e.True)
		fcomp.jump(done)

		fcomp.block = f
		fcomp.expr(e.False)
		fcomp.jump(done)

		fcomp.block = done

	case *syntax.IndexExpr:
		fcomp.expr(e.X)
		fcomp.expr(e.Y)
		fcomp.setPos(e.Lbrack)
		fcomp.emit(INDEX)

	case *syntax.SliceExpr:
		fcomp.setPos(e.Lbrack)
		fcomp.expr(e.X)
		if e.Lo != nil {
			fcomp.expr(e.Lo)
		} else {
			fcomp.emit(NONE)
		}
		if e.Hi != nil {
			fcomp.expr(e.Hi)
		} else {
			fcomp.emit(NONE)
		}
		if e.Step != nil {
			fcomp.expr(e.Step)
		} else {
			fcomp.emit(NONE)
		}
		fcomp.emit(SLICE)

	case *syntax.Comprehension:
		if e.Curly {
			if _, isDictEntry := e.Body.(*syntax.DictEntry); isDictEntry {
				fcomp.emit(MAKEDICT)
			} else {
				fcomp.emit(MAKESET)
			}
		} else {
			fcomp.emit1(MAKELIST, 0)
		}
		fcomp.comprehension(e, 0)

	case *syntax.TupleExpr:
		fcomp.tuple(e.List)

	case *syntax.DictExpr:
		fcomp.emit(MAKEDICT)
		for _, entry := range e.List {
			entry := entry.(*syntax.DictEntry)
			fcomp.emit(DUP)
			fcomp.expr(entry.Key)
			fcomp.expr(entry.Value)
			fcomp.setPos(entry.Colon)
			fcomp.emit(SETDICTUNIQ)
		}

	case *syntax.UnaryExpr:
		fcomp.expr(e.X)
		fcomp.setPos(e.OpPos)
		switch e.Op {
		case syntax.MINUS:
			fcomp.emit(UMINUS)
		case syntax.PLUS:
			fcomp.emit(UPLUS)
		case syntax.NOT:
			fcomp.emit(NOT)
		case syntax.TILDE:
			fcomp.emit(TILDE)
		default:
			log.Fatalf("%s: unexpected unary op: %s", e.OpPos, e.Op)
		}

	case *syntax.BinaryExpr:
		switch e.Op {
		// short-circuit operators
		// TODO(adonovan): use ifelse to simplify conditions.
		case syntax.OR:
			// x or y  =>  if x then x else y
			done := fcomp.newBlock()
			y := fcomp.newBlock()

			fcomp.expr(e.X)
			fcomp.emit(DUP)
			fcomp.condjump(CJMP, done, y)

			fcomp.block = y
			fcomp.emit(POP) // discard X
			fcomp.expr(e.Y)
			fcomp.jump(done)

			fcomp.block = done

		case syntax.AND:
			// x and y  =>  if x then y else x
			done := fcomp.newBlock()
			y := fcomp.newBlock()

			fcomp.expr(e.X)
			fcomp.emit(DUP)
			fcomp.condjump(CJMP, y, done)

			fcomp.block = y
			fcomp.emit(POP) // discard X
			fcomp.expr(e.Y)
			fcomp.jump(done)

			fcomp.block = done

		case syntax.PLUS:
			fcomp.plus(e)

		default:
			// all other strict binary operator (includes comparisons)
			fcomp.expr(e.X)
			fcomp.expr(e.Y)
			fcomp.binop(e.OpPos, e.Op)
		}

	case *syntax.DotExpr:
		fcomp.expr(e.X)
		fcomp.setPos(e.Dot)
		fcomp.emit1(ATTR, fcomp.pcomp.nameIndex(e.Name.Name))

	case *syntax.CallExpr:
		fcomp.call(e)

	case *syntax.LambdaExpr:
		fcomp.function(e.Lambda, "lambda", &e.Function)

	default:
		start, _ := e.Span()
		log.Fatalf("%s: unexpected expr %T", start, e)
	}
}

type summand struct {
	x       syntax.Expr
	plusPos syntax.Position
}

// plus emits optimized code for ((a+b)+...)+z that avoids naive
// quadratic behavior for strings, tuples, and lists,
// and folds together adjacent literals of the same type.
func (fcomp *fcomp) plus(e *syntax.BinaryExpr) {
	// Gather all the right operands of the left tree of plusses.
	// A tree (((a+b)+c)+d) becomes args=[a +b +c +d].
	args := make([]summand, 0, 2) // common case: 2 operands
	for plus := e; ; {
		args = append(args, summand{unparen(plus.Y), plus.OpPos})
		left := unparen(plus.X)
		x, ok := left.(*syntax.BinaryExpr)
		if !ok || x.Op != syntax.PLUS {
			args = append(args, summand{x: left})
			break
		}
		plus = x
	}
	// Reverse args to syntactic order.
	for i, n := 0, len(args)/2; i < n; i++ {
		j := len(args) - 1 - i
		args[i], args[j] = args[j], args[i]
	}

	// Fold sums of adjacent literals of the same type: ""+"", []+[], ()+().
	out := args[:0] // compact in situ
	for i := 0; i < len(args); {
		j := i + 1
		if code := addable(args[i].x); code != 0 {
			for j < len(args) && addable(args[j].x) == code {
				j++
			}
			if j > i+1 {
				args[i].x = add(code, args[i:j])
			}
		}
		out = append(out, args[i])
		i = j
	}
	args = out

	// Emit code for an n-ary sum (n > 0).
	fcomp.expr(args[0].x)
	for _, summand := range args[1:] {
		fcomp.expr(summand.x)
		fcomp.setPos(summand.plusPos)
		fcomp.emit(PLUS)
	}

	// If len(args) > 2, use of an accumulator instead of a chain of
	// PLUS operations may be more efficient.
	// However, no gain was measured on a workload analogous to Bazel loading;
	// TODO(adonovan): opt: re-evaluate on a Bazel analysis-like workload.
	//
	// We cannot use a single n-ary SUM operation
	//    a b c SUM<3>
	// because we need to report a distinct error for each
	// individual '+' operation, so three additional operations are
	// needed:
	//
	//   ACCSTART => create buffer and append to it
	//   ACCUM    => append to buffer
	//   ACCEND   => get contents of buffer
	//
	// For string, list, and tuple values, the interpreter can
	// optimize these operations by using a mutable buffer.
	// For all other types, ACCSTART and ACCEND would behave like
	// the identity function and ACCUM behaves like PLUS.
	// ACCUM must correctly support user-defined operations
	// such as list+foo.
	//
	// fcomp.emit(ACCSTART)
	// for _, summand := range args[1:] {
	// 	fcomp.expr(summand.x)
	// 	fcomp.setPos(summand.plusPos)
	// 	fcomp.emit(ACCUM)
	// }
	// fcomp.emit(ACCEND)
}

// addable reports whether e is a statically addable
// expression: a [s]tring, [l]ist, or [t]uple.
func addable(e syntax.Expr) rune {
	switch e := e.(type) {
	case *syntax.Literal:
		// TODO(adonovan): opt: support INT/FLOAT/BIGINT constant folding.
		switch e.Token {
		case syntax.STRING:
			return 's'
		}
	case *syntax.ListExpr:
		return 'l'
	case *syntax.TupleExpr:
		return 't'
	}
	return 0
}

// add returns an expression denoting the sum of args,
// which are all addable values of the type indicated by code.
// The resulting syntax is degenerate, lacking position, etc.
func add(code rune, args []summand) syntax.Expr {
	switch code {
	case 's':
		var buf bytes.Buffer
		for _, arg := range args {
			buf.WriteString(arg.x.(*syntax.Literal).Value.(string))
		}
		return &syntax.Literal{Token: syntax.STRING, Value: buf.String()}
	case 'l':
		var elems []syntax.Expr
		for _, arg := range args {
			elems = append(elems, arg.x.(*syntax.ListExpr).List...)
		}
		return &syntax.ListExpr{List: elems}
	case 't':
		var elems []syntax.Expr
		for _, arg := range args {
			elems = append(elems, arg.x.(*syntax.TupleExpr).List...)
		}
		return &syntax.TupleExpr{List: elems}
	}
	panic(code)
}

func unparen(e syntax.Expr) syntax.Expr {
	if p, ok := e.(*syntax.ParenExpr); ok {
		return unparen(p.X)
	}
	return e
}

func (fcomp *fcomp) binop(pos syntax.Position, op syntax.Token) {
	// TODO(adonovan): simplify by assuming syntax and compiler constants align.
	fcomp.setPos(pos)
	switch op {
	// arithmetic
	case syntax.PLUS:
		fcomp.emit(PLUS)
	case syntax.MINUS:
		fcomp.emit(MINUS)
	case syntax.STAR:
		fcomp.emit(STAR)
	case syntax.SLASH:
		fcomp.emit(SLASH)
	case syntax.SLASHSLASH:
		fcomp.emit(SLASHSLASH)
	case syntax.PERCENT:
		fcomp.emit(PERCENT)
	case syntax.AMP:
		fcomp.emit(AMP)
	case syntax.PIPE:
		fcomp.emit(PIPE)
	case syntax.CIRCUMFLEX:
		fcomp.emit(CIRCUMFLEX)
	case syntax.LTLT:
		fcomp.emit(LTLT)
	case syntax.GTGT:
		fcomp.emit(GTGT)
	case syntax.IN:
		fcomp.emit(IN)
	case syntax.NOT_IN:
		fcomp.emit(IN)
		fcomp.emit(NOT)

		// comparisons
	case syntax.EQL,
		syntax.NEQ,
		syntax.GT,
		syntax.LT,
		syntax.LE,
		syntax.GE:
		fcomp.emit(Opcode(op-syntax.EQL) + EQL)

	default:
		log.Fatalf("%s: unexpected binary op: %s", pos, op)
	}
}

func (fcomp *fcomp) call(call *syntax.CallExpr) {
	// TODO(adonovan): opt: Use optimized path for calling methods
	// of built-ins: x.f(...) to avoid materializing a closure.
	// if dot, ok := call.Fcomp.(*syntax.DotExpr); ok {
	// 	fcomp.expr(dot.X)
	// 	fcomp.args(call)
	// 	fcomp.emit1(CALL_ATTR, fcomp.name(dot.Name.Name))
	// 	return
	// }

	// usual case
	fcomp.expr(call.Fn)
	op, arg := fcomp.args(call)
	fcomp.setPos(call.Lparen)
	fcomp.emit1(op, arg)
}

// args emits code to push a tuple of positional arguments
// and a tuple of named arguments containing alternating keys and values.
// Either or both tuples may be empty (TODO(adonovan): optimize).
func (fcomp *fcomp) args(call *syntax.CallExpr) (op Opcode, arg uint32) {
	var callmode int
	// Compute the number of each kind of parameter.
	// TODO(adonovan): do this in resolver.
	var p, n int // number of  positional, named arguments
	var varargs, kwargs syntax.Expr
	for _, arg := range call.Args {
		if binary, ok := arg.(*syntax.BinaryExpr); ok && binary.Op == syntax.EQ {
			n++
			continue
		}
		if unary, ok := arg.(*syntax.UnaryExpr); ok {
			if unary.Op == syntax.STAR {
				callmode |= 1
				varargs = unary.X
				continue
			} else if unary.Op == syntax.STARSTAR {
				callmode |= 2
				kwargs = unary.X
				continue
			}
		}
		p++
	}

	// positional arguments
	for _, elem := range call.Args[:p] {
		fcomp.expr(elem)
	}

	// named argument pairs (name, value, ..., name, value)
	named := call.Args[p : p+n]
	for _, arg := range named {
		binary := arg.(*syntax.BinaryExpr)
		fcomp.string(binary.X.(*syntax.Ident).Name)
		fcomp.expr(binary.Y)
	}

	// *args
	if varargs != nil {
		fcomp.expr(varargs)
	}

	// **kwargs
	if kwargs != nil {
		fcomp.expr(kwargs)
	}

	// TODO(adonovan): avoid this with a more flexible encoding.
	if p >= 256 || n >= 256 {
		log.Fatalf("%s: compiler error: too many arguments in call", call.Lparen)
	}

	return CALL + Opcode(callmode), uint32(p<<8 | n)
}

func (fcomp *fcomp) tuple(elems []syntax.Expr) {
	for _, elem := range elems {
		fcomp.expr(elem)
	}
	fcomp.emit1(MAKETUPLE, uint32(len(elems)))
}

func (fcomp *fcomp) comprehension(comp *syntax.Comprehension, clauseIndex int) {
	if clauseIndex == len(comp.Clauses) {
		fcomp.emit(DUP) // accumulator
		if comp.Curly {
			// dict: {k:v for ...}
			// Parser ensures that body is of form k:v.
			// Python-style set comprehensions {body for vars in x}
			// are optionally supported.
			entry, ok := comp.Body.(*syntax.DictEntry)
			if !ok {
				fcomp.expr(comp.Body)
				fcomp.emit(APPEND)
			} else {
				fcomp.expr(entry.Key)
				fcomp.expr(entry.Value)
				fcomp.setPos(entry.Colon)
				fcomp.emit(SETDICT)
			}
		} else {
			// list: [body for vars in x]
			fcomp.expr(comp.Body)
			fcomp.emit(APPEND)
		}
		return
	}

	clause := comp.Clauses[clauseIndex]
	switch clause := clause.(type) {
	case *syntax.IfClause:
		t := fcomp.newBlock()
		done := fcomp.newBlock()
		fcomp.ifelse(clause.Cond, t, done)

		fcomp.block = t
		fcomp.comprehension(comp, clauseIndex+1)
		fcomp.jump(done)

		fcomp.block = done
		return

	case *syntax.ForClause:
		// Keep consistent with ForStmt.
		head := fcomp.newBlock()
		body := fcomp.newBlock()
		tail := fcomp.newBlock()

		fcomp.expr(clause.X)
		fcomp.setPos(clause.For)
		fcomp.emit(ITERPUSH)
		fcomp.jump(head)

		fcomp.block = head
		fcomp.condjump(ITERJMP, tail, body)

		fcomp.block = body
		fcomp.assign(clause.For, clause.Vars)
		fcomp.comprehension(comp, clauseIndex+1)
		fcomp.jump(head)

		fcomp.block = tail
		fcomp.emit(ITERPOP)
		return
	}

	start, _ := clause.Span()
	log.Fatalf("%s: unexpected comprehension clause %T", start, clause)
}

func (fcomp *fcomp) function(pos syntax.Position, name string, f *syntax.Function) {
	// Evalution of the elements of both MAKETUPLEs may fail,
	// so record the position.
	fcomp.setPos(pos)

	// Generate tuple of parameter defaults.
	n := 0
	for _, param := range f.Params {
		if binary, ok := param.(*syntax.BinaryExpr); ok {
			fcomp.expr(binary.Y)
			n++
		}
	}
	fcomp.emit1(MAKETUPLE, uint32(n))

	// Capture the values of the function's
	// free variables from the lexical environment.
	for _, freevar := range f.FreeVars {
		fcomp.lookup(freevar)
	}
	fcomp.emit1(MAKETUPLE, uint32(len(f.FreeVars)))

	funcode := fcomp.pcomp.function(name, pos, f.Body, f.Locals, f.FreeVars)

	if debug {
		// TODO(adonovan): do compilations sequentially not as a tree,
		// to make the log easier to read.
		// Simplify by identifying Toplevel and functionIndex 0.
		fmt.Fprintf(os.Stderr, "resuming %s @ %s\n", fcomp.fn.Name, fcomp.pos)
	}

	funcode.NumParams = len(f.Params)
	funcode.HasVarargs = f.HasVarargs
	funcode.HasKwargs = f.HasKwargs
	fcomp.emit1(MAKEFUNC, fcomp.pcomp.functionIndex(funcode))
}

// ifelse emits a Boolean control flow decision.
// On return, the current block is unset.
func (fcomp *fcomp) ifelse(cond syntax.Expr, t, f *block) {
	switch cond := cond.(type) {
	case *syntax.UnaryExpr:
		if cond.Op == syntax.NOT {
			// if not x then goto t else goto f
			//    =>
			// if x then goto f else goto t
			fcomp.ifelse(cond.X, f, t)
			return
		}

	case *syntax.BinaryExpr:
		switch cond.Op {
		case syntax.AND:
			// if x and y then goto t else goto f
			//    =>
			// if x then ifelse(y, t, f) else goto f
			fcomp.expr(cond.X)
			y := fcomp.newBlock()
			fcomp.condjump(CJMP, y, f)

			fcomp.block = y
			fcomp.ifelse(cond.Y, t, f)
			return

		case syntax.OR:
			// if x or y then goto t else goto f
			//    =>
			// if x then goto t else ifelse(y, t, f)
			fcomp.expr(cond.X)
			y := fcomp.newBlock()
			fcomp.condjump(CJMP, t, y)

			fcomp.block = y
			fcomp.ifelse(cond.Y, t, f)
			return
		case syntax.NOT_IN:
			// if x not in y then goto t else goto f
			//    =>
			// if x in y then goto f else goto t
			copy := *cond
			copy.Op = syntax.IN
			fcomp.expr(&copy)
			fcomp.condjump(CJMP, f, t)
			return
		}
	}

	// general case
	fcomp.expr(cond)
	fcomp.condjump(CJMP, t, f)
}
