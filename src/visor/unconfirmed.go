package visor

import (
    "errors"
    "github.com/skycoin/skycoin/src/coin"
    "github.com/skycoin/skycoin/src/util"
    "time"
)

// Performs additional transaction verification at the unconfirmed pool level.
// This checks tunable parameters that should prevent the transaction from
// entering the blockchain, but cannot be done at the blockchain level because
// they may be changed.
func VerifyTransaction(bc *coin.Blockchain, t *coin.Transaction, maxSize int,
    burnFactor uint64) error {
    if t.Size() > maxSize {
        return errors.New("Transaction too large")
    }
    if fee, err := bc.TransactionFee(t); err != nil {
        return err
    } else if burnFactor != 0 && t.OutputHours()/burnFactor > fee {
        return errors.New("Transaction fee minimum not met")
    }
    return nil
}

// Maps from coin.Transaction hash to its expected unspents.  The unspents'
// Head can be different at execution time, but the Unspent's hash is fixed.
type TxnUnspents map[coin.SHA256]coin.UxArray

// Returns all Unspents for a single address
func (self TxnUnspents) AllForAddress(a coin.Address) coin.UxArray {
    uxo := make(coin.UxArray, 0)
    for _, uxa := range self {
        for i, _ := range uxa {
            if uxa[i].Body.Address == a {
                uxo = append(uxo, uxa[i])
            }
        }
    }
    return uxo
}

type UnconfirmedTxn struct {
    Txn coin.Transaction
    // Time the txn was last received
    Received time.Time
    // Time the txn was last checked against the blockchain
    Checked time.Time
    // Last time we announced this txn
    Announced time.Time
}

// Returns the coin.Transaction's hash
func (self *UnconfirmedTxn) Hash() coin.SHA256 {
    return self.Txn.Hash()
}

// Manages unconfirmed transactions
type UnconfirmedTxnPool struct {
    Txns map[coin.SHA256]UnconfirmedTxn
    // Predicted unspents, assuming txns are valid.  Needed to predict
    // our future balance and avoid double spending our own coins
    // Maps from Transaction.Hash() to UxArray.
    Unspent TxnUnspents
}

func NewUnconfirmedTxnPool() *UnconfirmedTxnPool {
    return &UnconfirmedTxnPool{
        Txns:    make(map[coin.SHA256]UnconfirmedTxn),
        Unspent: make(TxnUnspents),
    }
}

func (self *UnconfirmedTxnPool) SetAnnounced(h coin.SHA256, t time.Time) {
    if tx, ok := self.Txns[h]; ok {
        tx.Announced = t
        self.Txns[h] = tx
    }
}

// Creates an unconfirmed transaction
func (self *UnconfirmedTxnPool) createUnconfirmedTxn(bcUnsp *coin.UnspentPool,
    t coin.Transaction, addrs map[coin.Address]byte) UnconfirmedTxn {
    now := util.Now()
    return UnconfirmedTxn{
        Txn:       t,
        Received:  now,
        Checked:   now,
        Announced: util.ZeroTime(),
    }
}

// Adds a coin.Transaction to the pool, or updates an existing one's timestamps
// Returns an error if txn is invalid, and whether the transaction already
// existed in the pool.
func (self *UnconfirmedTxnPool) RecordTxn(bc *coin.Blockchain,
    t coin.Transaction, addrs map[coin.Address]byte, maxSize int,
    burnFactor uint64) (error, bool) {
    if err := VerifyTransaction(bc, &t, maxSize, burnFactor); err != nil {
        return err, false
    }
    if err := bc.VerifyTransaction(t); err != nil {
        return err, false
    }

    // Update if we already have this txn
    h := t.Hash()
    ut, ok := self.Txns[h]
    if ok {
        now := util.Now()
        ut.Received = now
        ut.Checked = now
        self.Txns[h] = ut
        return nil, true
    }

    // Add txn to index
    self.Txns[h] = self.createUnconfirmedTxn(&bc.Unspent, t, addrs)
    // Add predicted unspents
    self.Unspent[h] = coin.CreateUnspents(bc.Head().Head, t)

    return nil, false
}

// Returns underlying coin.Transactions
func (self *UnconfirmedTxnPool) RawTxns() coin.Transactions {
    txns := make(coin.Transactions, len(self.Txns))
    i := 0
    for _, t := range self.Txns {
        txns[i] = t.Txn
        i++
    }
    return txns
}

// Remove a single txn by hash
func (self *UnconfirmedTxnPool) removeTxn(bc *coin.Blockchain,
    txHash coin.SHA256) {
    delete(self.Txns, txHash)
    delete(self.Unspent, txHash)
}

// Removes multiple txns at once. Slightly more efficient than a series of
// single RemoveTxns.  Hashes is an array of Transaction hashes.
func (self *UnconfirmedTxnPool) removeTxns(bc *coin.Blockchain,
    hashes []coin.SHA256) {
    for i, _ := range hashes {
        delete(self.Txns, hashes[i])
        delete(self.Unspent, hashes[i])
    }
}

// Removes confirmed txns from the pool
func (self *UnconfirmedTxnPool) RemoveTransactions(bc *coin.Blockchain,
    txns coin.Transactions) {
    toRemove := make([]coin.SHA256, len(txns))
    for i, _ := range txns {
        toRemove[i] = txns[i].Hash()
    }
    self.removeTxns(bc, toRemove)
}

// Checks all unconfirmed txns against the blockchain. maxAge is how long
// we'll hold a txn regardless of whether it has been invalidated.
// checkPeriod is how often we check the txn against the blockchain.
func (self *UnconfirmedTxnPool) Refresh(bc *coin.Blockchain,
    checkPeriod, maxAge time.Duration) {
    now := util.Now()
    toRemove := make([]coin.SHA256, 0)
    for k, t := range self.Txns {
        if now.Sub(t.Received) >= maxAge {
            toRemove = append(toRemove, k)
        } else if now.Sub(t.Checked) >= checkPeriod {
            if bc.VerifyTransaction(t.Txn) == nil {
                t.Checked = now
                self.Txns[k] = t
            } else {
                toRemove = append(toRemove, k)
            }
        }
    }
    self.removeTxns(bc, toRemove)
}

// Returns txn hashes with known ones removed
func (self *UnconfirmedTxnPool) FilterKnown(txns []coin.SHA256) []coin.SHA256 {
    unknown := make([]coin.SHA256, 0)
    for _, h := range txns {
        if _, known := self.Txns[h]; !known {
            unknown = append(unknown, h)
        }
    }
    return unknown
}

// Returns all known coin.Transactions from the pool, given hashes to select
func (self *UnconfirmedTxnPool) GetKnown(txns []coin.SHA256) coin.Transactions {
    known := make(coin.Transactions, 0)
    for _, h := range txns {
        if txn, have := self.Txns[h]; have {
            known = append(known, txn.Txn)
        }
    }
    return known
}

// Returns all unconfirmed coin.UxOut spends for addresses
// Looks at all inputs for unconfirmed txns, gets their source UxOut from the
// blockchain's unspent pool, and returns as coin.AddressUxOuts
// TODO -- optimize or cache
func (self *UnconfirmedTxnPool) SpendsForAddresses(bcUnspent *coin.UnspentPool,
    a map[coin.Address]byte) coin.AddressUxOuts {
    auxs := make(coin.AddressUxOuts, len(a))
    for _, utx := range self.Txns {
        for _, h := range utx.Txn.In {
            if ux, ok := bcUnspent.Get(h); ok {
                if _, ok := a[ux.Body.Address]; ok {
                    auxs[ux.Body.Address] = append(auxs[ux.Body.Address], ux)
                }
            }
        }
    }
    return auxs
}

func (self *UnconfirmedTxnPool) SpendsForAddress(bcUnspent *coin.UnspentPool,
    a coin.Address) coin.UxArray {
    ma := map[coin.Address]byte{a: 1}
    auxs := self.SpendsForAddresses(bcUnspent, ma)
    return auxs[a]
}
