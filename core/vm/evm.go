// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

type (
	CanTransferFunc func(StateDB, common.Address, *big.Int) bool
	TransferFunc    func(StateDB, common.Address, common.Address, *big.Int)
	// GetHashFunc returns the nth block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

// run runs the given contract and takes care of running precompiles with a fallback to the byte code interpreter.
func run(evm *EVM, snapshot int, contract *Contract, input []byte) ([]byte, error) {
	if contract.CodeAddr != nil {
		precompiledContracts := PrecompiledContracts
		if p := precompiledContracts[*contract.CodeAddr]; p != nil {
			return RunPrecompiledContract(p, input, contract)
		}
	}

	return evm.interpreter.Run(snapshot, contract, input)
}

// Context provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
type Context struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc

	// Message information
	Origin   common.Address // Provides information for ORIGIN
	GasPrice *big.Int       // Provides information for GASPRICE

	// Block information
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    *big.Int       // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        *big.Int       // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
}

// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context
	// StateDB gives access to the underlying state
	StateDB StateDB
	// Depth is the current call stack
	depth int

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig
	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules
	// virtual machine configuration options used to initialise the
	// evm.
	vmConfig Config
	// global (to this context) ethereum virtual machine
	// used throughout the execution of the tx.
	interpreter *Interpreter
	// abort is used to abort the EVM calling operations
	// NOTE: must be set atomically
	abort int32

	// Quorum additions:
	privateState      StateDB
	states            [1027]*state.StateDB
	currentStateDepth uint
	readOnly          bool
	readOnlyDepth     uint
}

// NewEVM returns a new EVM environment. The returned EVM is not thread safe
// and should only ever be used *once*.
func NewEVM(ctx Context, statedb, privateState StateDB, chainConfig *params.ChainConfig, vmConfig Config) *EVM {
	evm := &EVM{
		Context:     ctx,
		StateDB:     statedb,
		vmConfig:    vmConfig,
		chainConfig: chainConfig,
		chainRules:  chainConfig.Rules(ctx.BlockNumber),

		privateState: privateState,
	}

	evm.Push(privateState)

	evm.interpreter = NewInterpreter(evm, vmConfig)
	return evm
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
func (evm *EVM) Cancel() {
	atomic.StoreInt32(&evm.abort, 1)
}

// Call executes the contract associated with the addr with the given input as parameters. It also handles any
// necessary value transfer required and takes the necessary steps to create accounts and reverses the state in
// case of an execution error or failed value transfer.
func (evm *EVM) Call(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	println("in EVM.Call")
	evm.Push(getDualState(evm, addr))
	defer func() { evm.Pop() }()

	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance
	}

	var createAccount bool
	if addr == (common.Address{}) {
		println("in EVM.Call: createAddressAndIncrementNonce")
		addr = createAddressAndIncrementNonce(evm, caller)
		createAccount = true
	}

	var (
		to       = AccountRef(addr)
		snapshot = evm.StateDB.Snapshot()
	)
	if createAccount {
		evm.StateDB.CreateAccount(addr)
	} else {
		if !evm.StateDB.Exist(addr) {
			if PrecompiledContracts[addr] == nil && evm.ChainConfig().IsEIP158(evm.BlockNumber) && value.Sign() == 0 {
				return nil, gas, nil
			}

			evm.StateDB.CreateAccount(addr)
		}
	}
	evm.Transfer(evm.StateDB, caller.Address(), to.Address(), value)

	// initialise a new contract and set the code that is to be used by the
	// E The contract is a scoped evmironment for this execution context
	// only.
	contract := NewContract(caller, to, value, gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, snapshot, contract, input)
	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		contract.UseGas(contract.Gas)
		evm.StateDB.RevertToSnapshot(snapshot)
	}
	return ret, contract.Gas, err
}

// CallCode executes the contract associated with the addr with the given input as parameters. It also handles any
// necessary value transfer required and takes the necessary steps to create accounts and reverses the state in
// case of an execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address' code with the caller as context.
func (evm *EVM) CallCode(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	evm.Push(getDualState(evm, addr))
	defer func() { evm.Pop() }()

	// TODO(joel) do we need to do the createAccount / createAccountAndIncrementNonce dance from the old exec()?

	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	if !evm.CanTransfer(caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance
	}

	var (
		snapshot = evm.StateDB.Snapshot()
		to       = AccountRef(caller.Address())
	)
	// initialise a new contract and set the code that is to be used by the
	// E The contract is a scoped evmironment for this execution context
	// only.
	contract := NewContract(caller, to, value, gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, snapshot, contract, input)
	if err != nil {
		contract.UseGas(contract.Gas)
		evm.StateDB.RevertToSnapshot(snapshot)
	}

	return ret, contract.Gas, err
}

// DelegateCall executes the contract associated with the addr with the given input as parameters.
// It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address' code with the caller as context
// and the caller is set to the caller of the caller.
func (evm *EVM) DelegateCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	evm.Push(getDualState(evm, addr))
	defer func() { evm.Pop() }()

	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}

	var (
		snapshot = evm.StateDB.Snapshot()
		to       = AccountRef(caller.Address())
	)

	// Iinitialise a new contract and make initialise the delegate values
	contract := NewContract(caller, to, nil, gas).AsDelegate()
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, snapshot, contract, input)
	if err != nil {
		contract.UseGas(contract.Gas)
		evm.StateDB.RevertToSnapshot(snapshot)
	}

	return ret, contract.Gas, err
}

// Create creates a new contract using code as deployment code.
func (evm *EVM) Create(caller ContractRef, code []byte, gas uint64, value *big.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, common.Address{}, gas, nil
	}

	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, ErrDepth
	}
	if !evm.CanTransfer(caller.Address(), value) {
		return nil, common.Address{}, gas, ErrInsufficientBalance
	}

	contractAddr = createAddressAndIncrementNonce(evm, caller)

	snapshot := evm.StateDB.Snapshot()
	evm.StateDB.CreateAccount(contractAddr)
	if evm.ChainConfig().IsEIP158(evm.BlockNumber) {
		evm.StateDB.SetNonce(contractAddr, 1)
	}
	evm.Transfer(evm.StateDB, caller.Address(), contractAddr, value)

	// initialise a new contract and set the code that is to be used by the
	// E The contract is a scoped evmironment for this execution context
	// only.
	contract := NewContract(caller, AccountRef(contractAddr), value, gas)
	contract.SetCallCode(&contractAddr, crypto.Keccak256Hash(code), code)

	ret, err = run(evm, snapshot, contract, nil)
	// check whether the max code size has been exceeded
	maxCodeSizeExceeded := len(ret) > params.MaxCodeSize
	// if the contract creation ran successfully and no errors were returned
	// calculate the gas required to store the code. If the code could not
	// be stored due to not enough gas set an error and let it be handled
	// by the error checking condition below.
	if err == nil && !maxCodeSizeExceeded {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if contract.UseGas(createDataGas) {
			evm.StateDB.SetCode(contractAddr, ret)
		} else {
			err = ErrCodeStoreOutOfGas
		}
	}

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if maxCodeSizeExceeded ||
		(err != nil && (evm.ChainConfig().IsHomestead(evm.BlockNumber) || err != ErrCodeStoreOutOfGas)) {
		contract.UseGas(contract.Gas)
		evm.StateDB.RevertToSnapshot(snapshot)
	}
	// If the vm returned with an error the return value should be set to nil.
	// This isn't consensus critical but merely to for behaviour reasons such as
	// tests, RPC calls, etc.
	if err != nil {
		ret = nil
	}

	return ret, contractAddr, contract.Gas, err
}

// ChainConfig returns the evmironment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }

// Interpreter returns the EVM interpreter
func (evm *EVM) Interpreter() *Interpreter { return evm.interpreter }

// TODO(joel): just switch to EVM?
type DualStateEnv interface {
	PublicState() StateDB
	PrivateState() StateDB

	Push(StateDB)
	Pop()
}

/*
func stateSwitch(env vm.Environment, addr common.Address) {
	if env, ok := env.(DualStateEnv); ok {
		var state *state.StateDB
		if env.PrivateState().Exist(addr) {
			state = env.PrivateState()
		} else if env.PublicState().Exist(addr) {
			state = env.PublicState()
		}
		env.Push(state)
		defer func() { env.Pop() }()
	}
}
*/

func getDualState(env DualStateEnv, addr common.Address) StateDB {
	// priv: (a) -> (b)  (private)
	// pub:   a  -> [b]  (private -> public)
	// priv: (a) ->  b   (public)
	var state StateDB
	println("public state:", addr.String(), env.PublicState(), env.PublicState().GetCode(addr))
	println("private state:", addr.String(), env.PrivateState(), env.PrivateState().GetCode(addr))
	if env.PrivateState().Exist(addr) {
		println("getDualState: private")
		state = env.PrivateState()
	} else if env.PublicState().Exist(addr) {
		println("getDualState: public")
		state = env.PublicState()
	}

	return state
}

// createAddressAndIncrementNonce returns an address based on the caller address and nonce.
//
// It also gets the right state in case of a dual state environment. If a sender
// is a transaction (depth == 0) use the public state to derive the address
// and increment the nonce of the public state. If the sender is a contract
// (depth > 0) use the private state to derive the nonce and increment the
// nonce on the private state only.
//
// If the transaction went to a public contract the private and public state
// are the same.
func createAddressAndIncrementNonce(env *EVM, caller ContractRef) common.Address {
	db := env.Db()
	// check for a dual state in case of quorum.
	if env.Depth() > 0 {
		db = env.PrivateState()
	} else {
		db = env.PublicState()
	}
	// Increment the callers nonce on the state based on the current depth
	nonce := db.GetNonce(caller.Address())
	db.SetNonce(caller.Address(), nonce+1)

	return crypto.CreateAddress(caller.Address(), nonce)
}

func (env *EVM) PublicState() StateDB  { return env.StateDB }
func (env *EVM) PrivateState() StateDB { return env.privateState }
func (env *EVM) Push(statedb StateDB) {
	if env.privateState != statedb {
		env.readOnly = true
		env.readOnlyDepth = env.currentStateDepth
	}

	if castedStateDb, ok := statedb.(*state.StateDB); ok {
		env.states[env.currentStateDepth] = castedStateDb
		env.currentStateDepth++
	}
}
func (env *EVM) Pop() {
	env.currentStateDepth--
	if env.readOnly && env.currentStateDepth == env.readOnlyDepth {
		env.readOnly = false
	}
}
func (env *EVM) currentState() *state.StateDB { return env.states[env.currentStateDepth-1] }
func (env *EVM) Db() StateDB                  { return env.currentState() }
func (env *EVM) Depth() int                   { return env.depth }
func (env *EVM) SetDepth(i int)               { env.depth = i }

func (self *EVM) AddLog(log *types.Log) {
	self.currentState().AddLog(log)
}
func (self *EVM) CanTransfer(from common.Address, balance *big.Int) bool {
	return self.currentState().GetBalance(from).Cmp(balance) >= 0
}

//func (self *EVM) SnapshotDatabase() int {
//	return self.currentState().Snapshot()
//}

// We only need to revert the current state because when we call from private
// public state it's read only, there wouldn't be anything to reset.
// (A)->(B)->C->(B): A failure in (B) wouldn't need to reset C, as C was flagged
// read only.
func (self *EVM) RevertToSnapshot(snapshot int) {
	self.currentState().RevertToSnapshot(snapshot)
}
