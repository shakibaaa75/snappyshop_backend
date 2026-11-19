package mongorepo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type AdminStats struct {
	db *mongo.Database
}

func NewAdminStats(db *mongo.Database) *AdminStats {
	return &AdminStats{db: db}
}

func (r *AdminStats) DashboardStats(ctx context.Context) (*repository.DashboardStats, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	users := r.db.Collection(database.UsersCollection)
	products := r.db.Collection(database.ProductsCollection)
	orders := r.db.Collection(database.OrdersCollection)
	subs := r.db.Collection(database.SubscriptionsCollection)
	payments := r.db.Collection(database.SubscriptionPaymentsCollection)
	verifs := r.db.Collection(database.SellerVerificationsCollection)
	reports := r.db.Collection(database.SellerReportsCollection)
	s := &repository.DashboardStats{}
	var err error
	if s.TotalUsers, err = users.CountDocuments(ctx, bson.D{}); err != nil {
		return nil, err
	}
	if s.TotalSellers, err = users.CountDocuments(ctx, bson.D{{Key: "role", Value: models.RoleSeller}}); err != nil {
		return nil, err
	}
	if s.ActiveSellers, err = users.CountDocuments(ctx, bson.D{{Key: "role", Value: models.RoleSeller}, {Key: "status", Value: models.StatusActive}}); err != nil {
		return nil, err
	}
	if s.SuspendedSellers, err = users.CountDocuments(ctx, bson.D{{Key: "role", Value: models.RoleSeller}, {Key: "status", Value: models.StatusSuspended}}); err != nil {
		return nil, err
	}
	if s.RestrictedSellers, err = users.CountDocuments(ctx, bson.D{{Key: "role", Value: models.RoleSeller}, {Key: "status", Value: models.StatusRestricted}}); err != nil {
		return nil, err
	}
	if s.BannedSellers, err = users.CountDocuments(ctx, bson.D{{Key: "role", Value: models.RoleSeller}, {Key: "status", Value: models.StatusBanned}}); err != nil {
		return nil, err
	}
	if s.TotalCustomers, err = users.CountDocuments(ctx, bson.D{{Key: "role", Value: models.RoleBuyer}}); err != nil {
		return nil, err
	}
	if s.PublishedProducts, err = products.CountDocuments(ctx, bson.D{{Key: "isActive", Value: true}}); err != nil {
		return nil, err
	}
	if s.InactiveProducts, err = products.CountDocuments(ctx, bson.D{{Key: "isActive", Value: false}}); err != nil {
		return nil, err
	}
	if s.TotalOrders, err = orders.CountDocuments(ctx, bson.D{}); err != nil {
		return nil, err
	}
	if s.DeliveredOrders, err = orders.CountDocuments(ctx, bson.D{{Key: "status", Value: models.OrderDelivered}}); err != nil {
		return nil, err
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "status", Value: models.SubPayPaid}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "total", Value: bson.D{{Key: "$sum", Value: "$amount"}}}}}},
	}
	cur, err := payments.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	if cur.Next(ctx) {
		var row struct {
			Total int64 `bson:"total"`
		}
		if err := cur.Decode(&row); err == nil {
			s.SubscriptionRevenueTaka = row.Total
		}
	}
	_ = cur.Close(ctx)
	now := time.Now().UTC()
	live := bson.D{
		{Key: "currentPeriodEnd", Value: bson.D{{Key: "$gt", Value: now}}},
		{Key: "plan", Value: bson.D{{Key: "$ne", Value: "free"}}},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: models.SubStatusInactive}}},
	}
	if s.ActivePaidSubscriptions, err = subs.CountDocuments(ctx, append(append(bson.D{}, live...),
		bson.E{Key: "source", Value: bson.D{{Key: "$ne", Value: models.SubSourceAdminFree}}})); err != nil {
		return nil, err
	}
	if s.ActivePromoSubscriptions, err = subs.CountDocuments(ctx, append(append(bson.D{}, live...),
		bson.E{Key: "source", Value: models.SubSourceAdminFree})); err != nil {
		return nil, err
	}
	if s.PendingVerifications, err = verifs.CountDocuments(ctx, bson.D{{Key: "status", Value: models.VerificationPending}}); err != nil {
		return nil, err
	}
	if s.OpenReports, err = reports.CountDocuments(ctx, bson.D{
		{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{models.ReportStatusResolved, models.ReportStatusDismissed}}}},
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *AdminStats) ProductCountsGlobal(ctx context.Context) (published, inactive int64, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	col := r.db.Collection(database.ProductsCollection)
	published, err = col.CountDocuments(ctx, bson.D{{Key: "isActive", Value: true}})
	if err != nil {
		return
	}
	inactive, err = col.CountDocuments(ctx, bson.D{{Key: "isActive", Value: false}})
	return
}

func (r *AdminStats) OrderCountsGlobal(ctx context.Context) (total, delivered int64, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	col := r.db.Collection(database.OrdersCollection)
	total, err = col.CountDocuments(ctx, bson.D{})
	if err != nil {
		return
	}
	delivered, err = col.CountDocuments(ctx, bson.D{{Key: "status", Value: models.OrderDelivered}})
	return
}

func (r *AdminStats) SubscriptionRevenue(ctx context.Context) (paidTaka int64, activePaid, activeFree int64, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	payments := r.db.Collection(database.SubscriptionPaymentsCollection)
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "status", Value: models.SubPayPaid}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "total", Value: bson.D{{Key: "$sum", Value: "$amount"}}}}}},
	}
	cur, err := payments.Aggregate(ctx, pipeline)
	if err != nil {
		return
	}
	if cur.Next(ctx) {
		var row struct {
			Total int64 `bson:"total"`
		}
		_ = cur.Decode(&row)
		paidTaka = row.Total
	}
	_ = cur.Close(ctx)
	now := time.Now().UTC()
	subs := r.db.Collection(database.SubscriptionsCollection)
	// Admin-granted free plans are counted apart from paying sellers.
	// Subscriptions an admin switched off are not active.
	notOff := bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: models.SubStatusInactive}}}}
	live := bson.D{
		{Key: "currentPeriodEnd", Value: bson.D{{Key: "$gt", Value: now}}},
		{Key: "plan", Value: bson.D{{Key: "$ne", Value: "free"}}},
		notOff[0],
	}
	activePaid, err = subs.CountDocuments(ctx, append(append(bson.D{}, live...),
		bson.E{Key: "source", Value: bson.D{{Key: "$ne", Value: models.SubSourceAdminFree}}}))
	if err != nil {
		return
	}
	activeFree, err = subs.CountDocuments(ctx, append(append(bson.D{}, live...),
		bson.E{Key: "source", Value: models.SubSourceAdminFree}))
	return
}

func (r *AdminStats) SellerSummary(ctx context.Context, sellerID string) (*repository.SellerSummary, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	sum := &repository.SellerSummary{SellerID: sellerID}
	products := r.db.Collection(database.ProductsCollection)
	orders := r.db.Collection(database.OrdersCollection)
	stores := r.db.Collection(database.StoreDesignsCollection)
	subs := r.db.Collection(database.SubscriptionsCollection)
	verifs := r.db.Collection(database.SellerVerificationsCollection)
	restrictions := r.db.Collection(database.SellerRestrictionsCollection)
	var err error
	if sum.TotalProducts, err = products.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}}); err != nil {
		return nil, err
	}
	if sum.PublishedProducts, err = products.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}, {Key: "isActive", Value: true}}); err != nil {
		return nil, err
	}
	if sum.TotalOrders, err = orders.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}}); err != nil {
		return nil, err
	}
	if sum.DeliveredOrders, err = orders.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}, {Key: "status", Value: models.OrderDelivered}}); err != nil {
		return nil, err
	}
	if sum.CancelledOrders, err = orders.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}, {Key: "status", Value: models.OrderCancelled}}); err != nil {
		return nil, err
	}
	var store models.StoreDesign
	// Latest document wins, so the summary always shows the current slug.
	if err := stores.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}},
		options.FindOne().SetSort(bson.D{{Key: "updatedAt", Value: -1}}),
	).Decode(&store); err == nil {
		sum.StoreSlug = store.Slug
		sum.StoreName = store.StoreName
	}
	now := time.Now().UTC()
	// The subscription row is the single source of truth (admin grants write
	// to it too), so this always matches what the seller can actually use.
	var sub models.Subscription
	if err := subs.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}}).Decode(&sub); err == nil {
		if sub.IsActive(now) {
			sum.Plan = sub.Plan
			end := sub.CurrentPeriodEnd
			sum.PlanExpiresAt = &end
			sum.IsPromotionalPlan = sub.Source == models.SubSourceAdminFree
		}
	}
	if sum.Plan == "" {
		sum.Plan = "free"
	}
	var ver models.SellerVerification
	if err := verifs.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}},
		options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&ver); err == nil {
		sum.VerificationStatus = ver.Status
	}
	var rest models.SellerRestriction
	if err := restrictions.FindOne(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "active", Value: true},
		{Key: "kind", Value: bson.D{{Key: "$ne", Value: models.RestrictionRestore}}},
	}, options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&rest); err == nil {
		sum.RestrictionKind = rest.Kind
	}
	return sum, nil
}

func (r *AdminStats) ListSellersEnriched(ctx context.Context, filter repository.UserListFilter) ([]*repository.SellerListItem, int64, error) {
	usersRepo := NewUsers(r.db)
	filter.Role = models.RoleSeller
	users, total, err := usersRepo.ListByRole(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	if len(users) == 0 {
		return nil, total, nil
	}

	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	// Batch store designs
	storeBySeller := map[string]models.StoreDesign{}
	// One entry per seller, always the most recently updated document (the
	// current slug): oldest first, so later documents overwrite earlier ones.
	cur, err := r.db.Collection(database.StoreDesignsCollection).Find(ctx, bson.D{
		{Key: "sellerId", Value: bson.D{{Key: "$in", Value: ids}}},
	}, options.Find().
		SetSort(bson.D{{Key: "updatedAt", Value: 1}}).
		SetProjection(bson.D{{Key: "sellerId", Value: 1}, {Key: "slug", Value: 1}, {Key: "storeName", Value: 1}, {Key: "draft.storeName", Value: 1}}))
	if err == nil {
		for cur.Next(ctx) {
			var sd models.StoreDesign
			if cur.Decode(&sd) == nil {
				storeBySeller[sd.SellerID] = sd
			}
		}
		_ = cur.Close(ctx)
	}

	// Batch product counts
	type idCount struct {
		ID    string `bson:"_id"`
		Total int64  `bson:"total"`
		Pub   int64  `bson:"pub"`
	}
	prodCounts := map[string]idCount{}
	pc, err := r.db.Collection(database.ProductsCollection).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "sellerId", Value: bson.D{{Key: "$in", Value: ids}}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$sellerId"},
			{Key: "total", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "pub", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{"$isActive", 1, 0}}}}}},
		}}},
	})
	if err == nil {
		for pc.Next(ctx) {
			var row idCount
			if pc.Decode(&row) == nil {
				prodCounts[row.ID] = row
			}
		}
		_ = pc.Close(ctx)
	}

	// Batch order counts
	type ordCount struct {
		ID        string `bson:"_id"`
		Total     int64  `bson:"total"`
		Delivered int64  `bson:"delivered"`
	}
	ordCounts := map[string]ordCount{}
	oc, err := r.db.Collection(database.OrdersCollection).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "sellerId", Value: bson.D{{Key: "$in", Value: ids}}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$sellerId"},
			{Key: "total", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "delivered", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{
				bson.D{{Key: "$eq", Value: bson.A{"$status", models.OrderDelivered}}}, 1, 0,
			}}}}}},
		}}},
	})
	if err == nil {
		for oc.Next(ctx) {
			var row ordCount
			if oc.Decode(&row) == nil {
				ordCounts[row.ID] = row
			}
		}
		_ = oc.Close(ctx)
	}

	// Batch active subscriptions
	now := time.Now().UTC()
	subBySeller := map[string]models.Subscription{}
	sc, err := r.db.Collection(database.SubscriptionsCollection).Find(ctx, bson.D{
		{Key: "sellerId", Value: bson.D{{Key: "$in", Value: ids}}},
		{Key: "currentPeriodEnd", Value: bson.D{{Key: "$gt", Value: now}}},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: models.SubStatusInactive}}},
	})
	if err == nil {
		for sc.Next(ctx) {
			var sub models.Subscription
			if sc.Decode(&sub) == nil {
				subBySeller[sub.SellerID] = sub
			}
		}
		_ = sc.Close(ctx)
	}

	out := make([]*repository.SellerListItem, 0, len(users))
	for _, u := range users {
		item := &repository.SellerListItem{User: u, Plan: "free"}
		if sd, ok := storeBySeller[u.ID]; ok {
			item.StoreSlug = sd.Slug
			item.StoreName = sd.StoreName
		}
		if pc, ok := prodCounts[u.ID]; ok {
			item.TotalProducts = pc.Total
			item.PublishedProducts = pc.Pub
		}
		if oc, ok := ordCounts[u.ID]; ok {
			item.TotalOrders = oc.Total
			item.DeliveredOrders = oc.Delivered
		}
		if sub, ok := subBySeller[u.ID]; ok {
			item.Plan = sub.Plan
			end := sub.CurrentPeriodEnd
			item.PlanExpiresAt = &end
		}
		out = append(out, item)
	}
	return out, total, nil
}

func (r *AdminStats) RecentSellers(ctx context.Context, limit int) ([]*models.User, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	users, _, err := NewUsers(r.db).ListByRole(ctx, repository.UserListFilter{
		Role: models.RoleSeller, Limit: limit, SortNewest: true,
	})
	return users, err
}

func (r *AdminStats) RecentReports(ctx context.Context, limit int) ([]*models.SellerReport, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	items, _, err := NewSellerReports(r.db).List(ctx, repository.ReportListFilter{Limit: limit})
	return items, err
}

func (r *AdminStats) RecentAdminLogs(ctx context.Context, limit int) ([]*models.AdminActivityLog, error) {
	if limit <= 0 || limit > 50 {
		limit = 15
	}
	items, _, err := NewAdminActivityLogs(r.db).List(ctx, repository.AdminLogFilter{Limit: limit})
	return items, err
}

var _ repository.AdminStatsRepository = (*AdminStats)(nil)
