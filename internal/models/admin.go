// Package models — admin / moderation document shapes.
package models

import "time"

// Account statuses beyond active/suspended (users.status).
const (
	StatusRestricted Status = "restricted"
	StatusBanned     Status = "banned"
)

// StoreHiddenStatuses are the account statuses whose store (profile, products,
// marketplace listing) must NOT be publicly reachable. "restricted" accounts
// keep their public store; add StatusRestricted here to hide those as well.
var StoreHiddenStatuses = []Status{StatusSuspended, StatusBanned}

// StoreVisible reports whether a seller account in this status may have a
// public store.
func (s Status) StoreVisible() bool {
	for _, h := range StoreHiddenStatuses {
		if s == h {
			return false
		}
	}
	return true
}

// Restriction kinds for seller_restrictions.
const (
	RestrictionSuspend  = "suspend"
	RestrictionRestrict = "restrict"
	RestrictionBan      = "ban"
	RestrictionRestore  = "restore"
)

// Admin role levels (stored on users with RoleAdmin, refined via admin_profiles).
const (
	AdminRoleSuper     = "super"
	AdminRoleModerator = "moderator"
	AdminRoleProduct   = "product_manager"
	AdminRoleSupport   = "support"
	AdminRoleFinance   = "finance"
	AdminRoleAnalyst   = "analyst"
)

// Report / verification statuses.
const (
	ReportStatusNew             = "new"
	ReportStatusUnderReview     = "under_review"
	ReportStatusWaitingSeller   = "waiting_seller"
	ReportStatusWaitingEvidence = "waiting_evidence"
	ReportStatusEscalated       = "escalated"
	ReportStatusResolved        = "resolved"
	ReportStatusDismissed       = "dismissed"

	VerificationPending  = "pending"
	VerificationApproved = "approved"
	VerificationRejected = "rejected"
	VerificationRevoked  = "revoked"
)

// AdminProfile holds role/permission metadata for an admin user.
// One document per admin; _id matches users._id.
type AdminProfile struct {
	ID          string    `bson:"_id"`
	Role        string    `bson:"role"` // super | moderator | product_manager | support | finance | analyst
	Permissions []string  `bson:"permissions,omitempty"`
	CreatedAt   time.Time `bson:"createdAt"`
	UpdatedAt   time.Time `bson:"updatedAt"`
}

// AdminActivityLog is an immutable audit record of admin actions.
type AdminActivityLog struct {
	ID            string    `bson:"_id"`
	AdminID       string    `bson:"adminId"`
	AdminRole     string    `bson:"adminRole,omitempty"`
	Action        string    `bson:"action"`
	TargetType    string    `bson:"targetType,omitempty"` // seller | customer | product | order | report | subscription
	TargetID      string    `bson:"targetId,omitempty"`
	PreviousValue any       `bson:"previousValue,omitempty"`
	NewValue      any       `bson:"newValue,omitempty"`
	Reason        string    `bson:"reason,omitempty"`
	IP            string    `bson:"ip,omitempty"`
	CreatedAt     time.Time `bson:"createdAt"`
}

// SellerRestriction records a suspension, restriction, ban, or restoration.
// History is never deleted; the latest open restriction defines current state.
type SellerRestriction struct {
	ID        string     `bson:"_id"`
	SellerID  string     `bson:"sellerId"`
	Kind      string     `bson:"kind"` // suspend | restrict | ban | restore
	Reason    string     `bson:"reason"`
	AdminID   string     `bson:"adminId"`
	StartsAt  time.Time  `bson:"startsAt"`
	EndsAt    *time.Time `bson:"endsAt,omitempty"` // nil = permanent (ban) or N/A (restore)
	Active    bool       `bson:"active"`
	CreatedAt time.Time  `bson:"createdAt"`
}

// AdminNote is a private note visible only to authorized admins.
type AdminNote struct {
	ID         string    `bson:"_id"`
	TargetType string    `bson:"targetType"` // seller | customer
	TargetID   string    `bson:"targetId"`
	AdminID    string    `bson:"adminId"`
	Body       string    `bson:"body"`
	CreatedAt  time.Time `bson:"createdAt"`
	UpdatedAt  time.Time `bson:"updatedAt"`
}

// SellerVerification holds identity / business verification submissions.
type SellerVerification struct {
	ID              string     `bson:"_id"`
	SellerID        string     `bson:"sellerId"`
	Status          string     `bson:"status"` // pending | approved | rejected | revoked
	FullName        string     `bson:"fullName,omitempty"`
	BusinessName    string     `bson:"businessName,omitempty"`
	BusinessType    string     `bson:"businessType,omitempty"`
	Phone           string     `bson:"phone,omitempty"`
	PhoneVerified   bool       `bson:"phoneVerified"`
	DocumentURLs    []string   `bson:"documentUrls,omitempty"`
	Notes           string     `bson:"notes,omitempty"`
	RejectionReason string     `bson:"rejectionReason,omitempty"`
	ReviewedBy      string     `bson:"reviewedBy,omitempty"`
	ReviewedAt      *time.Time `bson:"reviewedAt,omitempty"`
	SubmittedAt     time.Time  `bson:"submittedAt"`
	CreatedAt       time.Time  `bson:"createdAt"`
	UpdatedAt       time.Time  `bson:"updatedAt"`
}

// SubscriptionOverride is an admin-granted plan (promotional or custom).
// Does not count toward subscription revenue when IsPromotional is true.
type SubscriptionOverride struct {
	ID            string    `bson:"_id"`
	SellerID      string    `bson:"sellerId"`
	Plan          string    `bson:"plan"`
	Interval      string    `bson:"interval,omitempty"`
	StartsAt      time.Time `bson:"startsAt"`
	EndsAt        time.Time `bson:"endsAt"`
	DiscountPct   int       `bson:"discountPct,omitempty"`   // 0-100
	DiscountFixed int64     `bson:"discountFixed,omitempty"` // taka
	IsPromotional bool      `bson:"isPromotional"`
	Reason        string    `bson:"reason"`
	AdminID       string    `bson:"adminId"`
	Active        bool      `bson:"active"`
	CreatedAt     time.Time `bson:"createdAt"`
}

// SellerReport is a customer (or system) report against a seller or product.
type SellerReport struct {
	ID            string     `bson:"_id"`
	ReporterID    string     `bson:"reporterId,omitempty"`
	ReporterName  string     `bson:"reporterName,omitempty"`
	ReporterEmail string     `bson:"reporterEmail,omitempty"`
	ReporterPhone string     `bson:"reporterPhone,omitempty"`
	SellerID      string     `bson:"sellerId"`
	ProductID     string     `bson:"productId,omitempty"`
	Category      string     `bson:"category"`
	Description   string     `bson:"description"`
	EvidenceURLs  []string   `bson:"evidenceUrls,omitempty"`
	Status        string     `bson:"status"`
	AssignedTo    string     `bson:"assignedTo,omitempty"`
	Resolution    string     `bson:"resolution,omitempty"`
	ResolvedBy    string     `bson:"resolvedBy,omitempty"`
	ResolvedAt    *time.Time `bson:"resolvedAt,omitempty"`
	CreatedAt     time.Time  `bson:"createdAt"`
	UpdatedAt     time.Time  `bson:"updatedAt"`
}

// SellerPromotion tracks manual ranking / featured placement.
type SellerPromotion struct {
	ID        string     `bson:"_id"`
	SellerID  string     `bson:"sellerId"`
	Kind      string     `bson:"kind"` // feature | boost | campaign
	Priority  int        `bson:"priority,omitempty"`
	StartsAt  time.Time  `bson:"startsAt"`
	EndsAt    *time.Time `bson:"endsAt,omitempty"`
	AdminID   string     `bson:"adminId"`
	Reason    string     `bson:"reason,omitempty"`
	Active    bool       `bson:"active"`
	CreatedAt time.Time  `bson:"createdAt"`
}

// Sources of a store address change.
const (
	SlugChangeAddress = "address" // seller changed the store address only
	SlugChangeRename  = "rename"  // address moved because the shop was renamed
	SlugChangeLegacy  = "legacy"  // rebuilt from the old previousSlugs list
)

// StoreSlugChange is one row of a seller's store-address history. Rows are
// append-only and are never edited or deleted. ChangedAt is nil only for
// legacy rows rebuilt from data written before changes were recorded.
type StoreSlugChange struct {
	ID        string     `bson:"_id"`
	SellerID  string     `bson:"sellerId"`
	OldSlug   string     `bson:"oldSlug"`
	NewSlug   string     `bson:"newSlug"`
	StoreName string     `bson:"storeName,omitempty"` // shop name after the change (renames only)
	Source    string     `bson:"source"`
	ChangedAt *time.Time `bson:"changedAt,omitempty"`
	CreatedAt time.Time  `bson:"createdAt"`
}
