package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var reservedSlugs = map[string]struct{}{
	"admin": {}, "api": {}, "auth": {}, "new": {}, "edit": {},
	"settings": {}, "support": {}, "help": {}, "about": {},
	"login": {}, "register": {}, "store": {}, "stores": {}, "seller": {},
}

// SlugChangeCooldown is how long a seller must wait between store-address
// (URL) changes. The same 30-day lock also applies after a shop-name change,
// because a name change moves the URL.
const SlugChangeCooldown = 30 * 24 * time.Hour

// NameChangeCooldown is how long the shop name is locked after a rename.
const NameChangeCooldown = SlugChangeCooldown

// MaxNameChanges is how many times a seller may rename the shop after
// registration. The name given at registration is free; ONE later change is
// allowed.
const MaxNameChanges = 1

const (
	minStoreNameLen = 3
	maxStoreNameLen = 60
	maxSlugLen      = 40
)

// NameChangeDisclaimer is the text the seller must see and accept before a
// rename is confirmed. The API refuses a rename unless it was acknowledged.
const NameChangeDisclaimer = "You can change your shop name only once, and after changing it you will not be " +
	"able to change it again for 30 days. Your shop URL will change immediately and the old " +
	"URL will stop working (404)."

type StoreDesignService struct {
	repo repository.StoreDesignRepository

	mu               sync.RWMutex
	onIdentityChange func()
	now              func() time.Time
	gate             DesignGate // optional: nil means no plan checks
}

// DesignGate is satisfied by *SubscriptionService.
type DesignGate interface {
	CheckDesignChoice(ctx context.Context, sellerID, themeID, cardID string) error
	CheckTemplateChoice(ctx context.Context, sellerID, templateID string) error
	CheckPremiumDesign(ctx context.Context, sellerID string, design map[string]any) error
	CheckSectionOrder(ctx context.Context, sellerID string, design, previous map[string]any) error
}

// SetDesignGate turns on plan checks for the theme a seller saves.
func (s *StoreDesignService) SetDesignGate(g DesignGate) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

func NewStoreDesignService(repo repository.StoreDesignRepository) *StoreDesignService {
	return &StoreDesignService{repo: repo, now: time.Now}
}

// SetOnIdentityChange registers a callback that runs after a shop name or URL
// changed. It is used to drop public caches so the old name/URL disappear at
// once instead of when the cache expires.
func (s *StoreDesignService) SetOnIdentityChange(fn func()) {
	s.mu.Lock()
	s.onIdentityChange = fn
	s.mu.Unlock()
}

func (s *StoreDesignService) identityChanged() {
	s.mu.RLock()
	fn := s.onIdentityChange
	s.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// SlugChangeInfo tells the client whether the seller may change their store
// address / shop name right now, and when the next change becomes available.
type SlugChangeInfo struct {
	CanChangeSlug    bool       `json:"canChangeSlug"`
	NextSlugChangeAt *time.Time `json:"nextSlugChangeAt,omitempty"`
	CurrentSlug      string     `json:"currentSlug,omitempty"`

	CurrentName          string     `json:"currentName,omitempty"`
	CanChangeName        bool       `json:"canChangeName"`
	NameChangesRemaining int        `json:"nameChangesRemaining"`
	NextNameChangeAt     *time.Time `json:"nextNameChangeAt,omitempty"`
	NameChangeDisclaimer string     `json:"nameChangeDisclaimer,omitempty"`
}

// DraftResult is returned by GetDraftWithMeta.
type DraftResult struct {
	Design map[string]any
	Meta   SlugChangeInfo
}

// GetDraft returns the seller's draft, or nil if none exists yet.
func (s *StoreDesignService) GetDraft(ctx context.Context, sellerID string) (map[string]any, error) {
	res, err := s.GetDraftWithMeta(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	return res.Design, nil
}

// GetDraftWithMeta returns the draft plus change-lock metadata so the UI can
// show when the seller may next change their shop name or address.
func (s *StoreDesignService) GetDraftWithMeta(ctx context.Context, sellerID string) (*DraftResult, error) {
	d, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return &DraftResult{
			Design: nil,
			Meta: SlugChangeInfo{
				CanChangeSlug:        true,
				NameChangesRemaining: MaxNameChanges,
			},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	m, _ := d.Draft.(map[string]any)
	if m != nil && d.StoreName != "" {
		// The server-side name is the source of truth.
		m["storeName"] = d.StoreName
	}
	return &DraftResult{
		Design: m,
		Meta:   s.slugChangeInfo(d),
	}, nil
}

func (s *StoreDesignService) slugChangeInfo(d *models.StoreDesign) SlugChangeInfo {
	info := SlugChangeInfo{
		CanChangeSlug: true,
		CurrentSlug:   d.Slug,
		CurrentName:   d.StoreName,
	}

	remaining := MaxNameChanges - d.NameChangeCount
	if remaining < 0 {
		remaining = 0
	}
	info.NameChangesRemaining = remaining
	info.CanChangeName = remaining > 0
	if info.CanChangeName {
		info.NameChangeDisclaimer = NameChangeDisclaimer
	}

	if d.SlugChangedAt == nil || d.SlugChangedAt.IsZero() {
		return info
	}
	next := d.SlugChangedAt.Add(SlugChangeCooldown)
	if s.now().UTC().Before(next) {
		info.CanChangeSlug = false
		t := next.UTC()
		info.NextSlugChangeAt = &t
		if remaining > 0 {
			info.CanChangeName = false
			info.NextNameChangeAt = &t
			info.NameChangeDisclaimer = ""
		}
	}
	return info
}

// SaveDraft upserts the design draft.
//
// Shop name and shop URL are an identity that only changes through the
// dedicated, confirmed flows (ChangeStoreName / ChangeSlug):
//   - On first save the shop name is REQUIRED (it is chosen at registration).
//   - Afterwards any storeName in the payload is ignored: the stored name is
//     written back, so autosave can never rename the shop.
//   - A different storeSlug is routed through ChangeSlug (30-day rule).
//
// Every other design field can be saved freely at any time.
func (s *StoreDesignService) SaveDraft(ctx context.Context, sellerID string, design map[string]any) error {
	if design == nil {
		return apperr.BadRequest("Missing design")
	}

	// Plan check on the server: a locked theme is refused even if the request
	// did not come from the dashboard. Publish calls SaveDraft, so it is
	// covered too.
	s.mu.RLock()
	gate := s.gate
	s.mu.RUnlock()
	if gate != nil {
		themeID, _ := design["themePreset"].(string)
		if err := gate.CheckDesignChoice(ctx, sellerID, strings.TrimSpace(themeID), ""); err != nil {
			return err
		}
		templateID, _ := design["template"].(string)
		if err := gate.CheckTemplateChoice(ctx, sellerID, strings.TrimSpace(templateID)); err != nil {
			return err
		}
		if err := gate.CheckPremiumDesign(ctx, sellerID, design); err != nil {
			return err
		}
	}

	existing, err := s.repo.GetBySeller(ctx, sellerID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}

	// Arranging homepage sections is premium: compare with the stored draft.
	if gate != nil {
		var prev map[string]any
		if existing != nil {
			prev, _ = existing.Draft.(map[string]any)
		}
		if err := gate.CheckSectionOrder(ctx, sellerID, design, prev); err != nil {
			return err
		}
	}

	raw, hasSlugField := design["storeSlug"].(string)
	requested := strings.ToLower(strings.TrimSpace(raw))

	var (
		slug      string
		storeName string
	)

	switch {
	case existing == nil || existing.Slug == "":
		// First time: shop name and address are both required, no cooldown.
		rawName, _ := design["storeName"].(string)
		name, nerr := normalizeStoreName(rawName)
		if nerr != nil {
			return nerr
		}
		if !hasSlugField || requested == "" {
			return apperr.Validation(map[string]string{
				"storeSlug": "Store address is required",
			})
		}
		if err := validateSlug(requested); err != nil {
			return err
		}
		slug = requested
		storeName = name

	case !hasSlugField || requested == "" || requested == strings.ToLower(strings.TrimSpace(existing.Slug)):
		// No address change: keep the current slug.
		slug = existing.Slug

	default:
		// Seller asks for a different address: same rules as ChangeSlug.
		if _, err := s.ChangeSlug(ctx, sellerID, requested); err != nil {
			return err
		}
		existing, err = s.repo.GetBySeller(ctx, sellerID)
		if err != nil {
			return err
		}
		slug = existing.Slug
	}

	// Pin the identity in the payload (the same map is published afterwards).
	design["storeSlug"] = slug
	if existing != nil && existing.StoreName != "" {
		storeName = existing.StoreName
	}
	if storeName == "" {
		// Legacy document without any name: it must get one now.
		rawName, _ := design["storeName"].(string)
		name, nerr := normalizeStoreName(rawName)
		if nerr != nil {
			return nerr
		}
		storeName = name
	}
	design["storeName"] = storeName

	// Uniqueness only matters when the address is new.
	if existing == nil || existing.Slug == "" {
		taken, err := s.repo.SlugTaken(ctx, slug, sellerID)
		if err != nil {
			return err
		}
		if taken {
			return apperr.Conflict(apperr.CodeSlugTaken, "This store address is already in use", "storeSlug")
		}
	}

	err = s.repo.UpsertDraft(ctx, repository.UpsertDraftParams{
		SellerID:  sellerID,
		Slug:      slug,
		Draft:     design,
		StoreName: storeName,
	})
	if errors.Is(err, repository.ErrSlugTaken) {
		return apperr.Conflict(apperr.CodeSlugTaken, "This store address is already in use", "storeSlug")
	}
	return err
}

// CheckNewStore validates the shop name and address a buyer picks when
// upgrading to a seller, using the same rules as the first save of a store.
// It returns the cleaned name and address. Nothing is stored.
func (s *StoreDesignService) CheckNewStore(ctx context.Context, userID, rawName, rawSlug string) (name, slug string, err error) {
	if name, err = normalizeStoreName(rawName); err != nil {
		return "", "", err
	}
	slug = strings.ToLower(strings.TrimSpace(rawSlug))
	if slug == "" {
		return "", "", apperr.Validation(map[string]string{"storeSlug": "Store address is required"})
	}
	if err = validateSlug(slug); err != nil {
		return "", "", err
	}
	taken, err := s.repo.SlugTaken(ctx, slug, userID)
	if err != nil {
		return "", "", err
	}
	if taken {
		return "", "", apperr.Conflict(apperr.CodeSlugTaken, "This store address is already in use", "storeSlug")
	}
	return name, slug, nil
}

func slugCooldownError(next time.Time) error {
	msg := fmt.Sprintf(
		"You can change your store address again on %s",
		next.UTC().Format("2 Jan 2006"),
	)
	e := apperr.New(http.StatusForbidden, apperr.CodeSlugChangeCooldown, msg)
	e.Fields = map[string]string{
		"storeSlug":        msg,
		"nextSlugChangeAt": next.UTC().Format(time.RFC3339),
	}
	return e
}

func nameCooldownError(next time.Time) error {
	msg := fmt.Sprintf(
		"Your shop name can't be changed until %s (30-day restriction)",
		next.UTC().Format("2 Jan 2006"),
	)
	e := apperr.New(http.StatusForbidden, apperr.CodeNameChangeCooldown, msg)
	e.Fields = map[string]string{
		"storeName":        msg,
		"nextNameChangeAt": next.UTC().Format(time.RFC3339),
	}
	return e
}

func nameLimitError() error {
	msg := "You have already used your one shop name change"
	e := apperr.New(http.StatusForbidden, apperr.CodeNameChangeLimit, msg)
	e.Fields = map[string]string{"storeName": msg}
	return e
}

func nameConfirmError() error {
	e := apperr.New(http.StatusPreconditionRequired, apperr.CodeNameChangeConfirm, NameChangeDisclaimer)
	e.Fields = map[string]string{"storeName": NameChangeDisclaimer}
	return e
}

// Publish copies the current draft into the published slot.
// The shop name and URL in the payload are always the stored ones.
func (s *StoreDesignService) Publish(ctx context.Context, sellerID string, design map[string]any) error {
	if err := s.SaveDraft(ctx, sellerID, design); err != nil {
		return err
	}
	return s.repo.Publish(ctx, sellerID, design)
}

// PublicStore returns the published design for a slug, or nil if not found.
// Previous (retired) slugs never match, so old store URLs stay invalid.
func (s *StoreDesignService) PublicStore(ctx context.Context, slug string) (map[string]any, error) {
	d, err := s.repo.GetPublishedBySlug(ctx, strings.ToLower(slug))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m, _ := d.Published.(map[string]any)
	return m, nil
}

// SellerIDBySlug resolves a published store slug to its seller's user ID.
// Returns "" if the store isn't published (caller treats that as 404).
func (s *StoreDesignService) SellerIDBySlug(ctx context.Context, slug string) (string, error) {
	d, err := s.repo.GetPublishedBySlug(ctx, strings.ToLower(slug))
	if errors.Is(err, repository.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.SellerID, nil
}

// CurrentSlugBySeller returns the seller's CURRENT store address ("" when
// none). Used to build links from a seller ID, so a stored address can never
// resurrect a retired URL.
func (s *StoreDesignService) CurrentSlugBySeller(ctx context.Context, sellerID string) (string, error) {
	d, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.Slug, nil
}

// ChangeSlug updates only the store address (URL). The previous address is
// retired permanently (404, never reusable, never redirected).
// Cooldown: after a successful change, the next change is blocked for 30 days.
func (s *StoreDesignService) ChangeSlug(ctx context.Context, sellerID, newSlug string) (*SlugChangeInfo, error) {
	newSlug = strings.ToLower(strings.TrimSpace(newSlug))
	if err := validateSlug(newSlug); err != nil {
		return nil, err
	}

	existing, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) || existing == nil {
		return nil, apperr.BadRequest("Create your store design before setting an address")
	}
	if err != nil {
		return nil, err
	}

	oldSlug := strings.ToLower(strings.TrimSpace(existing.Slug))
	if oldSlug == newSlug {
		info := s.slugChangeInfo(existing)
		return &info, nil
	}

	now := s.now().UTC()
	if existing.SlugChangedAt != nil && !existing.SlugChangedAt.IsZero() {
		next := existing.SlugChangedAt.Add(SlugChangeCooldown)
		if now.Before(next) {
			return nil, slugCooldownError(next)
		}
	}

	taken, err := s.repo.SlugTaken(ctx, newSlug, sellerID)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, apperr.Conflict(apperr.CodeSlugTaken, "This store address is already in use", "storeSlug")
	}

	err = s.repo.ApplyIdentityChange(ctx, repository.IdentityChangeParams{
		SellerID:     sellerID,
		OldSlug:      oldSlug,
		NewSlug:      newSlug,
		LockedBefore: now.Add(-SlugChangeCooldown),
		ChangedAt:    now,
		Published:    existing.IsPublished,
		Source:       models.SlugChangeAddress,
	})
	if err != nil {
		return nil, identityErr(err)
	}
	s.identityChanged()

	updated, err := s.repo.GetBySeller(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	info := s.slugChangeInfo(updated)
	return &info, nil
}

// ChangeNameInput is the request to rename a shop.
type ChangeNameInput struct {
	// Name is the new shop name.
	Name string
	// Slug optionally overrides the URL. When empty the URL is derived from
	// the new name. Either way the URL always changes together with the name.
	Slug string
	// AcknowledgedLock must be true: the seller saw the disclaimer that the
	// name cannot be changed again for 30 days and confirmed.
	AcknowledgedLock bool
}

// ChangeStoreName renames the shop.
//
//   - Allowed ONCE after registration (MaxNameChanges).
//   - Requires the seller's confirmation of the 30-day disclaimer.
//   - The shop URL is rebuilt from the new name immediately; the draft, the
//     published payload and stored references are all rewritten.
//   - The old URL is retired for good (404, no redirect, never reusable) and
//     the old name is not kept anywhere.
func (s *StoreDesignService) ChangeStoreName(ctx context.Context, sellerID string, in ChangeNameInput) (*SlugChangeInfo, error) {
	name, err := normalizeStoreName(in.Name)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) || existing == nil {
		return nil, apperr.BadRequest("Create your store before changing its name")
	}
	if err != nil {
		return nil, err
	}

	if existing.NameChangeCount >= MaxNameChanges {
		return nil, nameLimitError()
	}

	now := s.now().UTC()
	if existing.SlugChangedAt != nil && !existing.SlugChangedAt.IsZero() {
		next := existing.SlugChangedAt.Add(NameChangeCooldown)
		if now.Before(next) {
			return nil, nameCooldownError(next)
		}
	}

	if name == existing.StoreName {
		return nil, apperr.Validation(map[string]string{
			"storeName": "This is already your shop name",
		})
	}

	if !in.AcknowledgedLock {
		return nil, nameConfirmError()
	}

	oldSlug := strings.ToLower(strings.TrimSpace(existing.Slug))
	newSlug := strings.ToLower(strings.TrimSpace(in.Slug))
	if newSlug == "" {
		newSlug = SlugifyStoreName(name)
		if verr := validateSlug(newSlug); verr != nil {
			return nil, apperr.Validation(map[string]string{
				"storeSlug": "A web address could not be built from this name. " +
					"Enter an address using a-z, 0-9 and hyphens.",
			})
		}
	} else if verr := validateSlug(newSlug); verr != nil {
		return nil, verr
	}

	if newSlug != oldSlug {
		taken, err := s.repo.SlugTaken(ctx, newSlug, sellerID)
		if err != nil {
			return nil, err
		}
		if taken {
			return nil, apperr.Conflict(apperr.CodeSlugTaken,
				"The web address for this name is already in use. Choose a different name.", "storeName")
		}
	}

	err = s.repo.ApplyIdentityChange(ctx, repository.IdentityChangeParams{
		SellerID:       sellerID,
		OldSlug:        oldSlug,
		NewSlug:        newSlug,
		NewName:        name,
		CountName:      true,
		MaxNameChanges: MaxNameChanges,
		LockedBefore:   now.Add(-NameChangeCooldown),
		ChangedAt:      now,
		Published:      existing.IsPublished,
		Source:         models.SlugChangeRename,
	})
	if err != nil {
		return nil, identityErr(err)
	}
	s.identityChanged()

	updated, err := s.repo.GetBySeller(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	info := s.slugChangeInfo(updated)
	return &info, nil
}

func identityErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrSlugTaken):
		return apperr.Conflict(apperr.CodeSlugTaken, "This store address is already in use", "storeSlug")
	case errors.Is(err, repository.ErrIdentityConflict):
		return apperr.Conflict(apperr.CodeSlugChangeCooldown,
			"Your shop details just changed. Reload the page and try again.", "storeName")
	default:
		return err
	}
}

// normalizeStoreName trims, collapses inner whitespace and validates a shop name.
func normalizeStoreName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	n := utf8.RuneCountInString(name)
	switch {
	case n < minStoreNameLen:
		return "", apperr.Validation(map[string]string{
			"storeName": "Shop name must be at least 3 characters",
		})
	case n > maxStoreNameLen:
		return "", apperr.Validation(map[string]string{
			"storeName": "Shop name must be at most 60 characters",
		})
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", apperr.Validation(map[string]string{
				"storeName": "Shop name contains invalid characters",
			})
		}
	}
	return name, nil
}

// SlugifyStoreName builds the URL slug for a shop name: lowercase a-z, 0-9
// and single hyphens, at most 40 characters, no leading/trailing hyphen.
func SlugifyStoreName(name string) string {
	var b strings.Builder
	lastHyphen := true // swallows leading separators
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' || r == '_' || unicode.IsSpace(r):
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > maxSlugLen {
		out = strings.TrimRight(out[:maxSlugLen], "-")
	}
	return out
}

func validateSlug(slug string) error {
	if len(slug) < 3 || len(slug) > maxSlugLen {
		return apperr.Validation(map[string]string{
			"storeSlug": "Store address must be 3 to 40 characters",
		})
	}
	if !slugRe.MatchString(slug) {
		return apperr.Validation(map[string]string{
			"storeSlug": "Use only lowercase letters, numbers and hyphens",
		})
	}
	if _, reserved := reservedSlugs[slug]; reserved {
		return apperr.Validation(map[string]string{
			"storeSlug": "This store address is reserved",
		})
	}
	return nil
}
