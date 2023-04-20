package txmgr

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/core/types"
	"golang.org/x/sync/errgroup"
)

type TxReceipt[T any] struct {
	// ID can be used to identify unique tx receipts within the recept channel
	ID T
	// Receipt result from the transaction send
	Receipt *types.Receipt
	// Err contains any error that occurred during the tx send
	Err error
}

type Queue[T any] struct {
	ctx            context.Context
	txMgr          TxManager
	maxPending     uint64
	pendingChanged func(uint64)
	receiptWg      sync.WaitGroup
	pending        atomic.Uint64
	groupLock      sync.Mutex
	groupCtx       context.Context
	group          *errgroup.Group
}

// NewQueue creates a new transaction sending Queue, with the following parameters:
//   - maxPending: max number of pending txs at once (0 == no limit)
//   - pendingChanged: called whenever a tx send starts or finishes. The
//     number of currently pending txs is passed as a parameter.
func NewQueue[T any](ctx context.Context, txMgr TxManager, maxPending uint64, pendingChanged func(uint64)) *Queue[T] {
	if maxPending > math.MaxInt {
		// ensure we don't overflow as errgroup only accepts int; in reality this will never be an issue
		maxPending = math.MaxInt
	}
	return &Queue[T]{
		ctx:            ctx,
		txMgr:          txMgr,
		maxPending:     maxPending,
		pendingChanged: pendingChanged,
	}
}

// Wait waits for all pending txs to complete (or fail).
func (q *Queue[T]) Wait() {
	q.receiptWg.Wait()
	if q.group == nil {
		return
	}
	_ = q.group.Wait()
}

// Send will wait until the number of pending txs is below the max pending,
// and then send the next tx.
//
// The actual tx sending is non-blocking, with the receipt returned on the
// provided receipt channel.
func (q *Queue[T]) Send(id T, candidate TxCandidate, receiptCh chan TxReceipt[T]) {
	q.receiptWg.Add(1)
	group, ctx := q.groupContext()
	group.Go(func() error {
		return q.sendTx(ctx, id, candidate, receiptCh)
	})
}

// TrySend sends the next tx, but only if the number of pending txs is below the
// max pending.
//
// Returns false if there is no room in the queue to send. Otherwise, the
// transaction is queued and this method returns true.
//
// The actual tx sending is non-blocking, with the receipt returned on the
// provided receipt channel.
func (q *Queue[T]) TrySend(id T, candidate TxCandidate, receiptCh chan TxReceipt[T]) bool {
	q.receiptWg.Add(1)
	group, ctx := q.groupContext()
	started := group.TryGo(func() error {
		return q.sendTx(ctx, id, candidate, receiptCh)
	})
	if !started {
		// send didn't start so receipt will never be available
		q.receiptWg.Done()
	}
	return started
}

func (q *Queue[T]) sendTx(ctx context.Context, id T, candidate TxCandidate, receiptCh chan TxReceipt[T]) error {
	q.pendingChanged(q.pending.Add(1))
	defer func() {
		q.pendingChanged(q.pending.Add(^uint64(0))) // -1
	}()
	receipt, err := q.txMgr.Send(ctx, candidate)
	go func() {
		// notify from a goroutine to ensure the receipt channel won't block method completion
		receiptCh <- TxReceipt[T]{
			ID:      id,
			Receipt: receipt,
			Err:     err,
		}
		q.receiptWg.Done()
	}()
	return err
}

// mergeWithGroupContext creates a new Context that is canceled if either the given context is
// Done, or the group context is canceled. The returned CancelFunc should be called once finished.
//
// If the group context doesn't exist or has already been canceled, a new one is created after
// waiting for existing group threads to complete.
func (q *Queue[T]) groupContext() (*errgroup.Group, context.Context) {
	q.groupLock.Lock()
	defer q.groupLock.Unlock()
	if q.groupCtx == nil || q.groupCtx.Err() != nil {
		// no group exists, or the existing context has an error, so we need to wait
		// for existing group threads to complete (if any) and create a new group
		if q.group != nil {
			_ = q.group.Wait()
		}
		q.group, q.groupCtx = errgroup.WithContext(q.ctx)
		if q.maxPending > 0 {
			q.group.SetLimit(int(q.maxPending))
		}
	}
	return q.group, q.groupCtx
}
