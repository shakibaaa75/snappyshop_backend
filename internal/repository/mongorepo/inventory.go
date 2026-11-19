package mongorepo

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// txTimeout bounds a whole inventory transaction (including the automatic
// retries the driver does on write conflicts between two buyers).
const txTimeout = 15 * time.Second

/* ------------------------------------------------------------------ */
/* Transaction runner                                                  */
/* ------------------------------------------------------------------ */

// undoLog collects compensating actions. It is only used when the database
// does not support transactions (a standalone mongod in local development):
// there, a failure half-way is rolled back by hand. With a replica set /
// Atlas it is nil and MongoDB aborts the transaction for us.
type undoLog struct{ fns []func(context.Context) }

func (u *undoLog) add(f func(context.Context)) {
	if u != nil {
		u.fns = append(u.fns, f)
	}
}

// txRunner runs a function as one multi-document transaction.
type txRunner struct {
	client   *mongo.Client
	noTx     atomic.Bool // set once we learn the deployment has no transactions
	warnOnce sync.Once
}

// run executes fn atomically. fn MUST be safe to run more than once (the
// driver retries it on transient errors such as two buyers hitting the same
// product at the same instant), so it must build all its state from scratch
// and use the ctx it is given for every database call.
func (t *txRunner) run(ctx context.Context, fn func(ctx context.Context, undo *undoLog) error) error {
	if !t.noTx.Load() {
		sess, err := t.client.StartSession()
		if err != nil {
			return err
		}
		defer sess.EndSession(ctx)

		_, err = sess.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
			return nil, fn(sc, nil)
		}, options.Transaction().SetWriteConcern(writeconcern.Majority()))
		if err == nil || !isTxUnsupported(err) {
			return err
		}
		// Standalone MongoDB: remember it and fall through to the
		// compensating path below.
		t.noTx.Store(true)
		t.warnOnce.Do(func() {
			slog.Warn("MongoDB transactions are not available (standalone server?); " +
				"inventory falls back to atomic per-product updates with manual rollback. " +
				"Use a replica set or Atlas in production.")
		})
	}

	undo := &undoLog{}
	if err := fn(ctx, undo); err != nil {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		for i := len(undo.fns) - 1; i >= 0; i-- {
			undo.fns[i](bg)
		}
		return err
	}
	return nil
}

func isTxUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Transaction numbers are only allowed on a replica set member or mongos")
}

/* ------------------------------------------------------------------ */
/* Stock operations                                                    */
/* ------------------------------------------------------------------ */

// reserve takes every line off the products' stock. Each line is ONE atomic
// conditional update ("only if stock >= qty"), so two buyers can never both
// take the last unit, even without a transaction. Inside a transaction a
// failure on any line rolls back the lines already taken.
func (r *Orders) reserve(ctx context.Context, undo *undoLog, sellerID string, lines []models.OrderLine) error {
	for _, l := range lines {
		filter, update := decrementOp(sellerID, l)
		res, err := r.products.UpdateOne(ctx, filter, update)
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return r.insufficient(ctx, sellerID, l)
		}
		l := l
		undo.add(func(c context.Context) { r.releaseLine(c, sellerID, l) })
	}
	return nil
}

// release puts every line's quantity back. Products or variants that were
// deleted in the meantime are skipped: there is nothing left to restock.
func (r *Orders) release(ctx context.Context, sellerID string, lines []models.OrderLine) error {
	for _, l := range lines {
		filter, update := incrementOp(sellerID, l)
		if _, err := r.products.UpdateOne(ctx, filter, update); err != nil {
			return err
		}
	}
	return nil
}

// releaseLine is the best-effort variant used for manual rollback.
func (r *Orders) releaseLine(ctx context.Context, sellerID string, l models.OrderLine) {
	filter, update := incrementOp(sellerID, l)
	if _, err := r.products.UpdateOne(ctx, filter, update); err != nil {
		slog.Error("inventory rollback failed", "product", l.ProductID, "variant", l.VariantID, "qty", l.Qty, "err", err)
	}
}

func decrementOp(sellerID string, l models.OrderLine) (bson.D, bson.D) {
	if l.VariantID != "" {
		return bson.D{
			{Key: "_id", Value: l.ProductID},
			{Key: "sellerId", Value: sellerID},
			{Key: "combinations", Value: bson.D{{Key: "$elemMatch", Value: bson.D{
				{Key: "id", Value: l.VariantID},
				{Key: "stock", Value: bson.D{{Key: "$gte", Value: l.Qty}}},
			}}}},
		}, bson.D{{Key: "$inc", Value: bson.D{
			{Key: "combinations.$.stock", Value: -l.Qty},
			{Key: "stock", Value: -l.Qty}, // product total = sum of variant stocks
		}}}
	}
	return bson.D{
		{Key: "_id", Value: l.ProductID},
		{Key: "sellerId", Value: sellerID},
		// A line without a variant may only take stock from a product that has
		// no variants. Without this, a seller adding variants between the
		// service's check and this update would let the product total drop while
		// every variant's own stock stayed put.
		{Key: "combinations.0", Value: bson.D{{Key: "$exists", Value: false}}},
		{Key: "stock", Value: bson.D{{Key: "$gte", Value: l.Qty}}},
	}, bson.D{{Key: "$inc", Value: bson.D{
		{Key: "stock", Value: -l.Qty},
	}}}
}

func incrementOp(sellerID string, l models.OrderLine) (bson.D, bson.D) {
	if l.VariantID != "" {
		return bson.D{
			{Key: "_id", Value: l.ProductID},
			{Key: "sellerId", Value: sellerID},
			{Key: "combinations.id", Value: l.VariantID},
		}, bson.D{{Key: "$inc", Value: bson.D{
			{Key: "combinations.$.stock", Value: l.Qty},
			{Key: "stock", Value: l.Qty},
		}}}
	}
	return bson.D{
		{Key: "_id", Value: l.ProductID},
		{Key: "sellerId", Value: sellerID},
	}, bson.D{{Key: "$inc", Value: bson.D{
		{Key: "stock", Value: l.Qty},
	}}}
}

// insufficient builds the error for a line that could not be reserved, with
// the number of units that are actually left so the buyer sees a real number.
func (r *Orders) insufficient(ctx context.Context, sellerID string, l models.OrderLine) error {
	e := &repository.InsufficientStockError{ProductID: l.ProductID, VariantID: l.VariantID, Name: l.Name}

	var p models.Product
	err := r.products.FindOne(ctx, bson.D{
		{Key: "_id", Value: l.ProductID},
		{Key: "sellerId", Value: sellerID},
	}).Decode(&p)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return e // product deleted: nothing available
		}
		return err
	}
	if l.VariantID == "" {
		if len(p.Combinations) > 0 {
			e.Available = 0 // it has variants now: this line has nothing to take from
		} else {
			e.Available = p.Stock
		}
	} else {
		for _, c := range p.Combinations {
			if c.ID == l.VariantID {
				e.Available = c.Stock
				break
			}
		}
	}
	if e.Available < 0 {
		e.Available = 0
	}
	return e
}
