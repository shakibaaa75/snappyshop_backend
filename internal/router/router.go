package router

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/config"
	"shopbd/backend/internal/courierconn"
	"shopbd/backend/internal/handler"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/metrics"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/oauth"
	"shopbd/backend/internal/payment/sslcommerz"
	"shopbd/backend/internal/ratelimit"
	"shopbd/backend/internal/repository/mongorepo"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/shipping"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

type Pinger interface {
	Ping(ctx context.Context) error
}

type LimitSpec struct {
	Every time.Duration
	Burst int
}

type Limits struct {
	Register        LimitSpec
	Login           LimitSpec
	LoginEmail      LimitSpec
	Refresh         LimitSpec
	ForgotPassword  LimitSpec
	ResetPassword   LimitSpec
	CreateOrder     LimitSpec
	SendEmailCode   LimitSpec
	VerifyEmailCode LimitSpec
	Checkout        LimitSpec
	CourierConnect  LimitSpec
	CourierTest     LimitSpec
	Ship            LimitSpec
	Upload          LimitSpec
}

func DefaultLimits() Limits {
	return Limits{
		Register:        LimitSpec{Every: 12 * time.Second, Burst: 5},
		Login:           LimitSpec{Every: 6 * time.Second, Burst: 10},
		LoginEmail:      LimitSpec{Every: 20 * time.Second, Burst: 8},
		Refresh:         LimitSpec{Every: 1 * time.Minute, Burst: 2},
		ForgotPassword:  LimitSpec{Every: 30 * time.Second, Burst: 3},
		ResetPassword:   LimitSpec{Every: 15 * time.Second, Burst: 5},
		CreateOrder:     LimitSpec{Every: 3 * time.Second, Burst: 8},
		SendEmailCode:   LimitSpec{Every: 20 * time.Second, Burst: 5},
		VerifyEmailCode: LimitSpec{Every: 3 * time.Second, Burst: 10},
		Checkout:        LimitSpec{Every: 10 * time.Second, Burst: 5},
		CourierConnect:  LimitSpec{Every: 6 * time.Second, Burst: 10},
		CourierTest:     LimitSpec{Every: 3 * time.Second, Burst: 10},
		Ship:            LimitSpec{Every: 2 * time.Second, Burst: 10},
		Upload:          LimitSpec{Every: 1 * time.Second, Burst: 20},
	}
}

type Deps struct {
	ReviewGateway      *sslcommerz.Client
	Config             *config.Config
	Log                *slog.Logger
	Auth               *service.AuthService
	StoreDesign        *service.StoreDesignService
	Products           *service.ProductService
	Categories         *service.CategoryService
	Orders             *service.OrderService
	PasswordReset      *service.PasswordResetService
	ResetLimiter       ratelimit.Limiter // legacy fixed-window limiter for password reset; ignored when RateGuard is set
	RateGuard          *ratelimit.Guard  // shared token buckets (Redis) for auth endpoints; nil = per-process limits only
	Cache              *cache.Cache      // shared cache (Redis); nil = in-process caching only
	EmailVerification  *service.EmailVerificationService
	Analytics          *service.AnalyticsService
	Tracking           *service.TrackingService     // nil = pixel settings switched off
	CustomDomain       *service.CustomDomainService // nil = custom domains switched off
	Delivery           *service.DeliveryService     // nil = delivery settings switched off
	Subscription       *service.SubscriptionService
	SubscriptionGrants *service.SubscriptionGrantService // free-plan offers the seller claims; nil = off
	PlanCatalog        *service.PlanCatalogService       // admin-editable plan prices and features; nil = built-in plans only
	Features           *service.FeatureService           // one-time premium features (Try the Look); nil = off
	Looks              *service.LookService              // Try the Look showcases; nil = off
	Admin              *service.AdminService             // nil = admin panel disabled
	FraudAdmin         *service.FraudSettingsService     // nil = fraud settings (admin) off
	FraudCheck         *service.FraudCheckService        // nil = seller fraud check off
	AdminAuth          *service.AdminAuthService
	Notifications      *service.NotificationService // nil = admin-to-seller notifications switched off
	Buyer              *service.BuyerService        // nil = buyer profile area switched off
	BuyerOrders        *service.BuyerOrderService   // nil = buyer "My orders" switched off
	Couriers           *courierconn.Service         // nil = courier feature switched off
	Shipping           *shipping.Service            // nil = shipping switched off (needs couriers)
	ProviderName       func(provider string) string
	FrontendURL        string
	SocialGoogle       *oauth.Google   // nil = Continue with Google is off
	SocialApple        *oauth.Apple    // nil = Continue with Apple is off
	Storage            *storage.R2     // nil = image uploads switched off (R2 not configured)
	UploadScanner      uploads.Scanner // nil = no malware scan (images are still re-encoded)
	EnforcePlans       bool
	DB                 Pinger
	Mongo              *mongo.Database
	Limits             *Limits
}

func New(d Deps) http.Handler {
	limits := DefaultLimits()
	if d.Limits != nil {
		limits = *d.Limits
	}
	log := d.Log
	cfg := d.Config

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(middleware.Logger(log))
	// Traffic numbers for the admin "System health" page. Grouped by route
	// pattern, so /store/a and /store/b count as one row.
	r.Use(metrics.Middleware(metrics.Default, func(req *http.Request) string {
		if rc := chi.RouteContext(req.Context()); rc != nil {
			return rc.RoutePattern()
		}
		return ""
	}))
	r.Use(middleware.Recoverer(log))
	// Gzip for JSON answers: the storefront and marketplace lists are 100 KB to
	// 2 MB of JSON, which compresses about ten times. Only when the client sends
	// Accept-Encoding: gzip.
	r.Use(chimw.Compress(5, "application/json"))
	r.Use(middleware.SecurityHeaders(cfg.CookieSecure))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Accept", "Content-Type", middleware.CSRFHeader},
		ExposedHeaders:   []string{"Retry-After", "X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           600,
	}))
	r.Use(chimw.Timeout(25 * time.Second))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteError(w, req, apperr.NotFound(), log)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteError(w, req, apperr.MethodNotAllowed(), log)
	})

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		if d.DB != nil {
			ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
			defer cancel()
			if err := d.DB.Ping(ctx); err != nil {
				log.Error("readiness check failed", "error", err.Error())
				httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
				return
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// Auth endpoints use token buckets shared by all API instances when a
	// RateGuard (Redis) is configured; otherwise the per-process limiters run
	// exactly as before. Routes that share a name share one bucket per client,
	// as they shared one limiter before.
	authLimit := func(name string, s LimitSpec) func(http.Handler) http.Handler {
		if d.RateGuard != nil {
			return middleware.SharedRateLimit(d.RateGuard, name, ratelimit.Spec{Capacity: s.Burst, Every: s.Every}, cfg.TrustProxy, log)
		}
		return middleware.RateLimit(middleware.NewKeyedLimiter(s.Every, s.Burst), cfg.TrustProxy, log)
	}
	var emailLimiter handler.KeyLimiter = middleware.NewKeyedLimiter(limits.LoginEmail.Every, limits.LoginEmail.Burst)
	if d.RateGuard != nil {
		emailLimiter = &ratelimit.KeyedAllow{Guard: d.RateGuard, Name: "login-email",
			Spec: ratelimit.Spec{Capacity: limits.LoginEmail.Burst, Every: limits.LoginEmail.Every}}
	}
	auth := &handler.AuthHandler{
		Svc:          d.Auth,
		StoreDesign:  d.StoreDesign,
		Cookies:      httpx.CookieSettings{Secure: cfg.CookieSecure, Domain: cfg.CookieDomain},
		EmailLimiter: emailLimiter,
		Log:          log,
	}
	registerLimit := authLimit("register", limits.Register)
	loginLimit := authLimit("login", limits.Login)
	refreshLimit := authLimit("refresh", limits.Refresh)
	forgotLimit := authLimit("forgot", limits.ForgotPassword)
	resetLimit := authLimit("reset", limits.ResetPassword)
	// Order creation and checkout are shared across API instances too (Redis);
	// without a RateGuard they keep the per-process limiters.
	orderCreateLimit := authLimit("order-create", limits.CreateOrder)
	checkoutLimit := authLimit("checkout", limits.Checkout)

	sd := &handler.StoreDesignHandler{Svc: d.StoreDesign, Log: log, Cache: d.Cache}
	prod := &handler.ProductHandler{Svc: d.Products, StoreDesign: d.StoreDesign, Log: log, Cache: d.Cache}
	cat := &handler.CategoryHandler{Svc: d.Categories, StoreDesign: d.StoreDesign, Log: log}
	order := &handler.OrderHandler{Svc: d.Orders, Analytics: d.Analytics, TrustProxy: cfg.TrustProxy, Log: log, Cache: d.Cache}
	stats := &handler.StatsHandler{Svc: d.Orders, Log: log}
	reset := &handler.PasswordResetHandler{Svc: d.PasswordReset, Log: log, Limiter: d.ResetLimiter, Guard: d.RateGuard, TrustProxy: cfg.TrustProxy}

	// Platform-wide category tree (Fashion -> Hoodie ...), stored in MongoDB
	// and seeded on first run. The Catalog (second value) is used by the admin
	// category API to reload the tree after every change.
	taxIndex, taxCatalog := loadTaxonomy(d, log)
	tax := &handler.TaxonomyHandler{
		Index: taxIndex,
		Log:   log,
	}

	// Admin Category Management (rename / merge / move / delete).
	// nil when MongoDB or the catalog is unavailable.
	adminCats := newAdminCategories(d, taxCatalog, log)

	var analytics *handler.AnalyticsHandler
	if d.Analytics != nil {
		analytics = &handler.AnalyticsHandler{Svc: d.Analytics, Log: log, TrustProxy: cfg.TrustProxy}
	}

	var trackingH *handler.TrackingHandler
	if d.Tracking != nil {
		trackingH = &handler.TrackingHandler{Svc: d.Tracking, Log: log}
	}

	var customDomainH *handler.CustomDomainHandler
	// A store on a seller's own domain is a different origin from the main
	// site. This lets the CSRF check accept exactly the connected domains.
	var customOrigin func(ctx context.Context, origin string) bool
	if d.CustomDomain != nil {
		customDomainH = &handler.CustomDomainHandler{Svc: d.CustomDomain, Log: log}
		customOrigin = d.CustomDomain.OriginAllowed
	}

	var deliveryH *handler.DeliveryHandler
	if d.Delivery != nil {
		deliveryH = &handler.DeliveryHandler{Svc: d.Delivery, Log: log}
	}

	var notifH *handler.NotificationHandler
	if d.Notifications != nil {
		notifH = &handler.NotificationHandler{Svc: d.Notifications, Log: log}
	}

	var buyerH *handler.BuyerHandler
	if d.Buyer != nil {
		buyerH = &handler.BuyerHandler{Svc: d.Buyer, Log: log}
	}

	var buyerOrdersH *handler.BuyerOrderHandler
	if d.BuyerOrders != nil {
		buyerOrdersH = &handler.BuyerOrderHandler{Svc: d.BuyerOrders, Log: log}
	}

	var subH *handler.SubscriptionHandler
	if d.Subscription != nil {
		subH = &handler.SubscriptionHandler{
			Svc:         d.Subscription,
			Grants:      d.SubscriptionGrants,
			Log:         log,
			FrontendURL: d.FrontendURL,
		}
		if d.StoreDesign != nil {
			subH.CurrentSlug = func(ctx context.Context, sellerID string) string {
				slug, err := d.StoreDesign.CurrentSlugBySeller(ctx, sellerID)
				if err != nil {
					return ""
				}
				return slug
			}
		}
		r.Post(service.CallbackSuccessPath, subH.CallbackSuccess)
		r.Post(service.CallbackFailPath, subH.CallbackFail)
		r.Post(service.CallbackCancelPath, subH.CallbackCancel)
		r.Post(service.IPNPath, subH.IPN)
	}

	// One-time premium features (Try the Look). Same gateway flow as the
	// subscription, with its own callback routes.
	var featH *handler.FeatureHandler
	if d.Features != nil {
		featH = &handler.FeatureHandler{Svc: d.Features, Log: log, FrontendURL: d.FrontendURL}
		if d.StoreDesign != nil {
			featH.CurrentSlug = func(ctx context.Context, sellerID string) string {
				slug, err := d.StoreDesign.CurrentSlugBySeller(ctx, sellerID)
				if err != nil {
					return ""
				}
				return slug
			}
		}
		r.Post(service.FeatureCallbackSuccessPath, featH.CallbackSuccess)
		r.Post(service.FeatureCallbackFailPath, featH.CallbackFail)
		r.Post(service.FeatureCallbackCancelPath, featH.CallbackCancel)
		r.Post(service.FeatureIPNPath, featH.IPN)
	}

	var lookH *handler.LookHandler
	if d.Looks != nil {
		lookH = &handler.LookHandler{Svc: d.Looks, Log: log}
	}

	// Continue with Google / Apple. Either provider may be nil (keys not set);
	// the handler then redirects to /login?error=social_unavailable.
	socialH := &handler.SocialHandler{
		Auth:        auth,
		Google:      d.SocialGoogle,
		Apple:       d.SocialApple,
		Signer:      oauth.NewSigner(cfg.JWTSecret),
		Secure:      cfg.CookieSecure,
		FrontendURL: d.FrontendURL,
		Log:         log,
	}
	// Apple answers with a cross-site POST that cannot carry our CSRF header,
	// so this one route lives outside the CSRF-protected /api/v1 group. The
	// signed state cookie and the nonce in the ID token protect it instead.
	r.Post(httpx.RefreshCookiePath+"/apple/callback", socialH.AppleCallback)

	reviews := handler.Reviews{DB: d.Mongo, Gateway: d.ReviewGateway, Log: log, Cache: d.Cache}
	if d.Storage != nil {
		reviews.Photos = d.Storage // review photos go to Cloudflare R2, not MongoDB
	}
	if d.Mongo != nil {
		order.Reviews = &reviews
	}
	publicReadLimit := middleware.RateLimit(middleware.NewKeyedLimiter(500*time.Millisecond, 120), cfg.TrustProxy, log)
	if d.Mongo != nil {
		r.With(orderCreateLimit).Post(httpx.APIPrefix+"/orders/payment/ipn", reviews.OrderPaymentIPN)
	}
	trackLimit := middleware.RateLimit(middleware.NewKeyedLimiter(200*time.Millisecond, 60), cfg.TrustProxy, log)

	r.Route(httpx.APIPrefix, func(api chi.Router) {
		api.Use(middleware.CSRFWith(cfg.CORSOrigins, customOrigin, log))

		api.Route("/auth", func(a chi.Router) {
			a.Use(middleware.NoStore)
			a.With(registerLimit).Post("/register", auth.Register)
			a.With(loginLimit).Post("/login", auth.Login)
			a.With(refreshLimit).Post("/refresh", auth.Refresh)
			a.Post("/logout", auth.Logout)
			a.With(middleware.Authenticate(d.Auth, log)).Get("/me", auth.Me)
			// A signed-in buyer becomes a seller (same account, needs shop name + address).
			a.With(loginLimit, middleware.Authenticate(d.Auth, log)).Post("/become-seller", auth.BecomeSeller)

			a.With(forgotLimit).Post("/forgot-password", reset.ForgotPassword)
			a.With(resetLimit).Post("/reset-password", reset.ResetPassword)

			// Google-first accounts add a password (signed in + emailed code).
			a.With(forgotLimit, middleware.Authenticate(d.Auth, log)).Post("/password/set/request", reset.RequestSetPassword)
			a.With(resetLimit, middleware.Authenticate(d.Auth, log)).Post("/password/set", reset.SetPassword)

			if d.EmailVerification != nil {
				ev := &handler.EmailVerificationHandler{Svc: d.EmailVerification, Log: log}
				sendCodeLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.SendEmailCode.Every, limits.SendEmailCode.Burst), cfg.TrustProxy, log)
				verifyCodeLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.VerifyEmailCode.Every, limits.VerifyEmailCode.Burst), cfg.TrustProxy, log)
				a.With(sendCodeLimit).Post("/email/send-code", ev.SendCode)
				a.With(verifyCodeLimit).Post("/email/verify-code", ev.VerifyCode)
			}

			// Continue with Google / Apple (browser redirects, not JSON).
			a.With(loginLimit).Get("/google", socialH.GoogleStart)
			a.With(loginLimit).Get("/google/callback", socialH.GoogleCallback)
			a.With(loginLimit).Get("/apple", socialH.AppleStart)
			// Connect Google / Apple to an existing account after the owner
			// proves who they are (password, or a session for that account).
			a.Get("/social/pending", socialH.PendingLink)
			a.Delete("/social/pending", socialH.CancelLink)
			a.With(loginLimit).Post("/social/link", socialH.ConfirmLink)
			// POST /apple/callback is registered on the root router (no CSRF).
		})

		if d.Mongo != nil {
			mk := &handler.MarketplaceHandler{DB: d.Mongo, Log: log, Tax: taxIndex, Cache: d.Cache, ServeStale: true}
			// A renamed shop must disappear under its old name/URL at once,
			// not when the marketplace snapshot expires.
			if d.StoreDesign != nil {
				d.StoreDesign.SetOnIdentityChange(func() {
					mk.InvalidateSnapshot()
					sd.InvalidateIdentity() // old URL must 404 at once
				})
			}
			// Suspending / banning / restoring a seller must show up on the
			// marketplace at once as well.
			if d.Admin != nil {
				d.Admin.SetOnVisibilityChange(func() {
					mk.InvalidateSnapshot()
					sd.InvalidateIdentity()
				})
			}
			go mk.Warm() // build the snapshot now, not on the first visit
			api.With(publicReadLimit).Get("/marketplace/products", mk.Products)
			api.With(publicReadLimit).Get("/marketplace/sellers", mk.Sellers)
			api.With(publicReadLimit).Get("/marketplace/categories", mk.Categories)
		}

		// Platform-wide categories. Public and read-only.
		api.With(publicReadLimit).Get("/categories", tax.Home)
		if lookH != nil {
			// Public Try the Look page (published looks only).
			api.With(publicReadLimit).Get("/looks/{id}", lookH.Public)
			// Published looks for the marketplace, a store page and the all-looks page.
			api.With(publicReadLimit).Get("/looks", lookH.PublicList)
		}
		api.With(publicReadLimit).Get("/categories/tree", tax.Tree)
		api.With(publicReadLimit).Get("/categories/suggest", tax.Suggest)
		api.With(publicReadLimit).Get("/categories/{id}/children", tax.Children)

		api.With(publicReadLimit).Get("/store/{slug}", sd.PublicStore)
		api.With(publicReadLimit).Get("/store/{slug}/products", prod.PublicList)
		api.With(orderCreateLimit, middleware.OptionalCheckoutAuth(d.Auth, log)).Post("/store/{slug}/orders", order.Create)
		// Public: price a cart for a district (same rules as placing the order).
		api.With(publicReadLimit).Post("/store/{slug}/delivery-quote", order.Quote)
		api.With(publicReadLimit).Get("/store/{slug}/categories", cat.PublicList)

		if analytics != nil {
			api.With(trackLimit).Post("/analytics/events", analytics.Track)
		}
		if trackingH != nil {
			// Public: which pixels (if any) this one store uses, and whether it asks for consent.
			api.With(publicReadLimit).Get("/store/{slug}/tracking", trackingH.Public)
		}
		if customDomainH != nil {
			// Public lookups for the platform's own infrastructure. They are
			// called server-to-server (the frontend and the TLS server), so
			// every call shares one IP: they are not rate limited per IP.
			// ResolveHost caches (also for misses) and bounds that cache.
			api.Get("/domain/resolve", customDomainH.Resolve)
			api.Get("/domain/allowed", customDomainH.Allowed)
		}
		if deliveryH != nil {
			// Public: the delivery charges (Inside / Outside Dhaka) a storefront shows.
			api.With(publicReadLimit).Get("/store/{slug}/delivery-settings", deliveryH.Public)
		}

		if d.Mongo != nil {
			api.With(publicReadLimit, middleware.NoStore).Get("/reviews", reviews.Public)
		}
		if buyerH != nil {
			api.Route("/buyer", func(b chi.Router) {
				b.Use(middleware.NoStore)
				b.Use(middleware.Authenticate(d.Auth, log))
				b.Use(middleware.RequireRole(log, models.RoleBuyer))

				if d.Mongo != nil {
					b.Get("/reviews", reviews.Buyer)
					b.Get("/review-purchases", reviews.Purchases)
					b.With(orderCreateLimit).Put("/reviews", reviews.Save)
					b.With(orderCreateLimit).Post("/review-receipt", reviews.Receipt)
				}
				b.With(orderCreateLimit).Post("/store/{slug}/orders", order.Create)
				b.Get("/profile", buyerH.Profile)
				b.Put("/profile", buyerH.UpdateProfile)
				b.Put("/preferences", buyerH.UpdatePreferences)
				if notifH != nil {
					// The buyer's bell (welcome message, ...). Same handlers as the seller's:
					// the user always comes from the login, never from the request.
					b.Get("/notifications", notifH.SellerList)
					b.Post("/notifications/read-all", notifH.SellerMarkAllRead)
					b.Post("/notifications/{id}/read", notifH.SellerMarkRead)
				}

				b.Get("/addresses", buyerH.Addresses)
				b.Post("/addresses", buyerH.AddAddress)
				b.Put("/addresses/{id}", buyerH.UpdateAddress)
				b.Put("/addresses/{id}/default", buyerH.SetDefaultAddress)
				b.Delete("/addresses/{id}", buyerH.DeleteAddress)

				if buyerOrdersH != nil {
					b.Get("/orders", buyerOrdersH.Orders)
					b.Get("/orders/{id}", buyerOrdersH.Order)
					b.Post("/orders/{id}/issues", buyerOrdersH.ReportIssue)
					b.Get("/issues", buyerOrdersH.Issues)
					b.Post("/issues/{id}/withdraw", buyerOrdersH.WithdrawIssue)
				}
			})
		}

		// Plan prices and features for the pricing page. Public: it holds no
		// private data and the pricing page needs it before any purchase.
		if d.PlanCatalog != nil {
			planH := &handler.PlanCatalogHandler{Svc: d.PlanCatalog, Log: log}
			api.With(publicReadLimit).Get("/plans", planH.Public)
		}

		api.Route("/seller", func(s chi.Router) {
			s.Use(middleware.NoStore)
			s.Use(middleware.Authenticate(d.Auth, log))
			s.Use(middleware.RequireRole(log, models.RoleSeller))

			if d.Mongo != nil {
				s.Get("/reviews", reviews.Seller)
				s.Get("/reviews/settings", reviews.HomeSettings)
				s.Put("/reviews/settings", reviews.SaveHomeSettings)
				s.With(orderCreateLimit).Post("/reviews/{id}", reviews.SellerAction)
			}
			s.Get("/store-design", sd.GetDraft)
			s.Put("/store-design", sd.SaveDraft)
			s.Post("/store-design/publish", sd.Publish)
			s.Put("/store-address", sd.ChangeSlug)
			s.Put("/store-name", sd.ChangeName)

			s.Get("/products", prod.List)
			s.Post("/products", prod.Create)
			s.Put("/products/{id}", prod.Update)
			s.Delete("/products/{id}", prod.Delete)

			s.Get("/categories", cat.List)
			s.Post("/categories", cat.Create)
			s.Put("/categories/{id}", cat.Update)
			s.Delete("/categories/{id}", cat.Delete)

			s.Get("/orders", order.List)
			s.Get("/orders/{id}", order.Get)
			s.Patch("/orders/{id}", order.UpdateStatus)
			s.Patch("/orders/{id}/paid", order.MarkPaid)

			s.Get("/stats", stats.Get)

			if d.Storage != nil {
				uploadH := newUploadHandler(d, cfg.TrustProxy, log)
				uploadLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.Upload.Every, limits.Upload.Burst), cfg.TrustProxy, log)
				s.With(uploadLimit).Post("/uploads", uploadH.Upload)
				s.With(uploadLimit).Delete("/uploads/{id}", uploadH.Delete)
				s.With(uploadLimit).Delete("/uploads", uploadH.DeleteByKey)
				s.With(uploadLimit).Post("/uploads/presign", uploadH.Presign) // video only; images get 410
			}

			if d.Couriers != nil {
				courierH := &handler.CourierHandler{Svc: d.Couriers, Log: log}
				courierConnectLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.CourierConnect.Every, limits.CourierConnect.Burst), cfg.TrustProxy, log)
				courierTestLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.CourierTest.Every, limits.CourierTest.Burst), cfg.TrustProxy, log)
				s.Get("/couriers/providers", courierH.Providers)
				s.Get("/couriers", courierH.List)
				s.With(courierConnectLimit).Post("/couriers", courierH.Connect)
				s.With(courierTestLimit).Post("/couriers/{id}/test", courierH.Test)
				s.With(courierConnectLimit).Put("/couriers/{id}/credentials", courierH.UpdateCredentials)
				s.Delete("/couriers/{id}", courierH.Disconnect)

				if d.Shipping != nil {
					shipH := &handler.ShippingHandler{Svc: d.Shipping, ProviderName: d.ProviderName, Log: log}
					shipLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.Ship.Every, limits.Ship.Burst), cfg.TrustProxy, log)
					s.Get("/shipments", shipH.List)
					s.With(shipLimit).Post("/orders/ship", shipH.ShipMany)
					s.With(shipLimit).Post("/orders/{id}/ship", shipH.Ship)
				}
			}

			if d.FraudCheck != nil {
				fraudH := &handler.FraudSellerHandler{Svc: d.FraudCheck, Log: log}
				s.Get("/fraud/status", fraudH.Status)
				s.Post("/orders/{id}/fraud-check", fraudH.Check)
				s.Get("/orders/{id}/fraud-check", fraudH.Saved)
			}

			if notifH != nil {
				s.Get("/notifications", notifH.SellerList)
				s.Post("/notifications/read-all", notifH.SellerMarkAllRead)
				s.Post("/notifications/{id}/read", notifH.SellerMarkRead)
			}

			if subH != nil {
				s.Get("/subscription", subH.Get)
				s.Post("/subscription/grants/{id}/claim", subH.ClaimGrant)
				s.With(checkoutLimit).Post("/subscription/checkout", subH.Checkout)
			}

			if lookH != nil {
				s.Get("/looks", lookH.List)
				s.Post("/looks", lookH.Create)
				s.Get("/looks/{id}", lookH.Get)
				s.Put("/looks/{id}", lookH.Update)
				s.Delete("/looks/{id}", lookH.Delete)
				s.Post("/looks/{id}/publish", lookH.Publish)
				s.Post("/looks/{id}/unpublish", lookH.Unpublish)
				s.Post("/looks/{id}/items", lookH.AddExisting)
				s.Post("/looks/{id}/items/new", lookH.AddNew)
				s.Put("/looks/{id}/items/order", lookH.Reorder)
				s.Put("/looks/{id}/items/{productId}", lookH.Replace)
				s.Delete("/looks/{id}/items/{productId}", lookH.Remove)
			}

			if featH != nil {
				s.Get("/features/{feature}", featH.Get)
				s.With(checkoutLimit).Post("/features/{feature}/checkout", featH.Checkout)
			}

			if trackingH != nil {
				// Meta Pixel / Pyxel locks are checked inside TrackingService.Save,
				// so a Free seller can still open the page and clear a pixel.
				s.Get("/tracking-settings", trackingH.Get)
				s.Put("/tracking-settings", trackingH.Save)
			}

			if customDomainH != nil {
				// The plan check (Business) runs inside the service, so a seller
				// on a lower plan can still open the page and see the upgrade message.
				domainLimit := middleware.RateLimit(middleware.NewKeyedLimiter(2*time.Second, 10), cfg.TrustProxy, log)
				s.Get("/custom-domain", customDomainH.Get)
				s.With(domainLimit).Put("/custom-domain", customDomainH.Set)
				s.With(domainLimit).Post("/custom-domain/verify", customDomainH.Verify)
				s.Delete("/custom-domain", customDomainH.Remove)
			}

			if deliveryH != nil {
				// Available on every plan, including Free.
				s.Get("/delivery-settings", deliveryH.Get)
				s.Put("/delivery-settings", deliveryH.Save)
			}

			if analytics != nil {
				if d.EnforcePlans && d.Subscription != nil {
					s.With(requireAnalyticsPlan(d.Subscription, log)).Get("/analytics/overview", analytics.Overview)
				} else {
					s.Get("/analytics/overview", analytics.Overview)
				}
			}
		})

		// Super Admin control plane. All routes require RoleAdmin.
		if d.Admin != nil {
			adminH := &handler.AdminHandler{Svc: d.Admin, Log: log}
			api.Route("/admin", func(a chi.Router) {
				a.Use(middleware.NoStore)
				// Public admin gate (env credentials + email OTP)
				if d.AdminAuth != nil {
					aah := &handler.AdminAuthHandler{
						Svc:     d.AdminAuth,
						Cookies: httpx.CookieSettings{Secure: cfg.CookieSecure, Domain: cfg.CookieDomain},
						Log:     log,
					}
					a.Get("/auth/status", aah.Status)
					a.Post("/auth/logout", aah.LogoutAdmin)
					if d.AdminAuth.Enabled() {
						// Strict, per-IP limits: this is the most valuable login.
						adminLoginLimit := middleware.RateLimit(middleware.NewKeyedLimiter(30*time.Second, 5), cfg.TrustProxy, log)
						adminOTPLimit := middleware.RateLimit(middleware.NewKeyedLimiter(10*time.Second, 6), cfg.TrustProxy, log)
						a.With(adminLoginLimit).Post("/auth/login", aah.Login)
						a.With(adminOTPLimit).Post("/auth/verify-otp", aah.VerifyOTP)
					}
				}
				a.Group(func(ar chi.Router) {
					if d.AdminAuth != nil && d.AdminAuth.Enabled() {
						ar.Use(middleware.RequireAdminGate(d.AdminAuth, log))
					}
					ar.Use(middleware.Authenticate(d.Auth, log))
					ar.Use(middleware.RequireRole(log, models.RoleAdmin))

					if d.Mongo != nil {
						ar.Get("/reviews", reviews.Admin)
						ar.Post("/review-order-evidence", reviews.RecordOrderEvidence)
						ar.Post("/reviews/{id}", reviews.Moderate)
					}
					ar.Get("/dashboard", adminH.Dashboard)

					// Fraud check: provider key, switches, history import.
					if d.FraudAdmin != nil {
						fraudAdminH := &handler.FraudAdminHandler{Svc: d.FraudAdmin, Log: log}
						ar.Get("/fraud/settings", fraudAdminH.Get)
						ar.Put("/fraud/settings", fraudAdminH.Update)
						ar.Post("/fraud/test", fraudAdminH.Test)
						ar.Post("/fraud/backfill", fraudAdminH.BackfillStart)
						ar.Get("/fraud/backfill", fraudAdminH.BackfillStatus)
					}

					// Plan prices, limits and features (what the sellers see on
					// the pricing page and what the plan checks enforce).
					if d.PlanCatalog != nil {
						planAdminH := &handler.PlanCatalogHandler{Svc: d.PlanCatalog, Log: log}
						ar.Get("/plans", planAdminH.AdminGet)
						ar.Put("/plans", planAdminH.AdminUpdate)
						ar.Post("/plans/reset", planAdminH.AdminReset)
					}

					ar.Get("/sellers", adminH.ListSellers)
					ar.Get("/sellers/{id}", adminH.GetSeller)
					ar.Post("/sellers/{id}/suspend", adminH.RestrictSeller)
					ar.Post("/sellers/{id}/restrict", adminH.RestrictSeller)
					ar.Post("/sellers/{id}/ban", adminH.RestrictSeller)
					ar.Post("/sellers/{id}/restore", adminH.RestoreSeller)
					ar.Get("/sellers/{id}/notes", adminH.ListSellerNotes)
					ar.Post("/sellers/{id}/notes", adminH.AddSellerNote)
					ar.Post("/sellers/{id}/subscription/grant", adminH.GrantSubscription)
					ar.Get("/sellers/{id}/subscription", adminH.GetSellerSubscription)
					ar.Post("/sellers/{id}/subscription", adminH.AssignSubscription)
					ar.Post("/sellers/{id}/subscription/active", adminH.SetSubscriptionActive)
					ar.Post("/sellers/{id}/subscription/extend", adminH.ExtendSubscription)
					ar.Post("/sellers/{id}/subscription/remove", adminH.RemoveSubscription)
					ar.Post("/sellers/{id}/subscription/grants/{grantId}/revoke", adminH.RevokeSubscriptionOffer)
					if featH != nil {
						ar.Get("/sellers/{id}/features/{feature}", featH.AdminGet)
						ar.Post("/sellers/{id}/features/{feature}/grant", featH.AdminGrant)
						ar.Post("/sellers/{id}/features/{feature}/status", featH.AdminSetStatus)
					}
					ar.Post("/sellers/{id}/feature", adminH.FeatureSeller)
					ar.Post("/sellers/{id}/unfeature", adminH.UnfeatureSeller)

					ar.Get("/customers", adminH.ListCustomers)
					ar.Post("/customers/{id}/action", adminH.CustomerAction)

					ar.Get("/verifications", adminH.ListVerifications)
					ar.Post("/verifications/{id}/review", adminH.ReviewVerification)

					ar.Get("/reports", adminH.ListReports)
					ar.Get("/reports/{id}", adminH.GetReport)
					ar.Patch("/reports/{id}", adminH.ResolveReport)

					ar.Get("/slug-history", adminH.ListSlugHistory)

					if notifH != nil {
						ar.Get("/notifications", notifH.AdminList)
						ar.Post("/notifications", notifH.Send)
						ar.Delete("/notifications/{id}", notifH.AdminDelete)
					}

					if d.Mongo != nil {
						sysH := &handler.SystemHandler{DB: d.Mongo, Metrics: metrics.Default, Log: log}
						ar.Get("/system", sysH.Report)
					}

					ar.Get("/activity-logs", adminH.ListActivityLogs)

					// Image uploads for the admin (category pictures). Same presign
					// handler as the sellers use; the key folder is the admin's id.
					if d.Storage != nil {
						adminUploadH := newUploadHandler(d, cfg.TrustProxy, log)
						adminUploadLimit := middleware.RateLimit(middleware.NewKeyedLimiter(limits.Upload.Every, limits.Upload.Burst), cfg.TrustProxy, log)
						ar.With(adminUploadLimit).Post("/uploads", adminUploadH.Upload)
						ar.With(adminUploadLimit).Delete("/uploads/{id}", adminUploadH.Delete)
						ar.With(adminUploadLimit).Delete("/uploads", adminUploadH.DeleteByKey)
						ar.With(adminUploadLimit).Post("/uploads/presign", adminUploadH.Presign)
					}

					// Category Management
					if adminCats != nil {
						ar.Route("/categories", func(cr chi.Router) {
							cr.Get("/", adminCats.List)
							cr.Get("/new", adminCats.New)
							cr.Post("/seen", adminCats.MarkSeen)
							cr.Get("/{id}/similar", adminCats.Similar)
							cr.Put("/{id}", adminCats.Rename)
							cr.Put("/{id}/image", adminCats.SetImage)
							cr.Post("/{id}/merge", adminCats.Merge)
							cr.Post("/{id}/move", adminCats.Move)
							cr.Delete("/{id}", adminCats.Delete)
						})
					}
				})
			})
		}
	})

	return r
}

func requireAnalyticsPlan(svc *service.SubscriptionService, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := middleware.ClaimsFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), log)
				return
			}
			if err := svc.RequireAnalytics(r.Context(), claims.UserID); err != nil {
				httpx.WriteError(w, r, err, log)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// newUploadHandler builds the image-upload handler. Each route group gets its
// own per-user limiter; the per-IP limiter is added where the routes are set.
func newUploadHandler(d Deps, trustProxy bool, log *slog.Logger) *handler.UploadHandler {
	h := &handler.UploadHandler{
		R2: d.Storage, Scanner: d.UploadScanner, TrustProxy: trustProxy, Log: log,
		UserLimit: middleware.NewKeyedLimiter(3*time.Second, 10),
	}
	if d.Mongo != nil {
		h.Store = uploads.NewStore(d.Mongo)
		h.Products = mongorepo.NewProducts(d.Mongo)
	}
	if d.Subscription != nil {
		h.Plans = d.Subscription
	}
	return h
}
