/*
 * Copyright (c) 2013-2015 The btcsuite developers
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package wallet

import (
	"errors"
	"fmt"
	badrand "math/rand"
	"sort"
	"time"

	"github.com/conseweb/coinutil"
	"github.com/conseweb/stcd/blockchain"
	"github.com/conseweb/stcd/chaincfg"
	"github.com/conseweb/stcd/txscript"
	"github.com/conseweb/stcd/wire"
	"github.com/conseweb/stcwallet/waddrmgr"
	"github.com/conseweb/stcwallet/wtxmgr"
)

const (
	// All transactions have 4 bytes for version, 4 bytes of locktime,
	// and 2 varints for the number of inputs and outputs.
	txOverheadEstimate = 4 + 4 + 1 + 1

	// A best case signature script to redeem a P2PKH output for a
	// compressed pubkey has 70 bytes of the smallest possible DER signature
	// (with no leading 0 bytes for R and S), 33 bytes of serialized pubkey,
	// and data push opcodes for both, plus one byte for the hash type flag
	// appended to the end of the signature.
	sigScriptEstimate = 1 + 70 + 1 + 33 + 1

	// A best case tx input serialization cost is 32 bytes of sha, 4 bytes
	// of output index, 4 bytes of sequnce, and the estimated signature
	// script size.
	txInEstimate = 32 + 4 + 4 + sigScriptEstimate

	// A P2PKH pkScript contains the following bytes:
	//  - OP_DUP
	//  - OP_HASH160
	//  - OP_DATA_20 + 20 bytes of pubkey hash
	//  - OP_EQUALVERIFY
	//  - OP_CHECKSIG
	pkScriptEstimate = 1 + 1 + 1 + 20 + 1 + 1

	// A best case tx output serialization cost is 8 bytes of value, one
	// byte of varint, and the pkScript size.
	txOutEstimate = 8 + 1 + pkScriptEstimate
)

func estimateTxSize(numInputs, numOutputs int) int {
	return txOverheadEstimate + txInEstimate*numInputs + txOutEstimate*numOutputs
}

func feeForSize(incr coinutil.Amount, sz int) coinutil.Amount {
	return coinutil.Amount(1+sz/1000) * incr
}

// InsufficientFundsError represents an error where there are not enough
// funds from unspent tx outputs for a wallet to create a transaction.
// This may be caused by not enough inputs for all of the desired total
// transaction output amount, or due to
type InsufficientFundsError struct {
	in, out, fee coinutil.Amount
}

// Error satisifies the builtin error interface.
func (e InsufficientFundsError) Error() string {
	total := e.out + e.fee
	if e.fee == 0 {
		return fmt.Sprintf("insufficient funds: transaction requires "+
			"%s input but only %v spendable", total, e.in)
	}
	return fmt.Sprintf("insufficient funds: transaction requires %s input "+
		"(%v output + %v fee) but only %v spendable", total, e.out,
		e.fee, e.in)
}

// ErrUnsupportedTransactionType represents an error where a transaction
// cannot be signed as the API only supports spending P2PKH outputs.
var ErrUnsupportedTransactionType = errors.New("Only P2PKH transactions are supported")

// ErrNonPositiveAmount represents an error where a bitcoin amount is
// not positive (either negative, or zero).
var ErrNonPositiveAmount = errors.New("amount is not positive")

// ErrNegativeFee represents an error where a fee is erroneously
// negative.
var ErrNegativeFee = errors.New("fee is negative")

// defaultFeeIncrement is the default minimum transation fee (0.00001 BTC,
// measured in satoshis) added to transactions requiring a fee.
const defaultFeeIncrement = 1e3

// CreatedTx holds the state of a newly-created transaction and the change
// output (if one was added).
type CreatedTx struct {
	MsgTx       *wire.MsgTx
	ChangeAddr  coinutil.Address
	ChangeIndex int // negative if no change
}

// ByAmount defines the methods needed to satisify sort.Interface to
// sort a slice of Utxos by their amount.
type ByAmount []wtxmgr.Credit

func (u ByAmount) Len() int           { return len(u) }
func (u ByAmount) Less(i, j int) bool { return u[i].Amount < u[j].Amount }
func (u ByAmount) Swap(i, j int)      { u[i], u[j] = u[j], u[i] }

// txToPairs creates a raw transaction sending the amounts for each
// address/amount pair and fee to each address and the miner.  minconf
// specifies the minimum number of confirmations required before an
// unspent output is eligible for spending. Leftover input funds not sent
// to addr or as a fee for the miner are sent to a newly generated
// address. InsufficientFundsError is returned if there are not enough
// eligible unspent outputs to create the transaction.
func (w *Wallet) txToPairs(pairs map[string]coinutil.Amount, account uint32, minconf int32) (*CreatedTx, error) {

	// Address manager must be unlocked to compose transaction.  Grab
	// the unlock if possible (to prevent future unlocks), or return the
	// error if already locked.
	heldUnlock, err := w.HoldUnlock()
	if err != nil {
		return nil, err
	}
	defer heldUnlock.Release()

	// Get current block's height and hash.
	bs, err := w.chainSvr.BlockStamp()
	if err != nil {
		return nil, err
	}

	eligible, err := w.findEligibleOutputs(account, minconf, bs)
	if err != nil {
		return nil, err
	}

	return createTx(eligible, pairs, bs, w.FeeIncrement, w.Manager, account, w.NewChangeAddress, w.chainParams, w.DisallowFree)
}

// createTx selects inputs (from the given slice of eligible utxos)
// whose amount are sufficient to fulfil all the desired outputs plus
// the mining fee. It then creates and returns a CreatedTx containing
// the selected inputs and the given outputs, validating it (using
// validateMsgTx) as well.
func createTx(eligible []wtxmgr.Credit,
	outputs map[string]coinutil.Amount, bs *waddrmgr.BlockStamp,
	feeIncrement coinutil.Amount, mgr *waddrmgr.Manager, account uint32,
	changeAddress func(account uint32) (coinutil.Address, error),
	chainParams *chaincfg.Params, disallowFree bool) (*CreatedTx, error) {

	msgtx := wire.NewMsgTx()
	minAmount, err := addOutputs(msgtx, outputs, chainParams)
	if err != nil {
		return nil, err
	}

	// Sort eligible inputs so that we first pick the ones with highest
	// amount, thus reducing number of inputs.
	sort.Sort(sort.Reverse(ByAmount(eligible)))

	// Start by adding enough inputs to cover for the total amount of all
	// desired outputs.
	var input wtxmgr.Credit
	var inputs []wtxmgr.Credit
	totalAdded := coinutil.Amount(0)
	for totalAdded < minAmount {
		if len(eligible) == 0 {
			return nil, InsufficientFundsError{totalAdded, minAmount, 0}
		}
		input, eligible = eligible[0], eligible[1:]
		inputs = append(inputs, input)
		msgtx.AddTxIn(wire.NewTxIn(&input.OutPoint, nil))
		totalAdded += input.Amount
	}

	// Get an initial fee estimate based on the number of selected inputs
	// and added outputs, with no change.
	szEst := estimateTxSize(len(inputs), len(msgtx.TxOut))
	feeEst := minimumFee(feeIncrement, szEst, msgtx.TxOut, inputs, bs.Height, disallowFree)

	// Now make sure the sum amount of all our inputs is enough for the
	// sum amount of all outputs plus the fee. If necessary we add more,
	// inputs, but in that case we also need to recalculate the fee.
	for totalAdded < minAmount+feeEst {
		if len(eligible) == 0 {
			return nil, InsufficientFundsError{totalAdded, minAmount, feeEst}
		}
		input, eligible = eligible[0], eligible[1:]
		inputs = append(inputs, input)
		msgtx.AddTxIn(wire.NewTxIn(&input.OutPoint, nil))
		szEst += txInEstimate
		totalAdded += input.Amount
		feeEst = minimumFee(feeIncrement, szEst, msgtx.TxOut, inputs, bs.Height, disallowFree)
	}

	var changeAddr coinutil.Address
	// changeIdx is -1 unless there's a change output.
	changeIdx := -1

	for {
		change := totalAdded - minAmount - feeEst
		if change > 0 {
			if changeAddr == nil {
				changeAddr, err = changeAddress(account)
				if err != nil {
					return nil, err
				}
			}

			changeIdx, err = addChange(msgtx, change, changeAddr)
			if err != nil {
				return nil, err
			}
		}

		if err = signMsgTx(msgtx, inputs, mgr, chainParams); err != nil {
			return nil, err
		}

		if feeForSize(feeIncrement, msgtx.SerializeSize()) <= feeEst {
			// The required fee for this size is less than or equal to what
			// we guessed, so we're done.
			break
		}

		if change > 0 {
			// Remove the change output since the next iteration will add
			// it again (with a new amount) if necessary.
			tmp := msgtx.TxOut[:changeIdx]
			tmp = append(tmp, msgtx.TxOut[changeIdx+1:]...)
			msgtx.TxOut = tmp
		}

		feeEst += feeIncrement
		for totalAdded < minAmount+feeEst {
			if len(eligible) == 0 {
				return nil, InsufficientFundsError{totalAdded, minAmount, feeEst}
			}
			input, eligible = eligible[0], eligible[1:]
			inputs = append(inputs, input)
			msgtx.AddTxIn(wire.NewTxIn(&input.OutPoint, nil))
			szEst += txInEstimate
			totalAdded += input.Amount
			feeEst = minimumFee(feeIncrement, szEst, msgtx.TxOut, inputs, bs.Height, disallowFree)
		}
	}

	if err := validateMsgTx(msgtx, inputs); err != nil {
		return nil, err
	}

	info := &CreatedTx{
		MsgTx:       msgtx,
		ChangeAddr:  changeAddr,
		ChangeIndex: changeIdx,
	}
	return info, nil
}

// addChange adds a new output with the given amount and address, and
// randomizes the index (and returns it) of the newly added output.
func addChange(msgtx *wire.MsgTx, change coinutil.Amount, changeAddr coinutil.Address) (int, error) {
	pkScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		return 0, fmt.Errorf("cannot create txout script: %s", err)
	}
	msgtx.AddTxOut(wire.NewTxOut(int64(change), pkScript))

	// Randomize index of the change output.
	rng := badrand.New(badrand.NewSource(time.Now().UnixNano()))
	r := rng.Int31n(int32(len(msgtx.TxOut))) // random index
	c := len(msgtx.TxOut) - 1                // change index
	msgtx.TxOut[r], msgtx.TxOut[c] = msgtx.TxOut[c], msgtx.TxOut[r]
	return int(r), nil
}

// addOutputs adds the given address/amount pairs as outputs to msgtx,
// returning their total amount.
func addOutputs(msgtx *wire.MsgTx, pairs map[string]coinutil.Amount, chainParams *chaincfg.Params) (coinutil.Amount, error) {
	var minAmount coinutil.Amount
	for addrStr, amt := range pairs {
		if amt <= 0 {
			return minAmount, ErrNonPositiveAmount
		}
		minAmount += amt
		addr, err := coinutil.DecodeAddress(addrStr, chainParams)
		if err != nil {
			return minAmount, fmt.Errorf("cannot decode address: %s", err)
		}

		// Add output to spend amt to addr.
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return minAmount, fmt.Errorf("cannot create txout script: %s", err)
		}
		txout := wire.NewTxOut(int64(amt), pkScript)
		msgtx.AddTxOut(txout)
	}
	return minAmount, nil
}

func (w *Wallet) findEligibleOutputs(account uint32, minconf int32, bs *waddrmgr.BlockStamp) ([]wtxmgr.Credit, error) {
	unspent, err := w.TxStore.UnspentOutputs()
	if err != nil {
		return nil, err
	}

	// TODO: Eventually all of these filters (except perhaps output locking)
	// should be handled by the call to UnspentOutputs (or similar).
	// Because one of these filters requires matching the output script to
	// the desired account, this change depends on making wtxmgr a waddrmgr
	// dependancy and requesting unspent outputs for a single account.
	eligible := make([]wtxmgr.Credit, 0, len(unspent))
	for i := range unspent {
		output := &unspent[i]

		// Only include this output if it meets the required number of
		// confirmations.  Coinbase transactions must have have reached
		// maturity before their outputs may be spent.
		if !confirmed(minconf, output.Height, bs.Height) {
			continue
		}
		if output.FromCoinBase {
			const target = blockchain.CoinbaseMaturity
			if !confirmed(target, output.Height, bs.Height) {
				continue
			}
		}

		// Locked unspent outputs are skipped.
		if w.LockedOutpoint(output.OutPoint) {
			continue
		}

		// Filter out unspendable outputs, that is, remove those that
		// (at this time) are not P2PKH outputs.  Other inputs must be
		// manually included in transactions and sent (for example,
		// using createrawtransaction, signrawtransaction, and
		// sendrawtransaction).
		class, addrs, _, err := txscript.ExtractPkScriptAddrs(
			output.PkScript, w.chainParams)
		if err != nil || class != txscript.PubKeyHashTy {
			continue
		}

		// Only include the output if it is associated with the passed
		// account.  There should only be one address since this is a
		// P2PKH script.
		addrAcct, err := w.Manager.AddrAccount(addrs[0])
		if err != nil || addrAcct != account {
			continue
		}

		eligible = append(eligible, *output)
	}
	return eligible, nil
}

// signMsgTx sets the SignatureScript for every item in msgtx.TxIn.
// It must be called every time a msgtx is changed.
// Only P2PKH outputs are supported at this point.
func signMsgTx(msgtx *wire.MsgTx, prevOutputs []wtxmgr.Credit, mgr *waddrmgr.Manager, chainParams *chaincfg.Params) error {
	if len(prevOutputs) != len(msgtx.TxIn) {
		return fmt.Errorf(
			"Number of prevOutputs (%d) does not match number of tx inputs (%d)",
			len(prevOutputs), len(msgtx.TxIn))
	}
	for i, output := range prevOutputs {
		// Errors don't matter here, as we only consider the
		// case where len(addrs) == 1.
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(output.PkScript,
			chainParams)
		if len(addrs) != 1 {
			continue
		}
		apkh, ok := addrs[0].(*coinutil.AddressPubKeyHash)
		if !ok {
			return ErrUnsupportedTransactionType
		}

		ai, err := mgr.Address(apkh)
		if err != nil {
			return fmt.Errorf("cannot get address info: %v", err)
		}

		pka := ai.(waddrmgr.ManagedPubKeyAddress)
		privkey, err := pka.PrivKey()
		if err != nil {
			return fmt.Errorf("cannot get private key: %v", err)
		}

		sigscript, err := txscript.SignatureScript(msgtx, i,
			output.PkScript, txscript.SigHashAll, privkey,
			ai.Compressed())
		if err != nil {
			return fmt.Errorf("cannot create sigscript: %s", err)
		}
		msgtx.TxIn[i].SignatureScript = sigscript
	}

	return nil
}

func validateMsgTx(msgtx *wire.MsgTx, prevOutputs []wtxmgr.Credit) error {
	for i := range msgtx.TxIn {
		vm, err := txscript.NewEngine(prevOutputs[i].PkScript,
			msgtx, i, txscript.StandardVerifyFlags, nil)
		if err != nil {
			return fmt.Errorf("cannot create script engine: %s", err)
		}
		if err = vm.Execute(); err != nil {
			return fmt.Errorf("cannot validate transaction: %s", err)
		}
	}
	return nil
}

// minimumFee estimates the minimum fee required for a transaction.
// If cfg.DisallowFree is false, a fee may be zero so long as txLen
// s less than 1 kilobyte and none of the outputs contain a value
// less than 1 bitcent. Otherwise, the fee will be calculated using
// incr, incrementing the fee for each kilobyte of transaction.
func minimumFee(incr coinutil.Amount, txLen int, outputs []*wire.TxOut, prevOutputs []wtxmgr.Credit, height int32, disallowFree bool) coinutil.Amount {
	allowFree := false
	if !disallowFree {
		allowFree = allowNoFeeTx(height, prevOutputs, txLen)
	}
	fee := feeForSize(incr, txLen)

	if allowFree && txLen < 1000 {
		fee = 0
	}

	if fee < incr {
		for _, txOut := range outputs {
			if txOut.Value < coinutil.SatoshiPerBitcent {
				return incr
			}
		}
	}

	// How can fee be smaller than 0 here?
	if fee < 0 || fee > coinutil.MaxSatoshi {
		fee = coinutil.MaxSatoshi
	}

	return fee
}

// allowNoFeeTx calculates the transaction priority and checks that the
// priority reaches a certain threshold.  If the threshhold is
// reached, a free transaction fee is allowed.
func allowNoFeeTx(curHeight int32, txouts []wtxmgr.Credit, txSize int) bool {
	const blocksPerDayEstimate = 144.0
	const txSizeEstimate = 250.0
	const threshold = coinutil.SatoshiPerBitcoin * blocksPerDayEstimate / txSizeEstimate

	var weightedSum int64
	for _, txout := range txouts {
		depth := chainDepth(txout.Height, curHeight)
		weightedSum += int64(txout.Amount) * int64(depth)
	}
	priority := float64(weightedSum) / float64(txSize)
	return priority > threshold
}

// chainDepth returns the chaindepth of a target given the current
// blockchain height.
func chainDepth(target, current int32) int32 {
	if target == -1 {
		// target is not yet in a block.
		return 0
	}

	// target is in a block.
	return current - target + 1
}
