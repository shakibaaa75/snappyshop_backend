package mongorepo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Orders implements repository.OrderRepository.
//
// Inventory: placing an order takes its stock off the products, cancelling (or
// letting an unpaid order expire) puts it back, and every one of those steps
// runs inside a MongoDB transaction together with the order write. See
// inventory.go for the stock primitives.
type Orders struct {
	col      *mongo.Collection
	products *mongo.Collection
	counters *mongo.Collection
	tx       *txRunner
}

func NewOrders(db *mongo.Database) *Orders {
	return &Orders{
		col:      db.Collection(database.OrdersCollection),
		products: db.Collection(database.ProductsCollection),
		counters: db.Collection(database.CountersCollection),
		tx:       &txRunner{client: db.Client()},
	}
}

// Create reserves the stock for every line and inserts the order, atomically.
// Returns *repository.InsufficientStockError (and changes nothing) when any
// line cannot be covered, and repository.ErrDuplicateOrder (also changing
// nothing: the reservation is rolled back) when the order ID or number is
// already taken.
func (r *Orders) Create(ctx context.Context, o *models.Order) error {
	ctx, cancel := context.WithTimeout(ctx, txTimeout)
	defer cancel()

	o.StockReserved = true
	err := r.tx.run(ctx, func(sc context.Context, undo *undoLog) error {
		if err := r.reserve(sc, undo, o.SellerID, o.Lines); err != nil {
			return err
		}
		_, err := r.col.InsertOne(sc, o)
		return err
	})
	if err != nil {
		o.StockReserved = false
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrDuplicateOrder
		}
		return err
	}
	return nil
}

// NextOrderSequence is a single atomic find-and-increment on one counter
// document, so concurrent callers (on any API instance) always get different
// numbers. It deliberately runs OUTSIDE the order transaction: inside it,
// every order would conflict on the one counter document and serialize.
func (r *Orders) NextOrderSequence(ctx context.Context, counter string) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	var err error
	// The very first calls for a new counter can race on the upsert and one of
	// them gets a duplicate-key error; the retry then just increments.
	for attempt := 0; attempt < 3; attempt++ {
		err = r.counters.FindOneAndUpdate(ctx,
			bson.D{{Key: "_id", Value: counter}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}},
			opts,
		).Decode(&doc)
		if err == nil {
			return doc.Seq, nil
		}
		if !mongo.IsDuplicateKeyError(err) {
			break
		}
	}
	return 0, err
}

func (r *Orders) FindByID(ctx context.Context, sellerID, id string) (*models.Order, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var o models.Order
	err := r.col.FindOne(ctx, bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
	}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *Orders) FindByNumber(ctx context.Context, sellerID, number string) (*models.Order, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var o models.Order
	err := r.col.FindOne(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "number", Value: number},
	}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *Orders) List(ctx context.Context, sellerID string, filter repository.OrderFilter) ([]*models.Order, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := bson.D{{Key: "sellerId", Value: sellerID}}
	if filter.Status != nil {
		query = append(query, bson.E{Key: "status", Value: *filter.Status})
	}
	if !filter.Before.IsZero() {
		query = append(query, bson.E{Key: "createdAt", Value: bson.D{{Key: "$lt", Value: filter.Before}}})
	}

	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(limit))

	cur, err := r.col.Find(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []*models.Order
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateStatus changes an order's status and keeps stock in step with it, in
// one transaction:
//
//	active    -> cancelled : the reserved stock goes back to the products
//	cancelled -> active    : the stock is taken again (fails with
//	                         *InsufficientStockError if it is gone)
//	anything else          : status only
//
// The order write is guarded on the status/reservation we read, so two
// simultaneous cancels can never both restock.
func (r *Orders) UpdateStatus(ctx context.Context, sellerID, id string, status models.OrderStatus, note string, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, txTimeout)
	defer cancel()

	return r.tx.run(ctx, func(sc context.Context, undo *undoLog) error {
		var cur models.Order
		err := r.col.FindOne(sc, bson.D{
			{Key: "_id", Value: id},
			{Key: "sellerId", Value: sellerID},
		}).Decode(&cur)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return repository.ErrNotFound
		}
		if err != nil {
			return err
		}

		toCancelled := status == models.OrderCancelled
		wasCancelled := cur.Status == models.OrderCancelled
		releasing := toCancelled && !wasCancelled && cur.StockReserved
		reserving := !toCancelled && wasCancelled && !cur.StockReserved

		set := bson.D{
			{Key: "status", Value: status},
			{Key: "updatedAt", Value: at},
		}
		update := bson.D{
			{Key: "$push", Value: bson.D{{Key: "events", Value: models.OrderStatusEvent{Status: status, At: at, Note: note}}}},
		}
		var unset bson.D
		if status != models.OrderDelivered {
			unset = append(unset, bson.E{Key: "reviewReceipt", Value: ""})
		}
		switch {
		case releasing:
			set = append(set, bson.E{Key: "stockReserved", Value: false})
			unset = append(unset, bson.E{Key: "reservationExpiresAt", Value: ""})
		case reserving:
			set = append(set, bson.E{Key: "stockReserved", Value: true})
		}
		update = append(bson.D{{Key: "$set", Value: set}}, update...)
		if len(unset) > 0 {
			update = append(update, bson.E{Key: "$unset", Value: unset})
		}

		// Guard: only apply if nobody changed the order since we read it.
		guard := bson.D{
			{Key: "_id", Value: id},
			{Key: "sellerId", Value: sellerID},
			{Key: "status", Value: cur.Status},
			{Key: "stockReserved", Value: cur.StockReserved},
		}
		if !cur.StockReserved {
			// Legacy orders have no stockReserved field at all.
			guard = bson.D{
				{Key: "_id", Value: id},
				{Key: "sellerId", Value: sellerID},
				{Key: "status", Value: cur.Status},
				{Key: "stockReserved", Value: bson.D{{Key: "$ne", Value: true}}},
			}
		}

		switch {
		case reserving:
			// Take the stock first; if it is not there the order stays
			// cancelled. (Without transactions a failed order write below
			// gives the stock back through the undo log.)
			if err := r.reserve(sc, undo, sellerID, cur.Lines); err != nil {
				return err
			}
			return r.applyGuarded(sc, guard, update)
		case releasing:
			// Flip the order first: only the caller that wins this guarded
			// update restocks, so a double cancel cannot restock twice.
			if err := r.applyGuarded(sc, guard, update); err != nil {
				return err
			}
			return r.release(sc, sellerID, cur.Lines)
		default:
			return r.applyGuarded(sc, guard, update)
		}
	})
}

func (r *Orders) applyGuarded(ctx context.Context, guard, update bson.D) error {
	res, err := r.col.UpdateOne(ctx, guard, update)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrOrderChanged
	}
	return nil
}

// errNothingToExpire is returned inside the expiry transaction when another
// worker already handled the order. It is not a failure.
var errNothingToExpire = errors.New("mongorepo: order no longer expirable")

// ExpireUnpaid cancels pending, unpaid orders whose payment window has
// passed and gives their stock back. Each order is handled in its own
// transaction, and the guarded update means that if several API instances run
// this at once, every order is released exactly once.
func (r *Orders) ExpireUnpaid(ctx context.Context, now time.Time, limit int) ([]*models.Order, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	qctx, cancel := withTimeout(ctx)
	cur, err := r.col.Find(qctx, expirableFilter(now), options.Find().
		SetSort(bson.D{{Key: "reservationExpiresAt", Value: 1}}).
		SetLimit(int64(limit)).
		SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "sellerId", Value: 1}}))
	if err != nil {
		cancel()
		return nil, err
	}
	var candidates []struct {
		ID       string `bson:"_id"`
		SellerID string `bson:"sellerId"`
	}
	err = cur.All(qctx, &candidates)
	cancel()
	if err != nil {
		return nil, err
	}

	var expired []*models.Order
	for _, c := range candidates {
		o, err := r.expireOne(ctx, c.SellerID, c.ID, now)
		if err != nil {
			if errors.Is(err, errNothingToExpire) {
				continue
			}
			return expired, err
		}
		expired = append(expired, o)
	}
	return expired, nil
}

func expirableFilter(now time.Time) bson.D {
	return bson.D{
		{Key: "status", Value: models.OrderPending},
		{Key: "paid", Value: false},
		{Key: "stockReserved", Value: true},
		{Key: "reservationExpiresAt", Value: bson.D{{Key: "$lte", Value: now}}},
	}
}

func (r *Orders) expireOne(ctx context.Context, sellerID, id string, now time.Time) (*models.Order, error) {
	ctx, cancel := context.WithTimeout(ctx, txTimeout)
	defer cancel()

	var before models.Order
	err := r.tx.run(ctx, func(sc context.Context, _ *undoLog) error {
		before = models.Order{} // the driver may run this callback again
		filter := append(expirableFilter(now),
			bson.E{Key: "_id", Value: id},
			bson.E{Key: "sellerId", Value: sellerID},
		)
		err := r.col.FindOneAndUpdate(sc, filter, bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "status", Value: models.OrderCancelled},
				{Key: "stockReserved", Value: false},
				{Key: "updatedAt", Value: now},
			}},
			{Key: "$unset", Value: bson.D{{Key: "reservationExpiresAt", Value: ""}}},
			{Key: "$push", Value: bson.D{{Key: "events", Value: models.OrderStatusEvent{
				Status: models.OrderCancelled,
				At:     now,
				Note:   "Payment was not received in time - order cancelled automatically and stock released",
			}}}},
		}, options.FindOneAndUpdate().SetReturnDocument(options.Before)).Decode(&before)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return errNothingToExpire
		}
		if err != nil {
			return err
		}
		return r.release(sc, sellerID, before.Lines)
	})
	if err != nil {
		return nil, err
	}
	before.Status = models.OrderCancelled
	before.StockReserved = false
	before.ReservationExpiresAt = nil
	before.UpdatedAt = now
	return &before, nil
}

func (r *Orders) MarkPaid(ctx context.Context, sellerID, id string, paid bool) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "paid", Value: paid},
		{Key: "updatedAt", Value: time.Now().UTC()},
	}}}
	if !paid {
		update = append(update, bson.E{Key: "$unset", Value: bson.M{"reviewReceipt": "", "reviewGatewayVerified": ""}})
	}
	update = append(update, bson.E{Key: "$push", Value: bson.M{"reviewEvidence": bson.M{"at": time.Now().UTC(), "actor": sellerID, "action": "payment status", "paid": paid}}})
	if paid {
		// Paid in time: the payment deadline no longer applies.
		update = append(update, bson.E{Key: "$unset", Value: bson.D{{Key: "reservationExpiresAt", Value: ""}}})
	}
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		update,
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// Stats returns aggregated counts and revenue for a seller since a given time.
func (r *Orders) Stats(ctx context.Context, sellerID string, since time.Time) (*repository.OrderStats, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	match := bson.D{{Key: "sellerId", Value: sellerID}}
	if !since.IsZero() {
		match = append(match, bson.E{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}})
	}

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "orderCount", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "paidOrderCount", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{"$paid", 1, 0}}}}}},
			{Key: "revenue", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{"$paid", "$total", 0}}}}}},
			{Key: "pendingCount", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$eq", Value: bson.A{"$status", "pending"}}}, 1, 0}}}}}},
			{Key: "confirmedCount", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$eq", Value: bson.A{"$status", "confirmed"}}}, 1, 0}}}}}},
			{Key: "shippedCount", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$eq", Value: bson.A{"$status", "shipped"}}}, 1, 0}}}}}},
			{Key: "deliveredCount", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$eq", Value: bson.A{"$status", "delivered"}}}, 1, 0}}}}}},
			{Key: "cancelledCount", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$eq", Value: bson.A{"$status", "cancelled"}}}, 1, 0}}}}}},
			{Key: "unitsSold", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$sum", Value: "$lines.qty"}}}}},
		}}},
	}

	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var raw struct {
		OrderCount     int     `bson:"orderCount"`
		PaidOrderCount int     `bson:"paidOrderCount"`
		Revenue        float64 `bson:"revenue"`
		PendingCount   int     `bson:"pendingCount"`
		ConfirmedCount int     `bson:"confirmedCount"`
		ShippedCount   int     `bson:"shippedCount"`
		DeliveredCount int     `bson:"deliveredCount"`
		CancelledCount int     `bson:"cancelledCount"`
		UnitsSold      int     `bson:"unitsSold"`
	}
	if cur.Next(ctx) {
		if err := cur.Decode(&raw); err != nil {
			return nil, err
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}

	stats := &repository.OrderStats{
		OrderCount:     raw.OrderCount,
		PaidOrderCount: raw.PaidOrderCount,
		Revenue:        raw.Revenue,
		PendingCount:   raw.PendingCount,
		ConfirmedCount: raw.ConfirmedCount,
		ShippedCount:   raw.ShippedCount,
		DeliveredCount: raw.DeliveredCount,
		CancelledCount: raw.CancelledCount,
		UnitsSold:      raw.UnitsSold,
	}
	if raw.OrderCount > 0 {
		stats.AvgOrderValue = raw.Revenue / float64(raw.OrderCount)
	}
	return stats, nil
}
