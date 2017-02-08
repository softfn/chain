package vm

import (
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	// TODO(bobg): very little of this package depends on bc, consider trying to remove the dependency
	"chain/errors"
	"chain/protocol/bc"
	"chain/protocol/tx"
)

const initialRunLimit = 10000

type virtualMachine struct {
	program      []byte // the program currently executing
	mainprog     []byte // the outermost program, returned by OP_PROGRAM
	pc, nextPC   uint32
	runLimit     int64
	deferredCost int64

	expansionReserved bool

	// Stores the data parsed out of an opcode. Used as input to
	// data-pushing opcodes.
	data []byte

	// CHECKPREDICATE spawns a child vm with depth+1
	depth int

	// In each of these stacks, stack[len(stack)-1] is the top element.
	dataStack [][]byte
	altStack  [][]byte

	tx    *bc.Tx
	input tx.EntryRef // input.Entry must be non-nil

	block *bc.Block
}

// ErrFalseVMResult is one of the ways for a transaction to fail validation
var ErrFalseVMResult = errors.New("false VM result")

// TraceOut - if non-nil - will receive trace output during
// execution.
var TraceOut io.Writer

func VerifyTxInput(tx *bc.Tx, inputIndex uint32) (err error) {
	defer func() {
		if panErr := recover(); panErr != nil {
			err = ErrUnexpected
		}
	}()
	return verifyTxInput(tx, inputIndex)
}

func verifyTxInput(t *bc.Tx, input tx.EntryRef) error {
	expansionReserved := t.Version == 1

	inputID, err := input.Hash()
	if err != nil {
		return err
	}

	f := func(vmversion uint64, prog []byte, args [][]byte) error {
		if vmversion != 1 {
			return ErrUnsupportedVM
		}

		vm := virtualMachine{
			tx:    t,
			input: input,

			expansionReserved: expansionReserved,

			mainprog: prog,
			program:  prog,
			runLimit: initialRunLimit,
		}
		for _, arg := range args {
			err := vm.push(arg, false)
			if err != nil {
				return err
			}
		}
		err := vm.run()
		if err == nil && vm.falseResult() {
			err = ErrFalseVMResult
		}
		return wrapErr(err, &vm, args)
	}

	switch e := input.Entry.(type) {
	case *tx.Issuance:
		return f(e.IssuanceProgram(), e.Arguments())

	case *tx.Spend:
		oEntry := e.SpentOutput().Entry
		if oEntry == nil {
			// xxx error
		}
		o, ok := oEntry.(*tx.Output)
		if !ok {
			// xxx error
		}
		return f(o.ControlProgram(), e.Arguments())
	}

	return errors.WithDetailf(ErrUnsupportedTx, "transaction input has unknown type %T", input.Entry)
}

func VerifyBlockHeader(prev *bc.BlockHeader, block *bc.Block) (err error) {
	defer func() {
		if panErr := recover(); panErr != nil {
			err = ErrUnexpected
		}
	}()
	return verifyBlockHeader(prev, block)
}

func verifyBlockHeader(prev *bc.BlockHeader, block *bc.Block) error {
	vm := virtualMachine{
		block: block,

		expansionReserved: true,

		mainprog: prev.ConsensusProgram,
		program:  prev.ConsensusProgram,
		runLimit: initialRunLimit,
	}

	for _, arg := range block.Witness {
		err := vm.push(arg, false)
		if err != nil {
			return err
		}
	}

	err := vm.run()
	if err == nil && vm.falseResult() {
		err = ErrFalseVMResult
	}
	return wrapErr(err, &vm, block.Witness)
}

// falseResult returns true iff the stack is empty or the top
// item is false
func (vm *virtualMachine) falseResult() bool {
	return len(vm.dataStack) == 0 || !AsBool(vm.dataStack[len(vm.dataStack)-1])
}

func (vm *virtualMachine) run() error {
	for vm.pc = 0; vm.pc < uint32(len(vm.program)); { // handle vm.pc updates in step
		err := vm.step()
		if err != nil {
			return err
		}
	}
	return nil
}

func (vm *virtualMachine) step() error {
	inst, err := ParseOp(vm.program, vm.pc)
	if err != nil {
		return err
	}

	vm.nextPC = vm.pc + inst.Len

	if TraceOut != nil {
		opname := inst.Op.String()
		fmt.Fprintf(TraceOut, "vm %d pc %d limit %d %s", vm.depth, vm.pc, vm.runLimit, opname)
		if len(inst.Data) > 0 {
			fmt.Fprintf(TraceOut, " %x", inst.Data)
		}
		fmt.Fprint(TraceOut, "\n")
	}

	if isExpansion[inst.Op] {
		if vm.expansionReserved {
			return ErrDisallowedOpcode
		}
		vm.pc = vm.nextPC
		return vm.applyCost(1)
	}

	vm.deferredCost = 0
	vm.data = inst.Data
	err = ops[inst.Op].fn(vm)
	if err != nil {
		return err
	}
	err = vm.applyCost(vm.deferredCost)
	if err != nil {
		return err
	}
	vm.pc = vm.nextPC

	if TraceOut != nil {
		for i := len(vm.dataStack) - 1; i >= 0; i-- {
			fmt.Fprintf(TraceOut, "  stack %d: %x\n", len(vm.dataStack)-1-i, vm.dataStack[i])
		}
	}

	return nil
}

func (vm *virtualMachine) push(data []byte, deferred bool) error {
	cost := 8 + int64(len(data))
	if deferred {
		vm.deferCost(cost)
	} else {
		err := vm.applyCost(cost)
		if err != nil {
			return err
		}
	}
	vm.dataStack = append(vm.dataStack, data)
	return nil
}

func (vm *virtualMachine) pushBool(b bool, deferred bool) error {
	return vm.push(BoolBytes(b), deferred)
}

func (vm *virtualMachine) pushInt64(n int64, deferred bool) error {
	return vm.push(Int64Bytes(n), deferred)
}

func (vm *virtualMachine) pop(deferred bool) ([]byte, error) {
	if len(vm.dataStack) == 0 {
		return nil, ErrDataStackUnderflow
	}
	res := vm.dataStack[len(vm.dataStack)-1]
	vm.dataStack = vm.dataStack[:len(vm.dataStack)-1]

	cost := 8 + int64(len(res))
	if deferred {
		vm.deferCost(-cost)
	} else {
		vm.runLimit += cost
	}

	return res, nil
}

func (vm *virtualMachine) popInt64(deferred bool) (int64, error) {
	bytes, err := vm.pop(deferred)
	if err != nil {
		return 0, err
	}
	n, err := AsInt64(bytes)
	return n, err
}

func (vm *virtualMachine) top() ([]byte, error) {
	if len(vm.dataStack) == 0 {
		return nil, ErrDataStackUnderflow
	}
	return vm.dataStack[len(vm.dataStack)-1], nil
}

// positive cost decreases runlimit, negative cost increases it
func (vm *virtualMachine) applyCost(n int64) error {
	if n > vm.runLimit {
		return ErrRunLimitExceeded
	}
	vm.runLimit -= n
	return nil
}

func (vm *virtualMachine) deferCost(n int64) {
	vm.deferredCost += n
}

func stackCost(stack [][]byte) int64 {
	result := int64(8 * len(stack))
	for _, item := range stack {
		result += int64(len(item))
	}
	return result
}

type Error struct {
	Err  error
	Prog []byte
	Args [][]byte
}

func (e Error) Error() string {
	dis, err := Disassemble(e.Prog)
	if err != nil {
		dis = "???"
	}

	args := make([]string, 0, len(e.Args))
	for _, a := range e.Args {
		args = append(args, hex.EncodeToString(a))
	}

	return fmt.Sprintf("%s [prog %x = %s; args %s]", e.Err.Error(), e.Prog, dis, strings.Join(args, " "))
}

func wrapErr(err error, vm *virtualMachine, args [][]byte) error {
	if err == nil {
		return nil
	}
	return Error{
		Err:  err,
		Prog: vm.program,
		Args: args,
	}
}
