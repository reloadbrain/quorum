package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
)

var dualStateTestHeader = types.Header{
	Number:     new(big.Int),
	Time:       new(big.Int).SetUint64(43),
	Difficulty: new(big.Int).SetUint64(1000488),
	GasLimit:   new(big.Int).SetUint64(4700000),
}

func TestDualStatePrivateToPublicCall(t *testing.T) {
	callAddr := common.Address{1}

	db, _ := ethdb.NewMemDatabase()
	publicState, _ := state.New(common.Hash{}, db)
	publicState.SetCode(common.Address{2}, common.Hex2Bytes("600a6000526001601ff300"))

	privateState, _ := state.New(common.Hash{}, db)
	privateState.SetCode(callAddr, common.Hex2Bytes("60016000600060006000730200000000000000000000000000000000000000620186a0f160005160005500"))

	author := common.Address{}
	msg := callmsg{
		addr:     author,
		to:       &callAddr,
		value:    big.NewInt(1),
		gas:      big.NewInt(1000000),
		gasPrice: new(big.Int),
		data:     nil,
	}

	ctx := NewEVMContext(msg, &dualStateTestHeader, nil, &author)
	env := vm.NewEVM(ctx, publicState, privateState, &params.ChainConfig{}, vm.Config{})
	env.Call(vm.AccountRef(author), callAddr, msg.data, msg.gas.Uint64(), new(big.Int))

	if value := privateState.GetState(callAddr, common.Hash{}); value != (common.Hash{10}) {
		t.Errorf("expected 10 got %x", value)
	}
}

func TestDualStatePublicToPrivateCall(t *testing.T) {
	callAddr := common.Address{1}

	db, _ := ethdb.NewMemDatabase()
	privateState, _ := state.New(common.Hash{}, db)
	privateState.SetCode(common.Address{2}, common.Hex2Bytes("600a6000526001601ff300"))

	publicState, _ := state.New(common.Hash{}, db)
	publicState.SetCode(callAddr, common.Hex2Bytes("60016000600060006000730200000000000000000000000000000000000000620186a0f160005160005500"))

	author := common.Address{}
	msg := callmsg{
		addr:     author,
		to:       &callAddr,
		value:    big.NewInt(1),
		gas:      big.NewInt(1000000),
		gasPrice: new(big.Int),
		data:     nil,
	}

	ctx := NewEVMContext(msg, &dualStateTestHeader, nil, &author)
	env := vm.NewEVM(ctx, publicState, publicState, &params.ChainConfig{}, vm.Config{})
	env.Call(vm.AccountRef(author), callAddr, msg.data, msg.gas.Uint64(), new(big.Int))

	if value := publicState.GetState(callAddr, common.Hash{}); value != (common.Hash{}) {
		t.Errorf("expected 0 got %x", value)
	}
}

func TestDualStateReadOnly(t *testing.T) {
	callAddr := common.Address{1}

	db, _ := ethdb.NewMemDatabase()
	publicState, _ := state.New(common.Hash{}, db)
	publicState.SetCode(common.Address{2}, common.Hex2Bytes("600a60005500"))

	privateState, _ := state.New(common.Hash{}, db)
	privateState.SetCode(callAddr, common.Hex2Bytes("60016000600060006000730200000000000000000000000000000000000000620186a0f160005160005500"))

	author := common.Address{}
	msg := callmsg{
		addr:     author,
		to:       &callAddr,
		value:    big.NewInt(1),
		gas:      big.NewInt(1000000),
		gasPrice: new(big.Int),
		data:     nil,
	}

	ctx := NewEVMContext(msg, &dualStateTestHeader, nil, &author)
	env := vm.NewEVM(ctx, publicState, privateState, &params.ChainConfig{}, vm.Config{})
	env.Call(vm.AccountRef(author), callAddr, msg.data, msg.gas.Uint64(), new(big.Int))

	if value := publicState.GetState(common.Address{2}, common.Hash{}); value != (common.Hash{0}) {
		t.Errorf("expected 0 got %x", value)
	}
}
