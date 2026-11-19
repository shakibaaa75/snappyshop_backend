// Package tracking holds the small, dependency-free rules for store tracking:
// pixel ID validation, traffic-source normalisation, and visitor hashing.
// Keeping them here (stdlib only) makes them easy to unit test.
package tracking

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	// Meta (Facebook) pixel IDs are digits only.
	metaPixelRe = regexp.MustCompile(`^\d{8,20}$`)
	// TikTok pixel IDs are upper-case letters and digits (e.g. "C4ABC123DEF456GHI789").
	tiktokPixelRe = regexp.MustCompile(`^[A-Z0-9]{10,32}$`)
)

// ValidMetaPixelID reports whether id is a plausible Meta pixel ID. The ID is
// rendered inside a <script> on the storefront, so strict validation is also
// what prevents script injection by a seller.
func ValidMetaPixelID(id string) bool { return metaPixelRe.MatchString(id) }

// NormalizeTikTokPixelID trims and upper-cases the input (TikTok shows IDs in
// capitals; sellers often paste them in lower case).
func NormalizeTikTokPixelID(id string) string { return strings.ToUpper(strings.TrimSpace(id)) }

// ValidTikTokPixelID reports whether id is a plausible TikTok pixel ID. Pass a
// value already run through NormalizeTikTokPixelID.
func ValidTikTokPixelID(id string) bool { return tiktokPixelRe.MatchString(id) }

/* ------------------------------------------------------------------ */
/* Traffic sources                                                     */
/* ------------------------------------------------------------------ */

// Source keys stored on analytics events.
const (
	SourceDirect    = "direct"
	SourceFacebook  = "facebook"
	SourceInstagram = "instagram"
	SourceTikTok    = "tiktok"
	SourceGoogle    = "google"
	SourceYouTube   = "youtube"
	SourceWhatsApp  = "whatsapp"
	SourceTwitter   = "twitter"
	SourceLinkedIn  = "linkedin"
	SourcePinterest = "pinterest"
	SourceBing      = "bing"
	SourceEmail     = "email"
	SourceReferral  = "referral"
	SourceOther     = "other"
)

var sourceLabels = map[string]string{
	SourceDirect:    "Direct",
	SourceFacebook:  "Facebook",
	SourceInstagram: "Instagram",
	SourceTikTok:    "TikTok",
	SourceGoogle:    "Google",
	SourceYouTube:   "YouTube",
	SourceWhatsApp:  "WhatsApp",
	SourceTwitter:   "X / Twitter",
	SourceLinkedIn:  "LinkedIn",
	SourcePinterest: "Pinterest",
	SourceBing:      "Bing",
	SourceEmail:     "Email",
	SourceReferral:  "Referral",
	SourceOther:     "Other",
}

// SourceLabel returns the display name for a stored source key.
func SourceLabel(key string) string {
	if l, ok := sourceLabels[key]; ok {
		return l
	}
	if key == "" {
		return "Unknown"
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

// aliases maps the keys and domains a client might send to a source key.
// A domain also matches its sub-domains (l.facebook.com, m.facebook.com, ...).
var aliases = []struct {
	key     string
	names   []string // exact utm_source style values
	domains []string
}{
	{SourceFacebook, []string{"facebook", "fb", "meta", "messenger", "fbclid"}, []string{"facebook.com", "fb.com", "fb.me", "messenger.com"}},
	{SourceInstagram, []string{"instagram", "ig"}, []string{"instagram.com"}},
	{SourceTikTok, []string{"tiktok", "tt", "ttclid"}, []string{"tiktok.com"}},
	{SourceYouTube, []string{"youtube", "yt"}, []string{"youtube.com", "youtu.be"}},
	{SourceWhatsApp, []string{"whatsapp", "wa"}, []string{"whatsapp.com", "wa.me"}},
	{SourceTwitter, []string{"twitter", "x"}, []string{"twitter.com", "x.com", "t.co"}},
	{SourceLinkedIn, []string{"linkedin"}, []string{"linkedin.com", "lnkd.in"}},
	{SourcePinterest, []string{"pinterest"}, []string{"pinterest.com", "pin.it"}},
	{SourceBing, []string{"bing"}, []string{"bing.com"}},
	{SourceEmail, []string{"email", "newsletter", "mail"}, nil},
	{SourceGoogle, []string{"google", "googleads", "adwords", "gclid"}, nil}, // google.* handled below
}

// NormalizeSource turns whatever the browser sent (a utm_source value, a
// referrer host, a full URL) into one of a small fixed set of keys. The traffic
// endpoint is public, so this also stops anyone creating unlimited new
// "sources" by posting random strings.
func NormalizeSource(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == "(direct)" || s == "direct" || s == "none" || s == "(none)" {
		return SourceDirect
	}
	if len(s) > 200 {
		s = s[:200]
	}

	// If it looks like a URL or host, reduce it to a bare host name.
	host := s
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "www.")

	for _, a := range aliases {
		for _, n := range a.names {
			if s == n || host == n {
				return a.key
			}
		}
		for _, d := range a.domains {
			if host == d || strings.HasSuffix(host, "."+d) {
				return a.key
			}
		}
	}
	// google.com, google.com.bd, www.google.co.uk ...
	if host == "google" || strings.HasPrefix(host, "google.") || strings.Contains(host, ".google.") {
		return SourceGoogle
	}
	if strings.Contains(host, ".") {
		return SourceReferral // some other website
	}
	return SourceOther
}

/* ------------------------------------------------------------------ */
/* Visitor hashing                                                     */
/* ------------------------------------------------------------------ */

// VisitorHash returns a short, one-way identifier for a visitor of ONE seller's
// store. The raw visitor ID, IP and user agent are never stored; only this hash
// is. Because the seller ID is part of the input, the same person gets a
// different hash on every store, so stores cannot be linked to each other.
//
// When the browser sent a usable visitor ID (visitor consented to the local
// device ID), that ID is used. Otherwise the hash is built from IP + user agent
// plus the UTC day, so without consent a visitor can only be recognised within
// one day and never across days.
func VisitorHash(sellerID, visitorID, ip, userAgent, utcDay string) string {
	visitorID = strings.TrimSpace(visitorID)
	var basis string
	if n := len(visitorID); n >= 8 && n <= 64 {
		basis = "vid:" + visitorID
	} else {
		basis = "anon:" + strings.TrimSpace(ip) + "|" + strings.ToLower(strings.TrimSpace(userAgent)) + "|" + utcDay
	}
	sum := sha256.Sum256([]byte(sellerID + "|" + basis))
	return hex.EncodeToString(sum[:16])
}
