// Copyright 2016 The go-ethereum Authors
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

package les

import (
	"math"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	numRelayPeers = 3 // number of full nodes a tx is sent to
)

type ltrInfo struct {
	tx     *types.Transaction
	sentTo map[*peer]struct{}
}

type LesTxRelay struct {
	txSent    map[common.Hash][]*ltrInfo
	txPending map[common.Hash]struct{}
	ps        *peerSet
	peerList  []*peer
	lock      sync.RWMutex

	reqDist *requestDistributor
}

func NewLesTxRelay(ps *peerSet, reqDist *requestDistributor) *LesTxRelay {
	r := &LesTxRelay{
		txSent:    make(map[common.Hash][]*ltrInfo),
		txPending: make(map[common.Hash]struct{}),
		ps:        ps,
		reqDist:   reqDist,
	}
	ps.notify(r)
	return r
}

func (self *LesTxRelay) registerPeer(p *peer) {
	self.lock.Lock()
	defer self.lock.Unlock()

	self.peerList = self.ps.AllPeers()
}

func (self *LesTxRelay) unregisterPeer(p *peer) {
	self.lock.Lock()
	defer self.lock.Unlock()

	self.peerList = self.ps.AllPeers()
}

func (self *LesTxRelay) HasPeerWithEtherbase(etherbase common.Address) error {
	_, err := self.ps.getPeerWithEtherbase(etherbase)
	return err
}

// send sends a list of transactions to at most a given number of peers at
// once, never resending any particular transaction to the same peer twice
func (self *LesTxRelay) send(txs types.Transactions) {
	sendTo := make(map[*peer]types.Transactions)

	for _, tx := range txs {
		hash := tx.Hash()
		_, ok := self.txSent[hash]
		if !ok {
			ltrs := make([]*ltrInfo, 0)

			for i := 0; i < int(math.Min(numRelayPeers, float64(len(self.peerList)))); i++ {
				// TODO(henryzhang) make a deep copy of this transaction
				newTx := types.Transaction(*tx)

				// TODO(henryzhang) assign the copy with a new gas fee recipient
				p, err := self.ps.getPeerWithEtherbase(*newTx.GasFeeRecipient())
				// TODO(asa): When this happens, the nonce is still incremented, preventing future txs from being added.
				// We rely on transactions to be rejected in light/txpool validateTx to prevent transactions
				// with GasFeeRecipient != one of our peers from making it to the relayer.
				if err != nil {
					log.Error("Unable to find peer with matching etherbase", "err", err, "tx.hash", tx.Hash(), "tx.gasFeeRecipient", tx.GasFeeRecipient())
					continue
				}
				sendTo[p] = append(sendTo[p], &newTx)
				ltr := &ltrInfo{
					tx:     &newTx,
					sentTo: make(map[*peer]struct{}),
				}
				ltrs = append(ltrs, ltr)
			}

			self.txSent[hash] = ltrs
			self.txPending[hash] = struct{}{}
		}
	}

	for p, list := range sendTo {
		pp := p
		ll := list

		reqID := genReqID()
		rq := &distReq{
			getCost: func(dp distPeer) uint64 {
				peer := dp.(*peer)
				return peer.GetRequestCost(SendTxMsg, len(ll))
			},
			canSend: func(dp distPeer) bool {
				return dp.(*peer) == pp
			},
			request: func(dp distPeer) func() {
				peer := dp.(*peer)
				cost := peer.GetRequestCost(SendTxMsg, len(ll))
				peer.fcServer.QueueRequest(reqID, cost)
				return func() { peer.SendTxs(reqID, cost, ll) }
			},
		}
		self.reqDist.queue(rq)
	}
}

func (self *LesTxRelay) Send(txs types.Transactions) {
	self.lock.Lock()
	defer self.lock.Unlock()

	self.send(txs)
}

func (self *LesTxRelay) NewHead(head common.Hash, mined []common.Hash, rollback []common.Hash) {
	self.lock.Lock()
	defer self.lock.Unlock()

	for _, hash := range mined {
		delete(self.txPending, hash)
	}

	for _, hash := range rollback {
		self.txPending[hash] = struct{}{}
	}

	if len(self.txPending) > 0 {
		txs := make(types.Transactions, 0)
		for hash := range self.txPending {
			for _, ltr := range self.txSent[hash] {
				txs = append(txs, ltr.tx)
			}
		}
		self.send(txs)
	}
}

func (self *LesTxRelay) Discard(hashes []common.Hash) {
	self.lock.Lock()
	defer self.lock.Unlock()

	for _, hash := range hashes {
		delete(self.txSent, hash)
		delete(self.txPending, hash)
	}
}
