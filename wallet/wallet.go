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
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/conseweb/coinutil"
	"github.com/conseweb/stcd/blockchain"
	"github.com/conseweb/stcd/btcjson"
	"github.com/conseweb/stcd/chaincfg"
	"github.com/conseweb/stcd/txscript"
	"github.com/conseweb/stcd/wire"
	"github.com/conseweb/stcwallet/chain"
	"github.com/conseweb/stcwallet/waddrmgr"
	"github.com/conseweb/stcwallet/walletdb"
	"github.com/conseweb/stcwallet/wtxmgr"
)

const (
	walletDbWatchingOnlyName = "wowallet.db"
)

// ErrNotSynced describes an error where an operation cannot complete
// due wallet being out of sync (and perhaps currently syncing with)
// the remote chain server.
var ErrNotSynced = errors.New("wallet is not synchronized with the chain server")

// Namespace bucket keys.
var (
	waddrmgrNamespaceKey = []byte("waddrmgr")
	wtxmgrNamespaceKey   = []byte("wtxmgr")
)

// Wallet is a structure containing all the components for a
// complete wallet.  It contains the Armory-style key store
// addresses and keys),
type Wallet struct {
	// Data stores
	db      walletdb.DB
	Manager *waddrmgr.Manager
	TxStore *wtxmgr.Store

	chainSvr        *chain.Client
	chainSvrLock    sync.Mutex
	chainSvrSynced  bool
	chainSvrSyncMtx sync.Mutex

	lockedOutpoints map[wire.OutPoint]struct{}
	FeeIncrement    coinutil.Amount
	DisallowFree    bool

	// Channels for rescan processing.  Requests are added and merged with
	// any waiting requests, before being sent to another goroutine to
	// call the rescan RPC.
	rescanAddJob        chan *RescanJob
	rescanBatch         chan *rescanBatch
	rescanNotifications chan interface{} // From chain server
	rescanProgress      chan *RescanProgressMsg
	rescanFinished      chan *RescanFinishedMsg

	// Channel for transaction creation requests.
	createTxRequests chan createTxRequest

	// Channels for the manager locker.
	unlockRequests     chan unlockRequest
	lockRequests       chan struct{}
	holdUnlockRequests chan chan HeldUnlock
	lockState          chan bool
	changePassphrase   chan changePassphraseRequest

	// Notification channels so other components can listen in on wallet
	// activity.  These are initialized as nil, and must be created by
	// calling one of the Listen* methods.
	connectedBlocks    chan wtxmgr.BlockMeta
	disconnectedBlocks chan wtxmgr.BlockMeta
	relevantTxs        chan chain.RelevantTx
	lockStateChanges   chan bool // true when locked
	confirmedBalance   chan coinutil.Amount
	unconfirmedBalance chan coinutil.Amount
	notificationMu     sync.Mutex

	chainParams *chaincfg.Params
	wg          sync.WaitGroup

	started bool
	quit    chan struct{}
	quitMu  sync.Mutex
}

// ErrDuplicateListen is returned for any attempts to listen for the same
// notification more than once.  If callers must pass along a notifiation to
// multiple places, they must broadcast it themself.
var ErrDuplicateListen = errors.New("duplicate listen")

// ListenConnectedBlocks returns a channel that passes all blocks that a wallet
// has been marked in sync with. The channel must be read, or other wallet
// methods will block.
//
// If this is called twice, ErrDuplicateListen is returned.
func (w *Wallet) ListenConnectedBlocks() (<-chan wtxmgr.BlockMeta, error) {
	defer w.notificationMu.Unlock()
	w.notificationMu.Lock()

	if w.connectedBlocks != nil {
		return nil, ErrDuplicateListen
	}
	w.connectedBlocks = make(chan wtxmgr.BlockMeta)
	return w.connectedBlocks, nil
}

// ListenDisconnectedBlocks returns a channel that passes all blocks that a
// wallet has detached.  The channel must be read, or other wallet methods will
// block.
//
// If this is called twice, ErrDuplicateListen is returned.
func (w *Wallet) ListenDisconnectedBlocks() (<-chan wtxmgr.BlockMeta, error) {
	defer w.notificationMu.Unlock()
	w.notificationMu.Lock()

	if w.disconnectedBlocks != nil {
		return nil, ErrDuplicateListen
	}
	w.disconnectedBlocks = make(chan wtxmgr.BlockMeta)
	return w.disconnectedBlocks, nil
}

// ListenLockStatus returns a channel that passes the current lock state
// of the wallet whenever the lock state is changed.  The value is true for
// locked, and false for unlocked.  The channel must be read, or other wallet
// methods will block.
//
// If this is called twice, ErrDuplicateListen is returned.
func (w *Wallet) ListenLockStatus() (<-chan bool, error) {
	defer w.notificationMu.Unlock()
	w.notificationMu.Lock()

	if w.lockStateChanges != nil {
		return nil, ErrDuplicateListen
	}
	w.lockStateChanges = make(chan bool)
	return w.lockStateChanges, nil
}

// ListenConfirmedBalance returns a channel that passes the confirmed balance
// when any changes to the balance are made.  This channel must be read, or
// other wallet methods will block.
//
// If this is called twice, ErrDuplicateListen is returned.
func (w *Wallet) ListenConfirmedBalance() (<-chan coinutil.Amount, error) {
	defer w.notificationMu.Unlock()
	w.notificationMu.Lock()

	if w.confirmedBalance != nil {
		return nil, ErrDuplicateListen
	}
	w.confirmedBalance = make(chan coinutil.Amount)
	return w.confirmedBalance, nil
}

// ListenUnconfirmedBalance returns a channel that passes the unconfirmed
// balance when any changes to the balance are made.  This channel must be
// read, or other wallet methods will block.
//
// If this is called twice, ErrDuplicateListen is returned.
func (w *Wallet) ListenUnconfirmedBalance() (<-chan coinutil.Amount, error) {
	defer w.notificationMu.Unlock()
	w.notificationMu.Lock()

	if w.unconfirmedBalance != nil {
		return nil, ErrDuplicateListen
	}
	w.unconfirmedBalance = make(chan coinutil.Amount)
	return w.unconfirmedBalance, nil
}

// ListenRelevantTxs returns a channel that passes all transactions relevant to
// a wallet, optionally including metadata regarding the block they were mined
// in.  This channel must be read, or other wallet methods will block.
//
// If this is called twice, ErrDuplicateListen is returned.
func (w *Wallet) ListenRelevantTxs() (<-chan chain.RelevantTx, error) {
	defer w.notificationMu.Unlock()
	w.notificationMu.Lock()

	if w.relevantTxs != nil {
		return nil, ErrDuplicateListen
	}
	w.relevantTxs = make(chan chain.RelevantTx)
	return w.relevantTxs, nil
}

func (w *Wallet) notifyConnectedBlock(block wtxmgr.BlockMeta) {
	w.notificationMu.Lock()
	if w.connectedBlocks != nil {
		w.connectedBlocks <- block
	}
	w.notificationMu.Unlock()
}

func (w *Wallet) notifyDisconnectedBlock(block wtxmgr.BlockMeta) {
	w.notificationMu.Lock()
	if w.disconnectedBlocks != nil {
		w.disconnectedBlocks <- block
	}
	w.notificationMu.Unlock()
}

func (w *Wallet) notifyLockStateChange(locked bool) {
	w.notificationMu.Lock()
	if w.lockStateChanges != nil {
		w.lockStateChanges <- locked
	}
	w.notificationMu.Unlock()
}

func (w *Wallet) notifyConfirmedBalance(bal coinutil.Amount) {
	w.notificationMu.Lock()
	if w.confirmedBalance != nil {
		w.confirmedBalance <- bal
	}
	w.notificationMu.Unlock()
}

func (w *Wallet) notifyUnconfirmedBalance(bal coinutil.Amount) {
	w.notificationMu.Lock()
	if w.unconfirmedBalance != nil {
		w.unconfirmedBalance <- bal
	}
	w.notificationMu.Unlock()
}

func (w *Wallet) notifyRelevantTx(relevantTx chain.RelevantTx) {
	w.notificationMu.Lock()
	if w.relevantTxs != nil {
		w.relevantTxs <- relevantTx
	}
	w.notificationMu.Unlock()
}

// Start starts the goroutines necessary to manage a wallet.
func (w *Wallet) Start(chainServer *chain.Client) {
	w.quitMu.Lock()
	select {
	case <-w.quit:
		// Restart the wallet goroutines after shutdown finishes.
		w.WaitForShutdown()
		w.quit = make(chan struct{})
	default:
		// Ignore when the wallet is still running.
		if w.started {
			w.quitMu.Unlock()
			return
		}
		w.started = true
	}
	w.quitMu.Unlock()

	w.chainSvrLock.Lock()
	w.chainSvr = chainServer
	w.chainSvrLock.Unlock()

	w.wg.Add(6)
	go w.handleChainNotifications()
	go w.txCreator()
	go w.walletLocker()
	go w.rescanBatchHandler()
	go w.rescanProgressHandler()
	go w.rescanRPCHandler()
}

// quitChan atomically reads the quit channel.
func (w *Wallet) quitChan() <-chan struct{} {
	w.quitMu.Lock()
	c := w.quit
	w.quitMu.Unlock()
	return c
}

// Stop signals all wallet goroutines to shutdown.
func (w *Wallet) Stop() {
	w.quitMu.Lock()
	quit := w.quit
	w.quitMu.Unlock()

	select {
	case <-quit:
	default:
		close(quit)
		w.chainSvrLock.Lock()
		if w.chainSvr != nil {
			w.chainSvr.Stop()
		}
		w.chainSvrLock.Unlock()
	}
}

// ShuttingDown returns whether the wallet is currently in the process of
// shutting down or not.
func (w *Wallet) ShuttingDown() bool {
	select {
	case <-w.quitChan():
		return true
	default:
		return false
	}
}

// WaitForShutdown blocks until all wallet goroutines have finished executing.
func (w *Wallet) WaitForShutdown() {
	w.chainSvrLock.Lock()
	if w.chainSvr != nil {
		w.chainSvr.WaitForShutdown()
	}
	w.chainSvrLock.Unlock()
	w.wg.Wait()
}

// ChainSynced returns whether the wallet has been attached to a chain server
// and synced up to the best block on the main chain.
func (w *Wallet) ChainSynced() bool {
	w.chainSvrSyncMtx.Lock()
	synced := w.chainSvrSynced
	w.chainSvrSyncMtx.Unlock()
	return synced
}

// SetChainSynced marks whether the wallet is connected to and currently in sync
// with the latest block notified by the chain server.
//
// NOTE: Due to an API limitation with stcrpcclient, this may return true after
// the client disconnected (and is attempting a reconnect).  This will be unknown
// until the reconnect notification is received, at which point the wallet can be
// marked out of sync again until after the next rescan completes.
func (w *Wallet) SetChainSynced(synced bool) {
	w.chainSvrSyncMtx.Lock()
	w.chainSvrSynced = synced
	w.chainSvrSyncMtx.Unlock()
}

// activeData returns the currently-active receiving addresses and all unspent
// outputs.  This is primarely intended to provide the parameters for a
// rescan request.
func (w *Wallet) activeData() ([]coinutil.Address, []wtxmgr.Credit, error) {
	var addrs []coinutil.Address
	err := w.Manager.ForEachActiveAddress(func(addr coinutil.Address) error {
		addrs = append(addrs, addr)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	unspent, err := w.TxStore.UnspentOutputs()
	return addrs, unspent, err
}

// syncWithChain brings the wallet up to date with the current chain server
// connection.  It creates a rescan request and blocks until the rescan has
// finished.
//
func (w *Wallet) syncWithChain() error {
	// Request notifications for connected and disconnected blocks.
	//
	// TODO(jrick): Either request this notification only once, or when
	// stcrpcclient is modified to allow some notification request to not
	// automatically resent on reconnect, include the notifyblocks request
	// as well.  I am leaning towards allowing off all stcrpcclient
	// notification re-registrations, in which case the code here should be
	// left as is.
	err := w.chainSvr.NotifyBlocks()
	if err != nil {
		return err
	}

	// Request notifications for transactions sending to all wallet
	// addresses.
	addrs, unspent, err := w.activeData()
	if err != nil {
		return err
	}

	// TODO(jrick): How should this handle a synced height earlier than
	// the chain server best block?

	// Compare previously-seen blocks against the chain server.  If any of
	// these blocks no longer exist, rollback all of the missing blocks
	// before catching up with the rescan.
	iter := w.Manager.NewIterateRecentBlocks()
	rollback := iter == nil
	syncBlock := waddrmgr.BlockStamp{
		Hash:   *w.chainParams.GenesisHash,
		Height: 0,
	}
	for cont := iter != nil; cont; cont = iter.Prev() {
		bs := iter.BlockStamp()
		log.Debugf("Checking for previous saved block with height %v hash %v",
			bs.Height, bs.Hash)
		_, err = w.chainSvr.GetBlock(&bs.Hash)
		if err != nil {
			rollback = true
			continue
		}

		log.Debug("Found matching block.")
		syncBlock = bs
		break
	}
	if rollback {
		err = w.Manager.SetSyncedTo(&syncBlock)
		if err != nil {
			return err
		}
		// Rollback unconfirms transactions at and beyond the passed
		// height, so add one to the new synced-to height to prevent
		// unconfirming txs from the synced-to block.
		err = w.TxStore.Rollback(syncBlock.Height + 1)
		if err != nil {
			return err
		}
	}

	return w.Rescan(addrs, unspent)
}

type (
	createTxRequest struct {
		account uint32
		pairs   map[string]coinutil.Amount
		minconf int32
		resp    chan createTxResponse
	}
	createTxResponse struct {
		tx  *CreatedTx
		err error
	}
)

// txCreator is responsible for the input selection and creation of
// transactions.  These functions are the responsibility of this method
// (designed to be run as its own goroutine) since input selection must be
// serialized, or else it is possible to create double spends by choosing the
// same inputs for multiple transactions.  Along with input selection, this
// method is also responsible for the signing of transactions, since we don't
// want to end up in a situation where we run out of inputs as multiple
// transactions are being created.  In this situation, it would then be possible
// for both requests, rather than just one, to fail due to not enough available
// inputs.
func (w *Wallet) txCreator() {
	quit := w.quitChan()
out:
	for {
		select {
		case txr := <-w.createTxRequests:
			tx, err := w.txToPairs(txr.pairs, txr.account, txr.minconf)
			txr.resp <- createTxResponse{tx, err}

		case <-quit:
			break out
		}
	}
	w.wg.Done()
}

// CreateSimpleTx creates a new signed transaction spending unspent P2PKH
// outputs with at laest minconf confirmations spending to any number of
// address/amount pairs.  Change and an appropiate transaction fee are
// automatically included, if necessary.  All transaction creation through
// this function is serialized to prevent the creation of many transactions
// which spend the same outputs.
func (w *Wallet) CreateSimpleTx(account uint32, pairs map[string]coinutil.Amount,
	minconf int32) (*CreatedTx, error) {

	req := createTxRequest{
		account: account,
		pairs:   pairs,
		minconf: minconf,
		resp:    make(chan createTxResponse),
	}
	w.createTxRequests <- req
	resp := <-req.resp
	return resp.tx, resp.err
}

type (
	unlockRequest struct {
		passphrase []byte
		timeout    time.Duration // Zero value prevents the timeout.
		err        chan error
	}

	changePassphraseRequest struct {
		old, new []byte
		err      chan error
	}

	// HeldUnlock is a tool to prevent the wallet from automatically
	// locking after some timeout before an operation which needed
	// the unlocked wallet has finished.  Any aquired HeldUnlock
	// *must* be released (preferably with a defer) or the wallet
	// will forever remain unlocked.
	HeldUnlock chan struct{}
)

// walletLocker manages the locked/unlocked state of a wallet.
func (w *Wallet) walletLocker() {
	var timeout <-chan time.Time
	holdChan := make(HeldUnlock)
	quit := w.quitChan()
out:
	for {
		select {
		case req := <-w.unlockRequests:
			err := w.Manager.Unlock(req.passphrase)
			if err != nil {
				req.err <- err
				continue
			}
			w.notifyLockStateChange(false)
			if req.timeout == 0 {
				timeout = nil
			} else {
				timeout = time.After(req.timeout)
			}
			req.err <- nil
			continue

		case req := <-w.changePassphrase:
			err := w.Manager.ChangePassphrase(req.old, req.new, true,
				&waddrmgr.DefaultScryptOptions)
			req.err <- err
			continue

		case req := <-w.holdUnlockRequests:
			if w.Manager.IsLocked() {
				close(req)
				continue
			}

			req <- holdChan
			<-holdChan // Block until the lock is released.

			// If, after holding onto the unlocked wallet for some
			// time, the timeout has expired, lock it now instead
			// of hoping it gets unlocked next time the top level
			// select runs.
			select {
			case <-timeout:
				// Let the top level select fallthrough so the
				// wallet is locked.
			default:
				continue
			}

		case w.lockState <- w.Manager.IsLocked():
			continue

		case <-quit:
			break out

		case <-w.lockRequests:
		case <-timeout:
		}

		// Select statement fell through by an explicit lock or the
		// timer expiring.  Lock the manager here.
		if timeout != nil {
			timeout = nil
			err := w.Manager.Lock()
			if err != nil {
				log.Errorf("Could not lock wallet: %v", err)
			} else {
				w.notifyLockStateChange(true)
			}
		}
	}
	w.wg.Done()
}

// Unlock unlocks the wallet's address manager and relocks it after timeout has
// expired.  If the wallet is already unlocked and the new passphrase is
// correct, the current timeout is replaced with the new one.  The wallet will
// be locked if the passphrase is incorrect or any other error occurs during the
// unlock.
func (w *Wallet) Unlock(passphrase []byte, timeout time.Duration) error {
	err := make(chan error, 1)
	w.unlockRequests <- unlockRequest{
		passphrase: passphrase,
		timeout:    timeout,
		err:        err,
	}
	return <-err
}

// Lock locks the wallet's address manager.
func (w *Wallet) Lock() {
	w.lockRequests <- struct{}{}
}

// Locked returns whether the account manager for a wallet is locked.
func (w *Wallet) Locked() bool {
	return <-w.lockState
}

// HoldUnlock prevents the wallet from being locked.  The HeldUnlock object
// *must* be released, or the wallet will forever remain unlocked.
//
// TODO: To prevent the above scenario, perhaps closures should be passed
// to the walletLocker goroutine and disallow callers from explicitly
// handling the locking mechanism.
func (w *Wallet) HoldUnlock() (HeldUnlock, error) {
	req := make(chan HeldUnlock)
	w.holdUnlockRequests <- req
	hl, ok := <-req
	if !ok {
		// TODO(davec): This should be defined and exported from
		// waddrmgr.
		return nil, waddrmgr.ManagerError{
			ErrorCode:   waddrmgr.ErrLocked,
			Description: "address manager is locked",
		}
	}
	return hl, nil
}

// Release releases the hold on the unlocked-state of the wallet and allows the
// wallet to be locked again.  If a lock timeout has already expired, the
// wallet is locked again as soon as Release is called.
func (c HeldUnlock) Release() {
	c <- struct{}{}
}

// ChangePassphrase attempts to change the passphrase for a wallet from old
// to new.  Changing the passphrase is synchronized with all other address
// manager locking and unlocking.  The lock state will be the same as it was
// before the password change.
func (w *Wallet) ChangePassphrase(old, new []byte) error {
	err := make(chan error, 1)
	w.changePassphrase <- changePassphraseRequest{
		old: old,
		new: new,
		err: err,
	}
	return <-err
}

// AccountUsed returns whether there are any recorded transactions spending to
// a given account. It returns true if atleast one address in the account was
// used and false if no address in the account was used.
func (w *Wallet) AccountUsed(account uint32) (bool, error) {
	var used bool
	var err error
	merr := w.Manager.ForEachAccountAddress(account,
		func(maddr waddrmgr.ManagedAddress) error {
			used, err = maddr.Used()
			if err != nil {
				return err
			}
			if used {
				return waddrmgr.Break
			}
			return nil
		})
	if merr == waddrmgr.Break {
		merr = nil
	}
	return used, merr
}

// CalculateBalance sums the amounts of all unspent transaction
// outputs to addresses of a wallet and returns the balance.
//
// If confirmations is 0, all UTXOs, even those not present in a
// block (height -1), will be used to get the balance.  Otherwise,
// a UTXO must be in a block.  If confirmations is 1 or greater,
// the balance will be calculated based on how many how many blocks
// include a UTXO.
func (w *Wallet) CalculateBalance(confirms int32) (coinutil.Amount, error) {
	blk := w.Manager.SyncedTo()
	return w.TxStore.Balance(confirms, blk.Height)
}

// CalculateAccountBalance sums the amounts of all unspent transaction
// outputs to the given account of a wallet and returns the balance.
func (w *Wallet) CalculateAccountBalance(account uint32, confirms int32) (coinutil.Amount, error) {
	var bal coinutil.Amount

	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	syncBlock := w.Manager.SyncedTo()

	unspent, err := w.TxStore.UnspentOutputs()
	if err != nil {
		return 0, err
	}
	for i := range unspent {
		output := &unspent[i]

		if !confirmed(confirms, output.Height, syncBlock.Height) {
			continue
		}
		if output.FromCoinBase {
			const target = blockchain.CoinbaseMaturity
			if !confirmed(target, output.Height, syncBlock.Height) {
				continue
			}
		}

		var outputAcct uint32
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			output.PkScript, w.chainParams)
		if err == nil && len(addrs) > 0 {
			outputAcct, err = w.Manager.AddrAccount(addrs[0])
		}
		if err == nil && outputAcct == account {
			bal += output.Amount
		}
	}
	return bal, nil
}

// CurrentAddress gets the most recently requested Bitcoin payment address
// from a wallet.  If the address has already been used (there is at least
// one transaction spending to it in the blockchain or btcd mempool), the next
// chained address is returned.
func (w *Wallet) CurrentAddress(account uint32) (coinutil.Address, error) {
	addr, err := w.Manager.LastExternalAddress(account)
	if err != nil {
		// If no address exists yet, create the first external address
		if waddrmgr.IsError(err, waddrmgr.ErrAddressNotFound) {
			return w.NewAddress(account)
		}
		return nil, err
	}

	// Get next chained address if the last one has already been used.
	used, err := addr.Used()
	if err != nil {
		return nil, err
	}
	if used {
		return w.NewAddress(account)
	}

	return addr.Address(), nil
}

// CreditCategory describes the type of wallet transaction output.  The category
// of "sent transactions" (debits) is always "send", and is not expressed by
// this type.
//
// TODO: This is a requirement of the RPC server and should be moved.
type CreditCategory byte

// These constants define the possible credit categories.
const (
	CreditReceive CreditCategory = iota
	CreditGenerate
	CreditImmature
)

// String returns the category as a string.  This string may be used as the
// JSON string for categories as part of listtransactions and gettransaction
// RPC responses.
func (c CreditCategory) String() string {
	switch c {
	case CreditReceive:
		return "receive"
	case CreditGenerate:
		return "generate"
	case CreditImmature:
		return "immature"
	default:
		return "unknown"
	}
}

// RecvCategory returns the category of received credit outputs from a
// transaction record.  The passed block chain height is used to distinguish
// immature from mature coinbase outputs.
//
// TODO: This is intended for use by the RPC server and should be moved out of
// this package at a later time.
func RecvCategory(details *wtxmgr.TxDetails, syncHeight int32) CreditCategory {
	if blockchain.IsCoinBaseTx(&details.MsgTx) {
		if confirmed(blockchain.CoinbaseMaturity, details.Block.Height, syncHeight) {
			return CreditGenerate
		}
		return CreditImmature
	}
	return CreditReceive
}

// ListTransactions creates a object that may be marshalled to a response result
// for a listtransactions RPC.
//
// TODO: This should be moved out of this package into the main package's
// rpcserver.go, along with everything that requires this.
func ListTransactions(details *wtxmgr.TxDetails, syncHeight int32, net *chaincfg.Params) []btcjson.ListTransactionsResult {
	var (
		blockHashStr  string
		blockTime     int64
		confirmations int64
	)
	if details.Block.Height != -1 {
		blockHashStr = details.Block.Hash.String()
		blockTime = details.Block.Time.Unix()
		confirmations = int64(confirms(details.Block.Height, syncHeight))
	}

	results := []btcjson.ListTransactionsResult{}
	txHashStr := details.Hash.String()
	received := details.Received.Unix()
	generated := blockchain.IsCoinBaseTx(&details.MsgTx)
	recvCat := RecvCategory(details, syncHeight).String()

	send := len(details.Debits) != 0

	// Fee can only be determined if every input is a debit.
	var feeF64 float64
	if len(details.Debits) == len(details.MsgTx.TxIn) {
		var debitTotal coinutil.Amount
		for _, deb := range details.Debits {
			debitTotal += deb.Amount
		}
		var outputTotal coinutil.Amount
		for _, output := range details.MsgTx.TxOut {
			outputTotal += coinutil.Amount(output.Value)
		}
		// Note: The actual fee is debitTotal - outputTotal.  However,
		// this RPC reports negative numbers for fees, so the inverse
		// is calculated.
		feeF64 = (outputTotal - debitTotal).ToBTC()
	}

outputs:
	for i, output := range details.MsgTx.TxOut {
		// Determine if this output is a credit, and if so, determine
		// its spentness.
		var isCredit bool
		var spentCredit bool
		for _, cred := range details.Credits {
			if cred.Index == uint32(i) {
				// Change outputs are ignored.
				if cred.Change {
					continue outputs
				}

				isCredit = true
				spentCredit = cred.Spent
				break
			}
		}

		var address string
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(output.PkScript, net)
		if len(addrs) == 1 {
			address = addrs[0].EncodeAddress()
		}

		amountF64 := coinutil.Amount(output.Value).ToBTC()
		result := btcjson.ListTransactionsResult{
			// Fields left zeroed:
			//   InvolvesWatchOnly
			//   Account
			//   BlockIndex
			//
			// Fields set below:
			//   Category
			//   Amount
			//   Fee
			Address:         address,
			Vout:            uint32(i),
			Confirmations:   confirmations,
			Generated:       generated,
			BlockHash:       blockHashStr,
			BlockTime:       blockTime,
			TxID:            txHashStr,
			WalletConflicts: []string{},
			Time:            received,
			TimeReceived:    received,
		}

		// Add a received/generated/immature result if this is a credit.
		// If the output was spent, create a second result under the
		// send category with the inverse of the output amount.  It is
		// therefore possible that a single output may be included in
		// the results set zero, one, or two times.
		//
		// Since credits are not saved for outputs that are not
		// controlled by this wallet, all non-credits from transactions
		// with debits are grouped under the send category.

		if send || spentCredit {
			result.Category = "send"
			result.Amount = -amountF64
			result.Fee = &feeF64
			results = append(results, result)
		}
		if isCredit {
			result.Category = recvCat
			result.Amount = amountF64
			result.Fee = nil
			results = append(results, result)
		}
	}
	return results
}

// ListSinceBlock returns a slice of objects with details about transactions
// since the given block. If the block is -1 then all transactions are included.
// This is intended to be used for listsinceblock RPC replies.
func (w *Wallet) ListSinceBlock(start, end, syncHeight int32) ([]btcjson.ListTransactionsResult, error) {
	txList := []btcjson.ListTransactionsResult{}
	err := w.TxStore.RangeTransactions(start, end, func(details []wtxmgr.TxDetails) (bool, error) {
		for _, detail := range details {
			jsonResults := ListTransactions(&detail, syncHeight,
				w.chainParams)
			txList = append(txList, jsonResults...)
		}
		return false, nil
	})
	return txList, err
}

// ListTransactions returns a slice of objects with details about a recorded
// transaction.  This is intended to be used for listtransactions RPC
// replies.
func (w *Wallet) ListTransactions(from, count int) ([]btcjson.ListTransactionsResult, error) {
	txList := []btcjson.ListTransactionsResult{}

	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	syncBlock := w.Manager.SyncedTo()

	// Need to skip the first from transactions, and after those, only
	// include the next count transactions.
	skipped := 0
	n := 0

	// Return newer results first by starting at mempool height and working
	// down to the genesis block.
	err := w.TxStore.RangeTransactions(-1, 0, func(details []wtxmgr.TxDetails) (bool, error) {
		// Iterate over transactions at this height in reverse order.
		// This does nothing for unmined transactions, which are
		// unsorted, but it will process mined transactions in the
		// reverse order they were marked mined.
		for i := len(details) - 1; i >= 0; i-- {
			if from > skipped {
				skipped++
				continue
			}

			n++
			if n > count {
				return true, nil
			}

			jsonResults := ListTransactions(&details[i],
				syncBlock.Height, w.chainParams)
			txList = append(txList, jsonResults...)
		}

		return false, nil
	})

	return txList, err
}

// ListAddressTransactions returns a slice of objects with details about
// recorded transactions to or from any address belonging to a set.  This is
// intended to be used for listaddresstransactions RPC replies.
func (w *Wallet) ListAddressTransactions(pkHashes map[string]struct{}) (
	[]btcjson.ListTransactionsResult, error) {

	txList := []btcjson.ListTransactionsResult{}

	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	syncBlock := w.Manager.SyncedTo()

	err := w.TxStore.RangeTransactions(0, -1, func(details []wtxmgr.TxDetails) (bool, error) {
	loopDetails:
		for i := range details {
			detail := &details[i]

			for _, cred := range detail.Credits {
				pkScript := detail.MsgTx.TxOut[cred.Index].PkScript
				_, addrs, _, err := txscript.ExtractPkScriptAddrs(
					pkScript, w.chainParams)
				if err != nil || len(addrs) != 1 {
					continue
				}
				apkh, ok := addrs[0].(*coinutil.AddressPubKeyHash)
				if !ok {
					continue
				}
				_, ok = pkHashes[string(apkh.ScriptAddress())]
				if !ok {
					continue
				}

				jsonResults := ListTransactions(detail,
					syncBlock.Height, w.chainParams)
				if err != nil {
					return false, err
				}
				txList = append(txList, jsonResults...)
				continue loopDetails
			}
		}
		return false, nil
	})

	return txList, err
}

// ListAllTransactions returns a slice of objects with details about a recorded
// transaction.  This is intended to be used for listalltransactions RPC
// replies.
func (w *Wallet) ListAllTransactions() ([]btcjson.ListTransactionsResult, error) {
	txList := []btcjson.ListTransactionsResult{}

	// Get current block.  The block height used for calculating
	// the number of tx confirmations.
	syncBlock := w.Manager.SyncedTo()

	// Return newer results first by starting at mempool height and working
	// down to the genesis block.
	err := w.TxStore.RangeTransactions(-1, 0, func(details []wtxmgr.TxDetails) (bool, error) {
		// Iterate over transactions at this height in reverse order.
		// This does nothing for unmined transactions, which are
		// unsorted, but it will process mined transactions in the
		// reverse order they were marked mined.
		for i := len(details) - 1; i >= 0; i-- {
			jsonResults := ListTransactions(&details[i],
				syncBlock.Height, w.chainParams)
			txList = append(txList, jsonResults...)
		}
		return false, nil
	})

	return txList, err
}

// creditSlice satisifies the sort.Interface interface to provide sorting
// transaction credits from oldest to newest.  Credits with the same receive
// time and mined in the same block are not guaranteed to be sorted by the order
// they appear in the block.  Credits from the same transaction are sorted by
// output index.
type creditSlice []wtxmgr.Credit

func (s creditSlice) Len() int {
	return len(s)
}

func (s creditSlice) Less(i, j int) bool {
	switch {
	// If both credits are from the same tx, sort by output index.
	case s[i].OutPoint.Hash == s[j].OutPoint.Hash:
		return s[i].OutPoint.Index < s[j].OutPoint.Index

	// If both transactions are unmined, sort by their received date.
	case s[i].Height == -1 && s[j].Height == -1:
		return s[i].Received.Before(s[j].Received)

	// Unmined (newer) txs always come last.
	case s[i].Height == -1:
		return false
	case s[j].Height == -1:
		return true

	// If both txs are mined in different blocks, sort by block height.
	default:
		return s[i].Height < s[j].Height
	}
}

func (s creditSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// ListUnspent returns a slice of objects representing the unspent wallet
// transactions fitting the given criteria. The confirmations will be more than
// minconf, less than maxconf and if addresses is populated only the addresses
// contained within it will be considered.  If we know nothing about a
// transaction an empty array will be returned.
func (w *Wallet) ListUnspent(minconf, maxconf int32,
	addresses map[string]struct{}) ([]*btcjson.ListUnspentResult, error) {

	syncBlock := w.Manager.SyncedTo()

	filter := len(addresses) != 0

	unspent, err := w.TxStore.UnspentOutputs()
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(creditSlice(unspent)))

	defaultAccountName, err := w.Manager.AccountName(waddrmgr.DefaultAccountNum)
	if err != nil {
		return nil, err
	}

	results := make([]*btcjson.ListUnspentResult, 0, len(unspent))
	for i := range unspent {
		output := &unspent[i]

		// Outputs with fewer confirmations than the minimum or more
		// confs than the maximum are excluded.
		confs := confirms(output.Height, syncBlock.Height)
		if confs < minconf || confs > maxconf {
			continue
		}

		// Only mature coinbase outputs are included.
		if output.FromCoinBase {
			const target = blockchain.CoinbaseMaturity
			if !confirmed(target, output.Height, syncBlock.Height) {
				continue
			}
		}

		// Exclude locked outputs from the result set.
		if w.LockedOutpoint(output.OutPoint) {
			continue
		}

		// Lookup the associated account for the output.  Use the
		// default account name in case there is no associated account
		// for some reason, although this should never happen.
		//
		// This will be unnecessary once transactions and outputs are
		// grouped under the associated account in the db.
		acctName := defaultAccountName
		sc, addrs, _, err := txscript.ExtractPkScriptAddrs(
			output.PkScript, w.chainParams)
		if err != nil {
			continue
		}
		if len(addrs) > 0 {
			acct, err := w.Manager.AddrAccount(addrs[0])
			if err == nil {
				s, err := w.Manager.AccountName(acct)
				if err == nil {
					acctName = s
				}
			}
		}

		if filter {
			for _, addr := range addrs {
				_, ok := addresses[addr.EncodeAddress()]
				if ok {
					goto include
				}
			}
			continue
		}

	include:
		// At the moment watch-only addresses are not supported, so all
		// recorded outputs that are not multisig are "spendable".
		// Multisig outputs are only "spendable" if all keys are
		// controlled by this wallet.
		//
		// TODO: Each case will need updates when watch-only addrs
		// is added.  For P2PK, P2PKH, and P2SH, the address must be
		// looked up and not be watching-only.  For multisig, all
		// pubkeys must belong to the manager with the associated
		// private key (currently it only checks whether the pubkey
		// exists, since the private key is required at the moment).
		var spendable bool
	scSwitch:
		switch sc {
		case txscript.PubKeyHashTy:
			spendable = true
		case txscript.PubKeyTy:
			spendable = true
		case txscript.ScriptHashTy:
			spendable = true
		case txscript.MultiSigTy:
			for _, a := range addrs {
				_, err := w.Manager.Address(a)
				if err == nil {
					continue
				}
				if waddrmgr.IsError(err, waddrmgr.ErrAddressNotFound) {
					break scSwitch
				}
				return nil, err
			}
			spendable = true
		}

		result := &btcjson.ListUnspentResult{
			TxID:          output.OutPoint.Hash.String(),
			Vout:          output.OutPoint.Index,
			Account:       acctName,
			ScriptPubKey:  hex.EncodeToString(output.PkScript),
			Amount:        output.Amount.ToBTC(),
			Confirmations: int64(confs),
			Spendable:     spendable,
		}

		// BUG: this should be a JSON array so that all
		// addresses can be included, or removed (and the
		// caller extracts addresses from the pkScript).
		if len(addrs) > 0 {
			result.Address = addrs[0].EncodeAddress()
		}

		results = append(results, result)
	}

	return results, nil
}

// DumpPrivKeys returns the WIF-encoded private keys for all addresses with
// private keys in a wallet.
func (w *Wallet) DumpPrivKeys() ([]string, error) {
	var privkeys []string
	// Iterate over each active address, appending the private key to
	// privkeys.
	err := w.Manager.ForEachActiveAddress(func(addr coinutil.Address) error {
		ma, err := w.Manager.Address(addr)
		if err != nil {
			return err
		}

		// Only those addresses with keys needed.
		pka, ok := ma.(waddrmgr.ManagedPubKeyAddress)
		if !ok {
			return nil
		}

		wif, err := pka.ExportPrivKey()
		if err != nil {
			// It would be nice to zero out the array here. However,
			// since strings in go are immutable, and we have no
			// control over the caller I don't think we can. :(
			return err
		}
		privkeys = append(privkeys, wif.String())
		return nil
	})
	return privkeys, err
}

// DumpWIFPrivateKey returns the WIF encoded private key for a
// single wallet address.
func (w *Wallet) DumpWIFPrivateKey(addr coinutil.Address) (string, error) {
	// Get private key from wallet if it exists.
	address, err := w.Manager.Address(addr)
	if err != nil {
		return "", err
	}

	pka, ok := address.(waddrmgr.ManagedPubKeyAddress)
	if !ok {
		return "", fmt.Errorf("address %s is not a key type", addr)
	}

	wif, err := pka.ExportPrivKey()
	if err != nil {
		return "", err
	}
	return wif.String(), nil
}

// ImportPrivateKey imports a private key to the wallet and writes the new
// wallet to disk.
func (w *Wallet) ImportPrivateKey(wif *coinutil.WIF, bs *waddrmgr.BlockStamp,
	rescan bool) (string, error) {

	// The starting block for the key is the genesis block unless otherwise
	// specified.
	if bs == nil {
		bs = &waddrmgr.BlockStamp{
			Hash:   *w.chainParams.GenesisHash,
			Height: 0,
		}
	}

	// Attempt to import private key into wallet.
	addr, err := w.Manager.ImportPrivateKey(wif, bs)
	if err != nil {
		return "", err
	}

	// Rescan blockchain for transactions with txout scripts paying to the
	// imported address.
	if rescan {
		job := &RescanJob{
			Addrs:      []coinutil.Address{addr.Address()},
			OutPoints:  nil,
			BlockStamp: *bs,
		}

		// Submit rescan job and log when the import has completed.
		// Do not block on finishing the rescan.  The rescan success
		// or failure is logged elsewhere, and the channel is not
		// required to be read, so discard the return value.
		_ = w.SubmitRescan(job)
	}

	addrStr := addr.Address().EncodeAddress()
	log.Infof("Imported payment address %s", addrStr)

	// Return the payment address string of the imported private key.
	return addrStr, nil
}

// ExportWatchingWallet returns a watching-only version of the wallet serialized
// database as a base64-encoded string.
func (w *Wallet) ExportWatchingWallet(pubPass string) (string, error) {
	tmpDir, err := ioutil.TempDir("", "btcwallet")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	// Create a new file and write a copy of the current database into it.
	woDbPath := filepath.Join(tmpDir, walletDbWatchingOnlyName)
	fi, err := os.OpenFile(woDbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	if err := w.db.Copy(fi); err != nil {
		fi.Close()
		return "", err
	}
	fi.Close()
	defer os.Remove(woDbPath)

	// Open the new database, get the address manager namespace, and open
	// it.
	woDb, err := walletdb.Open("bdb", woDbPath)
	if err != nil {
		_ = os.Remove(woDbPath)
		return "", err
	}
	defer woDb.Close()

	namespace, err := woDb.Namespace(waddrmgrNamespaceKey)
	if err != nil {
		return "", err
	}
	woMgr, err := waddrmgr.Open(namespace, []byte(pubPass),
		w.chainParams, nil)
	if err != nil {
		return "", err
	}
	defer woMgr.Close()

	// Convert the namespace to watching only if needed.
	if err := woMgr.ConvertToWatchingOnly(); err != nil {
		// Only return the error is it's not because it's already
		// watching-only.  When it is already watching-only, the code
		// just falls through to the export below.
		if !waddrmgr.IsError(err, waddrmgr.ErrWatchingOnly) {
			return "", err
		}
	}

	// Export the watching only wallet's serialized data.
	woWallet := *w
	woWallet.db = woDb
	woWallet.Manager = woMgr
	return woWallet.exportBase64()
}

// exportBase64 exports a wallet's serialized database as a base64-encoded
// string.
func (w *Wallet) exportBase64() (string, error) {
	var buf bytes.Buffer
	if err := w.db.Copy(&buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// LockedOutpoint returns whether an outpoint has been marked as locked and
// should not be used as an input for created transactions.
func (w *Wallet) LockedOutpoint(op wire.OutPoint) bool {
	_, locked := w.lockedOutpoints[op]
	return locked
}

// LockOutpoint marks an outpoint as locked, that is, it should not be used as
// an input for newly created transactions.
func (w *Wallet) LockOutpoint(op wire.OutPoint) {
	w.lockedOutpoints[op] = struct{}{}
}

// UnlockOutpoint marks an outpoint as unlocked, that is, it may be used as an
// input for newly created transactions.
func (w *Wallet) UnlockOutpoint(op wire.OutPoint) {
	delete(w.lockedOutpoints, op)
}

// ResetLockedOutpoints resets the set of locked outpoints so all may be used
// as inputs for new transactions.
func (w *Wallet) ResetLockedOutpoints() {
	w.lockedOutpoints = map[wire.OutPoint]struct{}{}
}

// LockedOutpoints returns a slice of currently locked outpoints.  This is
// intended to be used by marshaling the result as a JSON array for
// listlockunspent RPC results.
func (w *Wallet) LockedOutpoints() []btcjson.TransactionInput {
	locked := make([]btcjson.TransactionInput, len(w.lockedOutpoints))
	i := 0
	for op := range w.lockedOutpoints {
		locked[i] = btcjson.TransactionInput{
			Txid: op.Hash.String(),
			Vout: op.Index,
		}
		i++
	}
	return locked
}

// ResendUnminedTxs iterates through all transactions that spend from wallet
// credits that are not known to have been mined into a block, and attempts
// to send each to the chain server for relay.
func (w *Wallet) ResendUnminedTxs() {
	txs, err := w.TxStore.UnminedTxs()
	if err != nil {
		log.Errorf("Cannot load unmined transactions for resending: %v", err)
		return
	}
	for _, tx := range txs {
		resp, err := w.chainSvr.SendRawTransaction(tx, false)
		if err != nil {
			// TODO(jrick): Check error for if this tx is a double spend,
			// remove it if so.
			log.Debugf("Could not resend transaction %v: %v",
				tx.TxSha(), err)
			continue
		}
		log.Debugf("Resent unmined transaction %v", resp)
	}
}

// SortedActivePaymentAddresses returns a slice of all active payment
// addresses in a wallet.
func (w *Wallet) SortedActivePaymentAddresses() ([]string, error) {
	var addrStrs []string
	err := w.Manager.ForEachActiveAddress(func(addr coinutil.Address) error {
		addrStrs = append(addrStrs, addr.EncodeAddress())
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Sort(sort.StringSlice(addrStrs))
	return addrStrs, nil
}

// NewAddress returns the next external chained address for a wallet.
func (w *Wallet) NewAddress(account uint32) (coinutil.Address, error) {
	// Get next address from wallet.
	addrs, err := w.Manager.NextExternalAddresses(account, 1)
	if err != nil {
		return nil, err
	}

	// Request updates from btcd for new transactions sent to this address.
	utilAddrs := make([]coinutil.Address, len(addrs))
	for i, addr := range addrs {
		utilAddrs[i] = addr.Address()
	}
	if err := w.chainSvr.NotifyReceived(utilAddrs); err != nil {
		return nil, err
	}

	return utilAddrs[0], nil
}

// NewChangeAddress returns a new change address for a wallet.
func (w *Wallet) NewChangeAddress(account uint32) (coinutil.Address, error) {
	// Get next chained change address from wallet for account.
	addrs, err := w.Manager.NextInternalAddresses(account, 1)
	if err != nil {
		return nil, err
	}

	// Request updates from btcd for new transactions sent to this address.
	utilAddrs := make([]coinutil.Address, len(addrs))
	for i, addr := range addrs {
		utilAddrs[i] = addr.Address()
	}

	if err := w.chainSvr.NotifyReceived(utilAddrs); err != nil {
		return nil, err
	}

	return utilAddrs[0], nil
}

// confirmed checks whether a transaction at height txHeight has met minconf
// confirmations for a blockchain at height curHeight.
func confirmed(minconf, txHeight, curHeight int32) bool {
	return confirms(txHeight, curHeight) >= minconf
}

// confirms returns the number of confirmations for a transaction in a block at
// height txHeight (or -1 for an unconfirmed tx) given the chain height
// curHeight.
func confirms(txHeight, curHeight int32) int32 {
	switch {
	case txHeight == -1, txHeight > curHeight:
		return 0
	default:
		return curHeight - txHeight + 1
	}
}

// TotalReceivedForAccount iterates through a wallet's transaction history,
// returning the total amount of bitcoins received for a single wallet
// account.
func (w *Wallet) TotalReceivedForAccount(account uint32, minConf int32) (coinutil.Amount, int32, error) {
	syncBlock := w.Manager.SyncedTo()

	var (
		amount     coinutil.Amount
		lastConf   int32 // Confs of the last matching transaction.
		stopHeight int32
	)

	if minConf > 0 {
		stopHeight = syncBlock.Height - minConf + 1
	} else {
		stopHeight = -1
	}
	err := w.TxStore.RangeTransactions(0, stopHeight, func(details []wtxmgr.TxDetails) (bool, error) {
		for i := range details {
			detail := &details[i]
			for _, cred := range detail.Credits {
				pkScript := detail.MsgTx.TxOut[cred.Index].PkScript
				var outputAcct uint32
				_, addrs, _, err := txscript.ExtractPkScriptAddrs(
					pkScript, w.chainParams)
				if err == nil && len(addrs) > 0 {
					outputAcct, err = w.Manager.AddrAccount(addrs[0])
				}
				if err == nil && outputAcct == account {
					amount += cred.Amount
					lastConf = confirms(detail.Block.Height, syncBlock.Height)
				}
			}
		}
		return false, nil
	})

	return amount, lastConf, err
}

// TotalReceivedForAddr iterates through a wallet's transaction history,
// returning the total amount of bitcoins received for a single wallet
// address.
func (w *Wallet) TotalReceivedForAddr(addr coinutil.Address, minConf int32) (coinutil.Amount, error) {
	syncBlock := w.Manager.SyncedTo()

	var (
		addrStr    = addr.EncodeAddress()
		amount     coinutil.Amount
		stopHeight int32
	)

	if minConf > 0 {
		stopHeight = syncBlock.Height - minConf + 1
	} else {
		stopHeight = -1
	}
	err := w.TxStore.RangeTransactions(0, stopHeight, func(details []wtxmgr.TxDetails) (bool, error) {
		for i := range details {
			detail := &details[i]
			for _, cred := range detail.Credits {
				pkScript := detail.MsgTx.TxOut[cred.Index].PkScript
				_, addrs, _, err := txscript.ExtractPkScriptAddrs(
					pkScript, w.chainParams)
				// An error creating addresses from the output script only
				// indicates a non-standard script, so ignore this credit.
				if err != nil {
					continue
				}
				for _, a := range addrs {
					if addrStr == a.EncodeAddress() {
						amount += cred.Amount
						break
					}
				}
			}
		}
		return false, nil
	})
	return amount, err
}

// SendPairs creates and sends payment transactions. It returns the transaction
// hash upon success
func (w *Wallet) SendPairs(amounts map[string]coinutil.Amount, account uint32,
	minconf int32) (*wire.ShaHash, error) {

	// Create transaction, replying with an error if the creation
	// was not successful.
	createdTx, err := w.CreateSimpleTx(account, amounts, minconf)
	if err != nil {
		return nil, err
	}

	// Create transaction record and insert into the db.
	rec, err := wtxmgr.NewTxRecordFromMsgTx(createdTx.MsgTx, time.Now())
	if err != nil {
		log.Errorf("Cannot create record for created transaction: %v", err)
		return nil, err
	}
	err = w.TxStore.InsertTx(rec, nil)
	if err != nil {
		log.Errorf("Error adding sent tx history: %v", err)
		return nil, err
	}

	if createdTx.ChangeIndex >= 0 {
		err = w.TxStore.AddCredit(rec, nil, uint32(createdTx.ChangeIndex), true)
		if err != nil {
			log.Errorf("Error adding change address for sent "+
				"tx: %v", err)
			return nil, err
		}
	}

	// TODO: The record already has the serialized tx, so no need to
	// serialize it again.
	return w.chainSvr.SendRawTransaction(&rec.MsgTx, false)
}

// Open loads an already-created wallet from the passed database and namespaces.
func Open(pubPass []byte, params *chaincfg.Params, db walletdb.DB, waddrmgrNS, wtxmgrNS walletdb.Namespace, cbs *waddrmgr.OpenCallbacks) (*Wallet, error) {
	addrMgr, err := waddrmgr.Open(waddrmgrNS, pubPass, params, cbs)
	if err != nil {
		return nil, err
	}
	txMgr, err := wtxmgr.Open(wtxmgrNS)
	if err != nil {
		if !wtxmgr.IsNoExists(err) {
			return nil, err
		}
		log.Info("No recorded transaction history -- needs full rescan")
		err = addrMgr.SetSyncedTo(nil)
		if err != nil {
			return nil, err
		}
		txMgr, err = wtxmgr.Create(wtxmgrNS)
		if err != nil {
			return nil, err
		}
	}

	log.Infof("Opened wallet") // TODO: log balance? last sync height?
	w := &Wallet{
		db:                  db,
		Manager:             addrMgr,
		TxStore:             txMgr,
		lockedOutpoints:     map[wire.OutPoint]struct{}{},
		FeeIncrement:        defaultFeeIncrement,
		rescanAddJob:        make(chan *RescanJob),
		rescanBatch:         make(chan *rescanBatch),
		rescanNotifications: make(chan interface{}),
		rescanProgress:      make(chan *RescanProgressMsg),
		rescanFinished:      make(chan *RescanFinishedMsg),
		createTxRequests:    make(chan createTxRequest),
		unlockRequests:      make(chan unlockRequest),
		lockRequests:        make(chan struct{}),
		holdUnlockRequests:  make(chan chan HeldUnlock),
		lockState:           make(chan bool),
		changePassphrase:    make(chan changePassphraseRequest),
		chainParams:         params,
		quit:                make(chan struct{}),
	}
	return w, nil
}
