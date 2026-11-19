package models

import "time"

// Custom domain states.
const (
	// DomainPending: the seller added the domain but has not proven they own it.
	DomainPending = "pending"
	// DomainVerified: the ownership DNS record was found. Only verified
	// domains serve the store and get a TLS certificate.
	DomainVerified = "verified"
)

// CustomDomain is stored in the "custom_domains" collection, one document per
// seller (the _id is the seller ID), so a seller has at most one domain.
//
// A unique index on Domain applies only to VERIFIED rows. Several sellers may
// have the same domain pending, so nobody can block the real owner by typing
// their domain first; whoever proves ownership first gets it.
type CustomDomain struct {
	SellerID string `bson:"_id"`
	Domain   string `bson:"domain"`
	Status   string `bson:"status"`
	// Token goes into the ownership TXT record. It is not a secret: it only
	// proves the person who set the DNS record is the one who added the domain.
	Token string `bson:"token"`
	// DNSOK is whether the domain pointed at the platform at the last check.
	DNSOK         bool       `bson:"dnsOk"`
	LastError     string     `bson:"lastError,omitempty"`
	VerifiedAt    *time.Time `bson:"verifiedAt,omitempty"`
	LastCheckedAt *time.Time `bson:"lastCheckedAt,omitempty"`
	CreatedAt     time.Time  `bson:"createdAt"`
	UpdatedAt     time.Time  `bson:"updatedAt"`
}
