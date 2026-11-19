package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"shopbd/backend/internal/config"
	"shopbd/backend/internal/courier"
	"shopbd/backend/internal/courierconn"
	"shopbd/backend/internal/customdomain"
	"shopbd/backend/internal/database"
	"shopbd/backend/internal/jobs"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/oauth"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/payment/sslcommerz"
	"shopbd/backend/internal/ratelimit"
	"shopbd/backend/internal/repository/mongorepo"
	"shopbd/backend/internal/router"
	"shopbd/backend/internal/secretbox"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/shipping"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

type mongoPinger struct{ client *mongo.Client }

func (p mongoPinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx, readpref.Primary())
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// splitList turns "a, b ,c" into ["a" "b" "c"], dropping blanks.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// platformHosts lists the platform's own hostnames (the frontend, every CORS
// origin, and PLATFORM_DOMAINS). Sellers cannot claim these or anything under
// them as a custom domain. A "www." host also protects the bare domain.
func platformHosts(frontendURL string, origins []string, extra string) []string {
	var hosts []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return
		}
		hosts = append(hosts, h)
		if bare := strings.TrimPrefix(h, "www."); bare != h {
			hosts = append(hosts, bare)
		}
	}
	for _, raw := range append([]string{frontendURL}, origins...) {
		if u, err := url.Parse(raw); err == nil {
			add(u.Hostname())
		}
	}
	for _, h := range splitList(extra) {
		add(h)
	}
	return hosts
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	_ = godotenv.Load()

	cfg, err := config.FromLookup(os.LookupEnv)
	if err != nil {
		return err
	}

	var handlerOpts = &slog.HandlerOptions{Level: slog.LevelInfo}
	var logHandler slog.Handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	if !cfg.IsProduction() {
		logHandler = slog.NewTextHandler(os.Stdout, handlerOpts)
	}
	log := slog.New(logHandler)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := database.Connect(ctx, cfg.MongoURI)
	if err != nil {
		return err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(dctx)
	}()
	db := client.Database(cfg.MongoDB)
	log.Info("connected to MongoDB", "database", cfg.MongoDB)

	// Shared analytics repo: used for the index below and for the service.
	analyticsRepo := mongorepo.NewAnalytics(db)

	if cfg.AutoCreateIndexes {
		// Includes the subscriptions and subscription_payments indexes.
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := database.EnsureIndexes(ictx, db, log)
		cancel()
		if err != nil {
			return err
		}

		// Analytics dedupe index (unique, partial on "dedupe"). Not fatal:
		// if old duplicate rows block the build, the server still starts and
		// Insert's upsert keeps counting correct. Clear the old rows with
		//   db.analytics_events.deleteMany({kind:{$in:["product_view","add_to_cart"]}})
		// and restart to build the index.
		actx, acancel := context.WithTimeout(ctx, 30*time.Second)
		if err := analyticsRepo.EnsureIndexes(actx); err != nil {
			log.Error("analytics index not created", "error", err.Error())
		} else {
			log.Info("analytics index ready", "index", "uniq_dedupe")
		}
		acancel()
	} else {
		log.Info("AUTO_CREATE_INDEXES=false: skipping index creation")
	}

	authSvc, err := service.NewAuthService(
		mongorepo.NewUsers(db),
		mongorepo.NewTokens(db),
		password.New(password.DefaultParams),
		service.AuthConfig{JWTSecret: cfg.JWTSecret, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL},
	)
	if err != nil {
		return err
	}

	// One SMTP config, shared by password reset and order emails.
	smtpCfg := mailer.Config{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword,
		From:     cfg.SMTPFrom,
	}

	storeSvc := service.NewStoreDesignService(mongorepo.NewStoreDesigns(db))
	productSvc := service.NewProductService(mongorepo.NewProducts(db))

	// NOTE: the platform category tree is NOT wired here any more. The router
	// loads it from MongoDB (loadTaxonomy in router/taxonomy.go) and hands it to
	// the product service itself, so products get the stored tree and the full
	// auto-create flow.

	categorySvc := service.NewCategoryService(mongorepo.NewCategories(db))
	orderSvc := service.NewOrderService(
		mongorepo.NewOrders(db),
		mongorepo.NewProducts(db),
		mongorepo.NewStoreDesigns(db),
	)
	sender := mailer.NewSender(smtpCfg)
	orderSvc.SetMailer(sender)

	// Unpaid online-payment orders (bKash / Nagad / Rocket / card) keep their
	// stock reserved for this long, then are cancelled and the stock is
	// released. ORDER_PAYMENT_WINDOW takes a Go duration ("45m", "24h");
	// "0" turns automatic expiry off. Cash-on-delivery orders never expire.
	if v := strings.TrimSpace(os.Getenv("ORDER_PAYMENT_WINDOW")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("ORDER_PAYMENT_WINDOW %q is not a valid duration (try 30m or 24h): %w", v, err)
		}
		orderSvc.SetPaymentWindow(d)
	}
	go orderSvc.RunReservationSweeper(ctx, time.Minute)

	// Brand prefix of readable order numbers: SNAPPY-20261115-000123.
	if v := strings.TrimSpace(os.Getenv("ORDER_NUMBER_PREFIX")); v != "" {
		orderSvc.SetOrderNumberPrefix(v)
	}

	analyticsSvc := service.NewAnalyticsService(
		analyticsRepo,
		mongorepo.NewProducts(db),
		mongorepo.NewOrders(db),
		mongorepo.NewStoreDesigns(db),
	)

	trackingSvc := service.NewTrackingService(
		mongorepo.NewTrackingSettings(db),
		mongorepo.NewStoreDesigns(db),
	)

	// Seller delivery charges (Inside / Outside Dhaka, global + per product).
	deliverySvc := service.NewDeliveryService(
		mongorepo.NewDeliverySettings(db),
		mongorepo.NewProducts(db),
		mongorepo.NewStoreDesigns(db),
	)
	// Orders and checkout quotes price delivery from those settings.
	orderSvc.SetDeliveryQuoter(deliverySvc)

	// Fraud history: records each order's outcome per customer phone while the
	// admin has the fraud feature switched on. Does nothing while it is off.
	fraudHistory := service.NewFraudHistory(mongorepo.NewFraudSettings(db), mongorepo.NewFraudHistory(db), log)
	orderSvc.SetOrderHistory(fraudHistory)
	go fraudHistory.RunRetention(ctx, 6*time.Hour)

	/* ---- subscriptions + payment gateway ---- */
	gwStoreID := strings.TrimSpace(os.Getenv("SSLCOMMERZ_STORE_ID"))
	gwStorePass := strings.TrimSpace(os.Getenv("SSLCOMMERZ_STORE_PASSWORD"))
	gwSandbox := os.Getenv("SSLCOMMERZ_SANDBOX") != "false" // sandbox unless explicitly "false"
	if gwStoreID == "" || gwStorePass == "" {
		log.Warn("SSLCOMMERZ_STORE_ID / SSLCOMMERZ_STORE_PASSWORD not set: subscription checkout will fail until they are")
	}
	log.Info("payment gateway", "provider", "sslcommerz", "sandbox", gwSandbox)

	frontendURL := envOr("FRONTEND_URL", "http://localhost:3000")
	subscriptionSvc := service.NewSubscriptionService(
		mongorepo.NewSubscriptions(db),
		mongorepo.NewSubscriptionPayments(db),
		sslcommerz.New(sslcommerz.Config{
			StoreID:       gwStoreID,
			StorePassword: gwStorePass,
			Sandbox:       gwSandbox,
		}),
		service.SubscriptionConfig{
			PublicAPIURL: envOr("PUBLIC_API_URL", "http://localhost:"+cfg.Port),
			FrontendURL:  frontendURL,
		},
		log,
	)

	// Plan prices, limits and feature switches are editable by an admin
	// (Admin > Plans). The saved catalog is loaded into the plans package that
	// every plan check and price quote reads. Until an admin saves one, the
	// built-in plans apply. A failed load is not fatal: the built-in plans stay
	// and the refresh below retries.
	planCatalogSvc := service.NewPlanCatalogService(
		mongorepo.NewPlanCatalog(db),
		mongorepo.NewAdminActivityLogs(db),
		log,
	)
	if err := planCatalogSvc.Load(ctx); err != nil {
		log.Error("could not load the saved plan catalog; built-in plans are active until it loads", "error", err.Error())
	}
	// Picks up changes saved through another server instance.
	go planCatalogSvc.Run(ctx, 30*time.Second)

	// Plan limits are OFF until you set ENFORCE_PLAN_LIMITS=true. While every
	// seller is on Free, turning it on would cap products at 50 and block the
	// analytics page.
	enforcePlans := os.Getenv("ENFORCE_PLAN_LIMITS") == "true"
	log.Info("plan limits", "analytics gate enforced", enforcePlans)

	// Free-plan rules are ALWAYS on and checked on the server:
	// product cap, theme choice, Meta Pixel, Pyxel, automatic courier.
	productSvc.SetPlanLimits(subscriptionSvc, mongorepo.NewProductCounter(db))
	storeSvc.SetDesignGate(subscriptionSvc)
	trackingSvc.SetGate(subscriptionSvc)

	// Custom domains (Business plan): a seller points their own domain at the
	// store, proves ownership with a DNS TXT record, and the store then serves
	// on it. Which names sellers should point their DNS at is set here.
	cdChecker := customdomain.NewChecker(
		envOr("CUSTOM_DOMAIN_CNAME_TARGET", ""),
		splitList(os.Getenv("CUSTOM_DOMAIN_IPS")),
	)
	// Look records up on a public resolver so a stale local cache cannot hide
	// a record the seller just added. CUSTOM_DOMAIN_DNS_SERVER=system turns
	// this off.
	cdChecker.Resolver = customdomain.PublicResolver(envOr("CUSTOM_DOMAIN_DNS_SERVER", "1.1.1.1:53"))
	var cdVerifier interface {
		Check(ctx context.Context, domain, token string) customdomain.Result
	} = cdChecker
	// LOCAL TESTING ONLY: skips the DNS check so a domain mapped in your hosts
	// file can be "verified". Ignored in production.
	cdDev := os.Getenv("CUSTOM_DOMAIN_DEV") == "true" && !cfg.IsProduction()
	if cdDev {
		cdVerifier = customdomain.DevChecker{}
		log.Warn("custom domains: CUSTOM_DOMAIN_DEV is on, DNS is NOT checked. Local testing only.")
	}
	customDomainSvc := service.NewCustomDomainService(
		mongorepo.NewCustomDomains(db),
		storeSvc,
		cdVerifier,
		service.CustomDomainConfig{
			Dev:           cdDev,
			PlatformHosts: platformHosts(frontendURL, cfg.CORSOrigins, os.Getenv("PLATFORM_DOMAINS")),
			CNAMETarget:   envOr("CUSTOM_DOMAIN_CNAME_TARGET", ""),
			IPs:           splitList(os.Getenv("CUSTOM_DOMAIN_IPS")),
			AskToken:      os.Getenv("CUSTOM_DOMAIN_ASK_TOKEN"),
		},
	)
	customDomainSvc.SetGate(subscriptionSvc)
	if cfg.IsProduction() {
		if os.Getenv("CUSTOM_DOMAIN_CNAME_TARGET") == "" && os.Getenv("CUSTOM_DOMAIN_IPS") == "" {
			log.Warn("custom domains: CUSTOM_DOMAIN_CNAME_TARGET and CUSTOM_DOMAIN_IPS are not set, so sellers are not shown where to point their DNS")
		}
		if os.Getenv("CUSTOM_DOMAIN_ASK_TOKEN") == "" {
			return errors.New("CUSTOM_DOMAIN_ASK_TOKEN is required when APP_ENV=production (use a long random value, for example: openssl rand -hex 32)")
		}
	}

	// Sign-up email verification: a code is emailed first, and registration
	// only succeeds with the token that proves the code was entered.
	verifySvc := service.NewEmailVerificationService(
		mongorepo.NewUsers(db),
		mongorepo.NewEmailVerifications(db),
		sender,
		cfg.JWTSecret,
	)
	if cfg.SMTPHost == "" && !cfg.IsProduction() {
		// No SMTP in development: print the code in the server log instead.
		verifySvc.LogCodesTo(log)
	}
	authSvc.SetEmailVerifier(verifySvc)

	resetSvc := service.NewPasswordResetService(
		mongorepo.NewUsers(db),
		mongorepo.NewPasswordResets(db),
		password.New(password.DefaultParams),
		sender,
		cfg.JWTSecret,
	)
	resetSvc.SetTokenRepo(mongorepo.NewTokens(db))

	// Background jobs: password reset lookups and emails run here, off the
	// request path, so a request takes the same time for every address.
	mailQueue := jobs.New(4, 256, log)
	resetSvc.SetQueue(mailQueue, log)
	if cfg.SMTPHost == "" && !cfg.IsProduction() {
		// No SMTP in development: print the code in the server log instead.
		resetSvc.LogCodesTo(log)
	}

	// Shared rate limiting for the password reset endpoints. Without REDIS_URL
	// only the per-process limiter runs, which is weaker once there is more
	// than one API instance.
	var resetLimiter ratelimit.Limiter
	if cfg.RedisURL != "" {
		rl, err := ratelimit.NewRedis(ctx, cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		defer rl.Close()
		resetLimiter = rl
		log.Info("password reset rate limiting: redis")
	} else if cfg.IsProduction() {
		log.Warn("REDIS_URL not set: password reset rate limits are per instance only")
	}

	// Courier integrations. The feature is off until COURIER_ENCRYPTION_KEY and
	// COURIER_FINGERPRINT_KEY are set; a half-set or invalid setup stops startup,
	// and production refuses to start without them.
	courierBox, err := secretbox.FromLookup(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("courier encryption config: %w", err)
	}
	var courierSvc *courierconn.Service
	var shippingSvc *shipping.Service
	var providerName func(string) string
	switch {
	case courierBox != nil:
		courierReg := courier.NewRegistry(
			courier.NewSteadfast("", nil),
			// PATHAO_BASE_URL is optional (e.g. the sandbox URL); empty = production.
			courier.NewPathao(os.Getenv("PATHAO_BASE_URL"), nil),
		)
		courierSvc = courierconn.New(
			mongorepo.NewCourierConnections(db),
			courierReg,
			courierBox,
			log,
		)
		shippingSvc = shipping.New(
			mongorepo.NewShipments(db),
			shipping.NewMongoOrders(db),
			courierSvc,
			models.NewID,
			log,
		)
		providerName = func(id string) string {
			if p, ok := courierReg.Get(id); ok {
				return p.Name()
			}
			return id
		}
		// Automatic courier integration is a paid feature (checked on the server).
		courierSvc.SetGate(subscriptionSvc)
		log.Info("courier integrations enabled", "providers", []string{courier.SteadfastID, courier.PathaoID})
	case cfg.IsProduction():
		return errors.New("COURIER_ENCRYPTION_KEY and COURIER_FINGERPRINT_KEY are required when APP_ENV=production")
	default:
		log.Warn("COURIER_ENCRYPTION_KEY not set: courier integrations are disabled")
	}

	adminAuthSvc := service.NewAdminAuthService(
		cfg.AdminEmail,
		cfg.AdminPassword,
		mongorepo.NewUsers(db),
		mongorepo.NewTokens(db),
		authSvc,
		sender,
		log,
	)
	// The OTP may only be printed to the log outside production.
	adminAuthSvc.AllowOTPLogging(!cfg.IsProduction())
	if cfg.IsProduction() && !adminAuthSvc.Enabled() {
		return errors.New("ADMIN_EMAIL and ADMIN_PASSWORD are required when APP_ENV=production (admin 2FA must be on)")
	}
	if cfg.IsProduction() && len(cfg.AdminPassword) < 12 {
		return errors.New("ADMIN_PASSWORD must be at least 12 characters when APP_ENV=production")
	}

	// Rebuild slug history rows for address changes made before history was
	// recorded (idempotent; not fatal).
	slugHistoryRepo := mongorepo.NewSlugHistory(db)
	if n, err := slugHistoryRepo.BackfillLegacy(ctx); err != nil {
		log.Error("slug history backfill failed", "error", err.Error())
	} else if n > 0 {
		log.Info("slug history backfilled", "rows", n)
	}

	adminSvc := service.NewAdminService(
		mongorepo.NewUsers(db),
		mongorepo.NewTokens(db),
		mongorepo.NewAdminActivityLogs(db),
		mongorepo.NewSellerRestrictions(db),
		mongorepo.NewAdminNotes(db),
		mongorepo.NewSellerVerifications(db),
		mongorepo.NewSubscriptionOverrides(db),
		mongorepo.NewSellerReports(db),
		mongorepo.NewSellerPromotions(db),
		slugHistoryRepo,
		mongorepo.NewAdminStats(db),
	)

	// Admin plan assignments write to the same subscription rows the plan
	// checks read, so they take effect immediately.
	adminSvc.SetSubscriptions(subscriptionSvc)

	// Free plans are OFFERED by an admin and start when the seller claims
	// them. The seller gets a bell notification with the admin's reason.
	grantSvc := service.NewSubscriptionGrantService(
		mongorepo.NewSubscriptionGrants(db),
		subscriptionSvc,
		mongorepo.NewSubscriptionOverrides(db),
		mongorepo.NewNotifications(db),
		mongorepo.NewAdminActivityLogs(db),
		func(ctx context.Context, sellerID string) string {
			slug, err := storeSvc.CurrentSlugBySeller(ctx, sellerID)
			if err != nil {
				return ""
			}
			return slug
		},
	)
	subscriptionSvc.SetGrants(grantSvc)
	adminSvc.SetGrantOffers(grantSvc)

	// One-time premium features (Try the Look). The price is whole taka; set
	// TRY_THE_LOOK_PRICE_BDT to change it without a code change.
	tryLookPrice := service.DefaultTryTheLookPrice
	if v := strings.TrimSpace(os.Getenv("TRY_THE_LOOK_PRICE_BDT")); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 0 {
			return fmt.Errorf("TRY_THE_LOOK_PRICE_BDT must be a whole number of taka, got %q", v)
		}
		tryLookPrice = n
	}
	log.Info("try the look", "price_bdt", tryLookPrice)
	featureSvc := service.NewFeatureService(
		mongorepo.NewFeatureEntitlements(db),
		mongorepo.NewFeaturePayments(db),
		sslcommerz.New(sslcommerz.Config{
			StoreID:       gwStoreID,
			StorePassword: gwStorePass,
			Sandbox:       gwSandbox,
		}),
		service.FeatureCatalog{
			models.FeatureTryTheLook: {Name: "Try the Look", Price: tryLookPrice},
		},
		service.SubscriptionConfig{
			PublicAPIURL: envOr("PUBLIC_API_URL", "http://localhost:"+cfg.Port),
			FrontendURL:  frontendURL,
		},
		mongorepo.NewAdminActivityLogs(db),
		log,
	)

	// Try the Look showcases. Products are referenced, never copied; a new
	// product made from the look editor goes through the normal product service.
	lookUsers := mongorepo.NewUsers(db)
	lookSvc := service.NewLookService(
		mongorepo.NewLooks(db),
		mongorepo.NewProducts(db),
		productSvc,
		featureSvc,
	)
	lookSvc.SetPublic(
		func(ctx context.Context, sellerID string) (models.Status, error) {
			u, err := lookUsers.FindByID(ctx, sellerID)
			if err != nil {
				return "", err
			}
			return u.Status, nil
		},
		func(ctx context.Context, sellerID string) string {
			slug, err := storeSvc.CurrentSlugBySeller(ctx, sellerID)
			if err != nil {
				return ""
			}
			return slug
		},
	)

	lookSvc.SetSlugResolver(storeSvc.SellerIDBySlug)

	// Admin -> seller notifications (shown on the home page and the seller area).
	notificationSvc := service.NewNotificationService(
		mongorepo.NewNotifications(db),
		mongorepo.NewUsers(db),
		mongorepo.NewAdminActivityLogs(db),
	)

	// Buyer profile and saved addresses.
	buyerSvc := service.NewBuyerService(mongorepo.NewBuyers(db), mongorepo.NewUsers(db))

	// Buyer "My orders" (across all sellers) and "Problem with this order".
	buyerOrderSvc := service.NewBuyerOrderService(
		mongorepo.NewUsers(db),
		mongorepo.NewBuyerOrders(db),
		mongorepo.NewShipments(db),
		mongorepo.NewStoreDesigns(db),
		mongorepo.NewOrderIssues(db),
		mongorepo.NewNotifications(db),
		log,
	)

	// New orders alert the seller: bell notification + email (same SMTP as
	// password reset).
	orderSvc.SetSellerAlerts(mongorepo.NewUsers(db), mongorepo.NewNotifications(db), frontendURL)

	// Continue with Google / Apple. Each one switches on only when its keys are
	// set. Start and callback must be on the same host (the sign-in cookie is
	// host-only), so the redirect base is the host the buttons point at.
	redirectBase := strings.TrimRight(envOr("SOCIAL_REDIRECT_BASE", envOr("PUBLIC_API_URL", "http://localhost:"+cfg.Port)), "/")
	var socialGoogle *oauth.Google
	if id, secret := envOr("GOOGLE_CLIENT_ID", ""), envOr("GOOGLE_CLIENT_SECRET", ""); id != "" && secret != "" {
		socialGoogle = oauth.NewGoogle(id, secret, redirectBase+"/api/v1/auth/google/callback")
		log.Info("google sign-in enabled", "redirect_uri", redirectBase+"/api/v1/auth/google/callback")
	} else {
		log.Warn("GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET not set: Continue with Google is off")
	}
	var socialApple *oauth.Apple
	if id := envOr("APPLE_CLIENT_ID", ""); id != "" {
		a, aerr := oauth.NewApple(id, envOr("APPLE_TEAM_ID", ""), envOr("APPLE_KEY_ID", ""),
			envOr("APPLE_PRIVATE_KEY", ""), redirectBase+"/api/v1/auth/apple/callback")
		if aerr != nil {
			return fmt.Errorf("apple sign-in config: %w", aerr)
		}
		socialApple = a
		log.Info("apple sign-in enabled", "redirect_uri", redirectBase+"/api/v1/auth/apple/callback")
	} else {
		log.Warn("APPLE_CLIENT_ID not set: Continue with Apple is off")
	}

	// Cloudflare R2 image storage. Switches on only when its settings are set.
	var r2 *storage.R2
	var uploadScanner uploads.Scanner
	if envOr("R2_ACCOUNT_ID", "") != "" {
		var rerr error
		r2, rerr = storage.New(storage.Config{
			AccountID:       envOr("R2_ACCOUNT_ID", ""),
			Bucket:          envOr("R2_BUCKET", ""),
			AccessKeyID:     envOr("R2_ACCESS_KEY_ID", ""),
			SecretAccessKey: envOr("R2_SECRET_ACCESS_KEY", ""),
			PublicBaseURL:   envOr("R2_PUBLIC_URL", ""),
		})
		if rerr != nil {
			return fmt.Errorf("r2 storage config: %w", rerr)
		}
		log.Info("r2 image storage enabled", "bucket", envOr("R2_BUCKET", ""))

		// Sellers may only attach images they uploaded themselves.
		productSvc.SetImagePolicy(func(sellerID, u string) bool {
			key, ok := r2.KeyFromURL(u)
			return ok && strings.Split(key, "/")[1] == storage.OwnerSegment(sellerID)
		})

		// Category pictures: sellers may only use their own uploads, and a
		// picture that is replaced, removed or whose category is deleted is
		// removed from R2 (and its upload record, so the quota is freed).
		categorySvc.SetImagePolicy(func(sellerID, u string) bool {
			key, ok := r2.KeyFromURL(u)
			return ok && strings.Split(key, "/")[1] == storage.OwnerSegment(sellerID) &&
				strings.HasPrefix(key, storage.Kinds["category"].Folder+"/")
		})
		categoryUploads := uploads.NewStore(db)
		categorySvc.SetImageRemover(func(rctx context.Context, sellerID, u string) {
			key, ok := r2.KeyFromURL(u)
			if !ok || strings.Split(key, "/")[1] != storage.OwnerSegment(sellerID) {
				return // base64 row, foreign host, or someone else's file: not ours to delete
			}
			dctx, dcancel := context.WithTimeout(rctx, 10*time.Second)
			defer dcancel()
			if err := r2.DeleteObject(dctx, key); err != nil {
				log.Error("category image: r2 delete failed", "error", err.Error())
				return // keep the record so the cleanup job can retry
			}
			if rec, err := categoryUploads.GetByKeyOwned(dctx, sellerID, key); err == nil {
				_ = categoryUploads.Delete(dctx, sellerID, rec.ID)
			}
		})

		// Optional malware scanning (clamd). When set, it fails closed.
		if addr := envOr("CLAMAV_ADDR", ""); addr != "" {
			uploadScanner = &uploads.ClamAV{Addr: addr}
			log.Info("clamav scanning enabled")
		} else {
			log.Warn("CLAMAV_ADDR not set: uploads are re-encoded but not malware-scanned")
		}

		// Remove half-finished uploads; orphan deletion is opt-in.
		upStore := uploads.NewStore(db)
		if cfg.AutoCreateIndexes {
			ictx, icancel := context.WithTimeout(ctx, 30*time.Second)
			if err := upStore.EnsureIndexes(ictx); err != nil {
				log.Error("uploads indexes", "error", err.Error())
			}
			icancel()
		}
		go (&uploads.Cleaner{
			Store: upStore, R2: r2, DB: db, Log: log, PublicBase: envOr("R2_PUBLIC_URL", ""),
			DeleteOrphans: envOr("UPLOAD_ORPHAN_DELETE", "") == "true",
		}).Run(ctx)
	} else {
		log.Warn("R2_ACCOUNT_ID not set: image uploads are off")
	}

	// Fraud settings (admin). The provider key is sealed with the same
	// encryption key as the courier credentials; without it the key cannot
	// be saved, but the on/off switch and history still work.
	fraudAdminSvc := service.NewFraudSettingsService(
		mongorepo.NewFraudSettings(db),
		mongorepo.NewAdminActivityLogs(db),
		courierBox,
		fraudHistory,
		mongorepo.NewFraudOrderSource(db),
		!cfg.IsProduction(),
		ctx,
		log,
	)

	fraudCheckSvc := service.NewFraudCheckService(fraudAdminSvc, mongorepo.NewFraudHistory(db), orderSvc, log)

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: router.New(router.Deps{
			Config:             cfg,
			Log:                log,
			Auth:               authSvc,
			StoreDesign:        storeSvc,
			Products:           productSvc,
			Categories:         categorySvc,
			Orders:             orderSvc,
			PasswordReset:      resetSvc,
			ResetLimiter:       resetLimiter,
			EmailVerification:  verifySvc,
			Analytics:          analyticsSvc,
			Tracking:           trackingSvc,
			CustomDomain:       customDomainSvc,
			Delivery:           deliverySvc,
			Subscription:       subscriptionSvc,
			SubscriptionGrants: grantSvc,
			PlanCatalog:        planCatalogSvc,
			Features:           featureSvc,
			Looks:              lookSvc,
			Admin:              adminSvc,
			FraudAdmin:         fraudAdminSvc,
			FraudCheck:         fraudCheckSvc,
			AdminAuth:          adminAuthSvc,
			Notifications:      notificationSvc,
			Buyer:              buyerSvc,
			BuyerOrders:        buyerOrderSvc,
			Couriers:           courierSvc,
			Shipping:           shippingSvc,
			ProviderName:       providerName,
			FrontendURL:        frontendURL,
			SocialGoogle:       socialGoogle,
			SocialApple:        socialApple,
			Storage:            r2,
			UploadScanner:      uploadScanner,
			EnforcePlans:       enforcePlans,
			DB:                 mongoPinger{client: client},
			Mongo:              db,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", srv.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	// Let queued emails finish (waiting retries are dropped).
	if qerr := mailQueue.Shutdown(sctx); qerr != nil {
		log.Warn("background jobs did not finish before shutdown", "error", qerr.Error())
	}
	return err
}
