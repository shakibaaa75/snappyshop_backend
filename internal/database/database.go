// Package database connects to MongoDB and creates indexes.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

const (
	UsersCollection                = "users"
	RefreshTokensCollection        = "refresh_tokens"
	StoreDesignsCollection         = "store_designs"
	StoreSlugHistoryCollection     = "store_slug_history"
	ProductsCollection             = "products"
	CategoriesCollection           = "categories"
	OrdersCollection               = "orders"
	PasswordResetsCollection       = "password_resets"
	EmailVerificationsCollection   = "email_verifications"
	AnalyticsEventsCollection      = "analytics_events"
	TrackingSettingsCollection     = "tracking_settings"
	DeliverySettingsCollection     = "delivery_settings" // per-seller delivery charges (one document per seller)
	SubscriptionsCollection        = "subscriptions"
	SubscriptionPaymentsCollection = "subscription_payments"
	PlanCatalogCollection          = "plan_catalog" // admin-edited plan prices / features (one document)
	CourierConnectionsCollection   = "courier_connections"
	ShipmentsCollection            = "shipments"

	// One-time premium features (Try the Look, ...)
	FeatureEntitlementsCollection = "feature_entitlements"
	FeaturePaymentsCollection     = "feature_payments"
	LooksCollection               = "looks" // Try the Look showcases

	// Admin / moderation collections
	AdminProfilesCollection         = "admin_profiles"
	AdminActivityLogsCollection     = "admin_activity_logs"
	SellerRestrictionsCollection    = "seller_restrictions"
	AdminNotesCollection            = "admin_notes"
	SellerVerificationsCollection   = "seller_verifications"
	SubscriptionOverridesCollection = "subscription_overrides"
	SubscriptionGrantsCollection    = "subscription_grants"
	SellerReportsCollection         = "seller_reports"
	SellerPromotionsCollection      = "seller_promotions"

	// Admin -> seller notifications
	NotificationsCollection     = "notifications"
	NotificationReadsCollection = "notification_reads"

	// Buyer account area
	BuyerProfilesCollection = "buyer_profiles"
	OrderIssuesCollection   = "order_issues"

	// Atomic counters (one document per counter; used for order numbers).
	CountersCollection = "counters"

	// Fraud check
	FraudSettingsCollection    = "fraud_settings"     // one document, _id "global"
	FraudCustomersCollection   = "fraud_customers"    // one document per phone
	FraudOrderEventsCollection = "fraud_order_events" // one document per order
	FraudChecksCollection      = "fraud_order_checks" // last check result per order

	// Custom domains
	CustomDomainsCollection = "custom_domains" // one document per seller (_id = seller ID)
)

func Connect(ctx context.Context, uri string) (*mongo.Client, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetAppName("shopbd-api").
		SetServerSelectionTimeout(5 * time.Second).
		SetConnectTimeout(10 * time.Second).
		SetMaxPoolSize(100).
		SetMinPoolSize(5).
		SetMaxConnIdleTime(5 * time.Minute).
		SetRetryWrites(true).
		SetRetryReads(true)

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("mongodb connect: %s", redact(err.Error(), uri))
	}
	pingCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongodb ping: %s", redact(err.Error(), uri))
	}
	return client, nil
}

func redact(msg, uri string) string {
	if uri == "" {
		return msg
	}
	return strings.ReplaceAll(msg, uri, "[REDACTED]")
}

func EnsureIndexes(ctx context.Context, db *mongo.Database, log *slog.Logger) error {
	specs := []struct {
		collection string
		models     []mongo.IndexModel
	}{
		{UsersCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetName("uniq_users_email").SetUnique(true)},
			// One account per Google / Apple ID. Partial, so the many users
			// without a social ID do not collide.
			{Keys: bson.D{{Key: "googleId", Value: 1}}, Options: options.Index().SetName("uniq_users_google_id").SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "googleId", Value: bson.D{{Key: "$type", Value: "string"}}}})},
			{Keys: bson.D{{Key: "appleId", Value: 1}}, Options: options.Index().SetName("uniq_users_apple_id").SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "appleId", Value: bson.D{{Key: "$type", Value: "string"}}}})},
		}},
		{RefreshTokensCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "tokenHash", Value: 1}}, Options: options.Index().SetName("uniq_refresh_tokens_hash").SetUnique(true)},
			{Keys: bson.D{{Key: "familyId", Value: 1}}, Options: options.Index().SetName("idx_refresh_tokens_family")},
			{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "revokedAt", Value: 1}}, Options: options.Index().SetName("idx_refresh_tokens_user_revoked")},
			{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetName("ttl_refresh_tokens_expires").SetExpireAfterSeconds(0)},
		}},
		{StoreDesignsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}}, Options: options.Index().SetName("uniq_store_designs_seller").SetUnique(true)},
			{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetName("uniq_store_designs_slug").SetUnique(true)},
		}},
		{StoreSlugHistoryCollection, []mongo.IndexModel{
			// A retired address can never be reused, so one seller can never
			// have the same old->new pair twice. This also makes the legacy
			// backfill idempotent.
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "oldSlug", Value: 1}, {Key: "newSlug", Value: 1}}, Options: options.Index().SetName("uniq_slug_history_pair").SetUnique(true)},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "changedAt", Value: -1}}, Options: options.Index().SetName("idx_slug_history_seller_changed")},
			{Keys: bson.D{{Key: "changedAt", Value: -1}}, Options: options.Index().SetName("idx_slug_history_changed")},
		}},
		{ProductsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "slug", Value: 1}}, Options: options.Index().SetName("uniq_products_seller_slug").SetUnique(true)},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_products_seller_created")},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "isActive", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_products_seller_active_created")},
			{Keys: bson.D{{Key: "name", Value: "text"}}, Options: options.Index().SetName("txt_products_name")},
			// Platform-wide category filters (marketplace browsing by Fashion / Hoodie).
			// Harmless until products start storing mainId / subId.
			{Keys: bson.D{{Key: "mainId", Value: 1}, {Key: "isActive", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_products_main_active_created")},
			{Keys: bson.D{{Key: "subId", Value: 1}, {Key: "isActive", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_products_sub_active_created")},
		}},
		{CategoriesCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "slug", Value: 1}}, Options: options.Index().SetName("uniq_categories_seller_slug").SetUnique(true)},
		}},
		{OrdersCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_orders_seller_created")},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_orders_seller_status_created")},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "number", Value: 1}}, Options: options.Index().SetName("uniq_orders_seller_number").SetUnique(true)},
			{Keys: bson.D{{Key: "customerPhone", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_orders_phone_created")},
			// Payment-window sweeper: finds unpaid pending orders whose
			// reservation has expired. Partial, so it only holds the few orders
			// that are still waiting for payment.
			{Keys: bson.D{{Key: "reservationExpiresAt", Value: 1}}, Options: options.Index().SetName("idx_orders_reservation_expiry").
				SetPartialFilterExpression(bson.D{
					{Key: "stockReserved", Value: true},
					{Key: "paid", Value: false},
					{Key: "status", Value: "pending"},
				})},
			// Buyer "My orders": look up by email, ignoring upper/lower case.
			{Keys: bson.D{{Key: "customerEmail", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_orders_email_created").SetCollation(&options.Collation{Locale: "en", Strength: 2})},
		}},
		{CustomDomainsCollection, []mongo.IndexModel{
			// A domain can be VERIFIED by only one seller. Pending rows are not
			// covered, so a seller who types someone else's domain first cannot
			// block the real owner from verifying it.
			{Keys: bson.D{{Key: "domain", Value: 1}}, Options: options.Index().SetName("uniq_custom_domains_verified").SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "status", Value: "verified"}})},
		}},
		{FraudCustomersCollection, []mongo.IndexModel{
			// Retention cleanup looks up customers by last activity.
			{Keys: bson.D{{Key: "lastEventAt", Value: 1}}, Options: options.Index().SetName("idx_fraud_customers_last_event")},
		}},
		{FraudChecksCollection, []mongo.IndexModel{
			// Saved check results are dropped after 180 days.
			{Keys: bson.D{{Key: "checkedAt", Value: 1}}, Options: options.Index().SetName("ttl_fraud_checks_checked").SetExpireAfterSeconds(180 * 24 * 3600)},
		}},
		{FraudOrderEventsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "updatedAt", Value: 1}}, Options: options.Index().SetName("idx_fraud_events_updated")},
		}},
		{OrderIssuesCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "buyerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_order_issues_buyer_created")},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_order_issues_seller_status")},
			// One open issue per order.
			{Keys: bson.D{{Key: "orderId", Value: 1}}, Options: options.Index().SetName("uniq_order_issues_open_order").SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "status", Value: "open"}})},
		}},
		{PasswordResetsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "userId", Value: 1}}, Options: options.Index().SetName("uniq_password_resets_user").SetUnique(true)},
			{Keys: bson.D{{Key: "codeHash", Value: 1}}, Options: options.Index().SetName("idx_password_resets_code")},
			{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetName("ttl_password_resets_expires").SetExpireAfterSeconds(0)},
		}},
		{EmailVerificationsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetName("uniq_email_verifications_email").SetUnique(true)},
			{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetName("ttl_email_verifications_expires").SetExpireAfterSeconds(0)},
		}},
		{SubscriptionsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}}, Options: options.Index().SetName("uniq_subscriptions_seller").SetUnique(true)},
		}},
		{CourierConnectionsCollection, []mongo.IndexModel{
			// One seller cannot connect the same courier account twice. The
			// fingerprint is scoped to the seller and provider, so different
			// sellers (and a seller's genuinely different accounts) never collide.
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "provider", Value: 1}, {Key: "accountFingerprint", Value: 1}}, Options: options.Index().SetName("uniq_courier_seller_provider_account").SetUnique(true)},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: 1}}, Options: options.Index().SetName("idx_courier_seller_created")},
		}},
		{ShipmentsCollection, []mongo.IndexModel{
			// One shipment per order. This unique index is what makes double-booking impossible.
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "orderId", Value: 1}}, Options: options.Index().SetName("uniq_shipments_seller_order").SetUnique(true)},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "state", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_shipments_seller_state_created")},
		}},
		{SubscriptionPaymentsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "tranId", Value: 1}}, Options: options.Index().SetName("uniq_sub_payments_tran").SetUnique(true)},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_sub_payments_seller_created")},
		}},
		{FeatureEntitlementsCollection, []mongo.IndexModel{
			// One row per seller and feature: this is what stops a double purchase
			// or a double grant from creating two rows.
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "feature", Value: 1}}, Options: options.Index().SetName("uniq_feature_entitlements_seller_feature").SetUnique(true)},
			{Keys: bson.D{{Key: "feature", Value: 1}, {Key: "status", Value: 1}}, Options: options.Index().SetName("idx_feature_entitlements_feature_status")},
		}},
		{LooksCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "updatedAt", Value: -1}}, Options: options.Index().SetName("idx_looks_seller_updated")},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "publishedAt", Value: -1}}, Options: options.Index().SetName("idx_looks_status_published")},
		}},
		{FeaturePaymentsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "tranId", Value: 1}}, Options: options.Index().SetName("uniq_feature_payments_tran").SetUnique(true)},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_feature_payments_seller_created")},
		}},
		{UsersCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "role", Value: 1}, {Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_users_role_status_created")},
			{Keys: bson.D{{Key: "role", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_users_role_created")},
		}},
		{AdminActivityLogsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_admin_logs_created")},
			{Keys: bson.D{{Key: "adminId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_admin_logs_admin_created")},
			{Keys: bson.D{{Key: "targetType", Value: 1}, {Key: "targetId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_admin_logs_target")},
		}},
		{SellerRestrictionsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_restrictions_seller_created")},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "active", Value: 1}}, Options: options.Index().SetName("idx_restrictions_seller_active")},
			{Keys: bson.D{{Key: "active", Value: 1}, {Key: "endsAt", Value: 1}}, Options: options.Index().SetName("idx_restrictions_active_ends")},
		}},
		{AdminNotesCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "targetType", Value: 1}, {Key: "targetId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_admin_notes_target")},
		}},
		{SellerVerificationsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_verifications_seller_created")},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "submittedAt", Value: -1}}, Options: options.Index().SetName("idx_verifications_status_submitted")},
		}},
		{SubscriptionOverridesCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "active", Value: 1}}, Options: options.Index().SetName("idx_sub_overrides_seller_active")},
			{Keys: bson.D{{Key: "active", Value: 1}, {Key: "endsAt", Value: 1}}, Options: options.Index().SetName("idx_sub_overrides_active_ends")},
		}},
		{SubscriptionGrantsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_sub_grants_seller_status")},
		}},
		{SellerReportsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_reports_status_created")},
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_reports_seller_created")},
			{Keys: bson.D{{Key: "productId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_reports_product_created")},
		}},
		{NotificationsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_notifications_created")},
			{Keys: bson.D{{Key: "audience", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_notifications_audience_created")},
			{Keys: bson.D{{Key: "recipientIds", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_notifications_recipient_created")},
		}},
		{NotificationReadsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "notificationId", Value: 1}}, Options: options.Index().SetName("idx_notification_reads_user_notification")},
			{Keys: bson.D{{Key: "notificationId", Value: 1}}, Options: options.Index().SetName("idx_notification_reads_notification")},
		}},
		{SellerPromotionsCollection, []mongo.IndexModel{
			{Keys: bson.D{{Key: "sellerId", Value: 1}, {Key: "active", Value: 1}}, Options: options.Index().SetName("idx_promotions_seller_active")},
			{Keys: bson.D{{Key: "kind", Value: 1}, {Key: "active", Value: 1}, {Key: "priority", Value: -1}}, Options: options.Index().SetName("idx_promotions_kind_active_priority")},
		}},
	}
	for _, s := range specs {
		if len(s.models) == 0 {
			continue
		}
		names, err := db.Collection(s.collection).Indexes().CreateMany(ctx, s.models)
		if err != nil {
			return fmt.Errorf("creating indexes on %q failed: %w", s.collection, err)
		}
		log.Info("indexes ensured", "collection", s.collection, "indexes", names)
	}
	return nil
}
