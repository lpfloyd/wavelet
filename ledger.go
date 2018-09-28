package wavelet

import (
	"github.com/lytics/hll"
	"github.com/perlin-network/graph/conflict"
	"github.com/perlin-network/graph/database"
	"github.com/perlin-network/graph/graph"
	"github.com/perlin-network/graph/system"
	"github.com/perlin-network/wavelet/log"
	"github.com/phf/go-queue/queue"
	"time"
)

var (
	BucketAccepted      = writeBytes("accepted_")
	BucketAcceptPending = writeBytes("p.accepted_")
)

type Ledger struct {
	state
	rpc

	*database.Store
	*graph.Graph
	*conflict.Resolver

	kill chan struct{}
}

func NewLedger() *Ledger {
	store := database.New("testdb")

	graph := graph.New(store)
	resolver := conflict.New(graph)

	ledger := &Ledger{
		Store:    store,
		Graph:    graph,
		Resolver: resolver,
		kill:     make(chan struct{}),
	}

	ledger.state = state{Ledger: ledger}
	ledger.rpc = rpc{Ledger: ledger}

	graph.AddOnReceiveHandler(ledger.ensureSafeCommittable)

	return ledger
}

// UpdateAcceptedTransactions incrementally from the root of the graph updates whether
// or not all transactions this node knows about are accepted.
//
// The graph will be incrementally checked for updates periodically. Ideally, you should
// execute this function in a new goroutine.
func (ledger *Ledger) UpdateAcceptedTransactions() {
	timer := time.NewTicker(100 * time.Millisecond)

	for {
		select {
		case <-ledger.kill:
			break
		case <-timer.C:
			ledger.updatedAcceptedTransactions()
		}
	}

	timer.Stop()
}

// updateAcceptedTransactions incrementally from the root of the graph updates whether
// or not all transactions this node knows about are accepted.
func (ledger *Ledger) updatedAcceptedTransactions() {
	// If there are no accepted transactions and none are pending, add the very first transaction.
	if ledger.Size(BucketAcceptPending) == 0 && ledger.Size(BucketAccepted) == 0 {
		tx, err := ledger.GetByIndex(0)
		if err != nil {
			return
		}

		ledger.Put(merge(BucketAcceptPending, writeBytes(tx.Id)), []byte{0})
	}

	var acceptedList []string

	ledger.ForEachKey(BucketAcceptPending, func(k []byte) error {
		symbol := string(k)

		tx, err := ledger.GetBySymbol(symbol)
		if err != nil {
			return nil
		}

		set, err := ledger.GetConflictSet(tx.Sender, tx.Nonce)
		if err != nil {
			return nil
		}

		transactions := new(hll.Hll)
		err = transactions.UnmarshalPb(set.Transactions)

		if err != nil {
			return nil
		}

		parentsAccepted := true
		for _, parent := range tx.Parents {
			if !ledger.WasAccepted(parent) {
				parentsAccepted = false
				break
			}
		}

		if parentsAccepted {
			conflicting := !(transactions.Cardinality() == 1)

			if set.Count > system.Beta2 || (!conflicting && ledger.CountAscendants(symbol, system.Beta1+1) > system.Beta1) {
				if !ledger.WasAccepted(symbol) {
					ledger.acceptTransaction(symbol)
					acceptedList = append(acceptedList, symbol)
				}
			}
		}

		return nil
	})

	if len(acceptedList) > 0 {
		// Trim transaction IDs.
		for i := 0; i < len(acceptedList); i++ {
			acceptedList[i] = acceptedList[i][:10]
		}

		log.Info().Interface("accepted", acceptedList).Msgf("Accepted %d transactions.", len(acceptedList))
	}
}

// WasAccepted returns whether or not a transaction given by its symbol
// was stored to be accepted inside the database.
func (ledger *Ledger) WasAccepted(symbol string) bool {
	bytes, _ := ledger.Get(merge(BucketAccepted, writeBytes(symbol)))
	return readBoolean(bytes)
}

// ensureAccepted gets called every single time the preferred transaction of a conflict set changes.
//
// It ensures that preferred transactions that were accepted, which should instead be rejected get
// reverted alongside all of their ascendant transactions.
func (ledger *Ledger) ensureAccepted(set *database.ConflictSet) error {
	transactions := new(hll.Hll)

	err := transactions.UnmarshalPb(set.Transactions)

	if err != nil {
		return err
	}

	// If the preferred transaction of a conflict set was accepted (due to safe early commit) and there are now transactions
	// conflicting with it, un-accept it.
	if conflicting := !(transactions.Cardinality() == 1); conflicting && ledger.WasAccepted(set.Preferred) && set.Count <= system.Beta2 {
		ledger.revertTransaction(set.Preferred)
	}

	return nil
}

// acceptTransaction accepts a transaction and ensures the transaction is not pending acceptance inside the graph.
// The children of said accepted transaction thereafter get queued to pending acceptance.
func (ledger *Ledger) acceptTransaction(symbol string) {
	ledger.Put(merge(BucketAccepted, writeBytes(symbol)), writeBoolean(true))
	ledger.Delete(merge(BucketAcceptPending, writeBytes(symbol)))

	visited := make(map[string]struct{})

	queue := queue.New()
	queue.PushBack(symbol)

	for queue.Len() > 0 {
		popped := queue.PopFront().(string)

		children, err := ledger.GetChildrenBySymbol(popped)
		if err != nil {
			continue
		}

		for _, child := range children.Transactions {
			if _, seen := visited[child]; !seen {
				visited[child] = struct{}{}

				if !ledger.WasAccepted(child) {
					ledger.Put(merge(BucketAcceptPending, writeBytes(child)), []byte{0})
				}
				queue.PushBack(child)

			}
		}
	}
}

// revertTransaction sets a transaction and all of its ascendants to not be accepted.
func (ledger *Ledger) revertTransaction(symbol string) {
	numReverted := 0

	visited := make(map[string]struct{})

	queue := queue.New()
	queue.PushBack(symbol)

	for queue.Len() > 0 {
		popped := queue.PopFront().(string)
		numReverted++

		ledger.Delete(merge(BucketAccepted, writeBytes(popped)))
		ledger.Put(merge(BucketAcceptPending, writeBytes(popped)), []byte{0})

		children, err := ledger.GetChildrenBySymbol(popped)
		if err != nil {
			continue
		}

		for _, child := range children.Transactions {
			if _, seen := visited[child]; !seen {
				visited[child] = struct{}{}

				queue.PushBack(child)
			}
		}
	}

	log.Debug().Int("num_reverted", numReverted).Msg("Reverted transactions.")
}

// ensureSafeCommittable ensures that incoming transactions which conflict with any
// of the transactions on our graph are not accepted.
func (ledger *Ledger) ensureSafeCommittable(index uint64, tx *database.Transaction) error {
	set, err := ledger.GetConflictSet(tx.Sender, tx.Nonce)

	if err != nil {
		return err
	}

	return ledger.ensureAccepted(set)
}
