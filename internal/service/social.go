package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/validation"
)

// Error codes for social sign-in.
const (
	// The provider gave no verified email address.
	CodeSocialEmailUnverified = "SOCIAL_EMAIL_UNVERIFIED"
	// The email already belongs to an account; the person must prove they own
	// it (password or active session) before the provider is linked.
	CodeSocialLinkRequired = "SOCIAL_LINK_REQUIRED"
	// The account (or the provider account) is already linked to a different
	// Google / Apple identity.
	CodeSocialAlreadyLinked = "SOCIAL_ALREADY_LINKED"
)

// SocialProfile is what Google or Apple confirmed about the person.
type SocialProfile struct {
	Provider      string // models.ProviderGoogle / models.ProviderApple
	Subject       string // the provider's ID for this person
	Email         string
	EmailVerified bool
	Name          string

	// AuthenticatedUserID is the account the browser is already signed in to
	// (a valid session), or "". When it is the account that owns the email,
	// the person has already proven ownership and linking needs no password.
	AuthenticatedUserID string
}

// SocialLink describes a provider identity waiting to be linked to an
// existing account once the owner proves who they are.
type SocialLink struct {
	Provider string
	Subject  string
	UserID   string
	Email    string
	Name     string
}

// LinkRequiredError is returned by LoginWithSocial when the provider's verified
// email matches an existing account. Nothing has been linked and no session has
// been created: the caller must ask the person to authenticate with the
// existing account and then call LinkSocial.
type LinkRequiredError struct{ Link SocialLink }

func (e *LinkRequiredError) Error() string {
	return CodeSocialLinkRequired + ": sign in to the existing account to connect " + e.Link.Provider
}

func socialAlreadyLinked() error {
	return apperr.New(http.StatusConflict, CodeSocialAlreadyLinked,
		"A different Google or Apple account is already connected")
}

// LoginWithSocial signs a person in after Google / Apple has vouched for them:
//
//  1. Known provider ID          -> that account. Its password is never touched.
//  2. Same, verified email       -> NOT linked automatically. A *LinkRequiredError
//     comes back; the owner must authenticate with the existing account (see
//     LinkSocial), unless this browser already has a session for that account.
//  3. Otherwise                  -> a new BUYER account (sellers go through onboarding).
//
// Admin accounts can never sign in this way: they have their own gate.
func (s *AuthService) LoginWithSocial(ctx context.Context, p SocialProfile) (*models.User, *TokenPair, error) {
	p.Subject = strings.TrimSpace(p.Subject)
	if p.Subject == "" || models.ProviderField(p.Provider) == "" {
		return nil, nil, apperr.Unauthenticated("Invalid sign-in")
	}

	u, err := s.users.FindByProvider(ctx, p.Provider, p.Subject)
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrNotFound):
		if u, err = s.firstSocialLogin(ctx, p); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, err
	}

	if u.Role == models.RoleAdmin {
		return nil, nil, apperr.Forbidden("Admins cannot sign in with Google or Apple")
	}
	if u.Status != models.StatusActive {
		return nil, nil, apperr.AccountSuspended()
	}
	pair, err := s.issueTokens(ctx, u, models.NewID(), models.NewID())
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

// providerIDOf returns the provider ID stored on the user.
func providerIDOf(u *models.User, provider string) string {
	if provider == models.ProviderGoogle {
		return u.GoogleID
	}
	return u.AppleID
}

// firstSocialLogin handles a provider ID we have not seen before.
func (s *AuthService) firstSocialLogin(ctx context.Context, p SocialProfile) (*models.User, error) {
	email := validation.NormalizeEmail(p.Email)
	// Without an email the provider has confirmed, we cannot tell whether this
	// person already has an account, and linking by an unconfirmed email would
	// let anyone take over someone else's account.
	if !p.EmailVerified || !validation.ValidEmail(email) {
		return nil, apperr.New(http.StatusForbidden, CodeSocialEmailUnverified,
			"Your account has no verified email address")
	}

	existing, err := s.users.FindByEmail(ctx, email)
	switch {
	case err == nil:
		if existing.Role == models.RoleAdmin {
			return nil, apperr.Forbidden("Admins cannot sign in with Google or Apple")
		}
		if existing.Status != models.StatusActive {
			return nil, apperr.AccountSuspended()
		}
		if providerIDOf(existing, p.Provider) != "" {
			// Already tied to another Google / Apple identity: never replace it.
			return nil, socialAlreadyLinked()
		}

		switch {
		case p.AuthenticatedUserID != "" && p.AuthenticatedUserID == existing.ID:
			// This browser is already signed in to the account: that is the
			// authentication we need. Link below.

		case !existing.EmailVerified:
			// Registered without proving the email, so whoever holds the
			// password may be a stranger who typed this address at sign-up.
			// The provider has just proven the real owner controls the email,
			// so drop the unproven password and end its sessions. (Accounts
			// with a verified email, the normal case, never reach this branch
			// and keep their password.)
			if err := s.users.UpdatePassword(ctx, existing.ID, ""); err != nil {
				return nil, err
			}
			if err := s.tokens.RevokeAllForUser(ctx, existing.ID, s.now()); err != nil {
				return nil, err
			}
			existing.PasswordHash = ""

		default:
			return nil, &LinkRequiredError{Link: SocialLink{
				Provider: p.Provider, Subject: p.Subject, UserID: existing.ID,
				Email: existing.Email, Name: strings.TrimSpace(p.Name),
			}}
		}

		if err := s.users.LinkProvider(ctx, existing.ID, p.Provider, p.Subject); err != nil {
			if errors.Is(err, repository.ErrProviderLinked) {
				return nil, socialAlreadyLinked()
			}
			return nil, err
		}
		existing.SetProviderID(p.Provider, p.Subject)
		existing.EmailVerified = true
		return existing, nil
	case !errors.Is(err, repository.ErrNotFound):
		return nil, err
	}

	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	now := s.now()
	u := &models.User{
		ID:            models.NewID(),
		Name:          name,
		Email:         email,
		EmailVerified: true,
		Role:          models.RoleBuyer,
		Status:        models.StatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	u.SetProviderID(p.Provider, p.Subject)
	if err := s.users.Create(ctx, u); err != nil {
		if errors.Is(err, repository.ErrDuplicateEmail) {
			return nil, apperr.Conflict(apperr.CodeEmailTaken, "An account with this email already exists", "email")
		}
		return nil, err
	}
	return u, nil
}

// LinkSocial finishes a link that LoginWithSocial asked for. The person proves
// they own the existing account with its password, or by already holding a
// session for it (sessionUserID, for accounts that have no password). Only
// then is the provider ID added.
//
// Linking adds one field. The password hash, role, status, products, orders and
// every other record stay exactly as they were, and the person is signed in to
// the same account as the email/password login would reach.
func (s *AuthService) LinkSocial(ctx context.Context, l SocialLink, pw, sessionUserID string) (*models.User, *TokenPair, error) {
	if l.UserID == "" || l.Subject == "" || models.ProviderField(l.Provider) == "" {
		return nil, nil, apperr.Unauthenticated("Invalid sign-in")
	}
	u, err := s.users.FindByID(ctx, l.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, apperr.Unauthenticated("Invalid sign-in")
		}
		return nil, nil, err
	}
	// The account's email must still be the one the provider vouched for.
	if u.Email != validation.NormalizeEmail(l.Email) {
		return nil, nil, apperr.Unauthenticated("Invalid sign-in")
	}

	// Prove ownership of the existing account.
	switch {
	case pw != "":
		if len(pw) > 128 || u.PasswordHash == "" {
			// No password on file: same work and same answer as a wrong one.
			_, _ = s.hasher.Verify(pw, s.dummyHash)
			return nil, nil, apperr.InvalidCredentials()
		}
		ok, err := s.hasher.Verify(pw, u.PasswordHash)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, apperr.InvalidCredentials()
		}
	case sessionUserID != "" && sessionUserID == u.ID:
		// Already signed in to this very account.
	default:
		return nil, nil, apperr.InvalidCredentials()
	}

	if u.Role == models.RoleAdmin {
		return nil, nil, apperr.Forbidden("Admins cannot sign in with Google or Apple")
	}
	if u.Status != models.StatusActive {
		return nil, nil, apperr.AccountSuspended()
	}
	if cur := providerIDOf(u, l.Provider); cur != "" && cur != l.Subject {
		return nil, nil, socialAlreadyLinked()
	}
	if err := s.users.LinkProvider(ctx, u.ID, l.Provider, l.Subject); err != nil {
		if errors.Is(err, repository.ErrProviderLinked) {
			return nil, nil, socialAlreadyLinked()
		}
		return nil, nil, err
	}
	u.SetProviderID(l.Provider, l.Subject)
	u.EmailVerified = true

	pair, err := s.issueTokens(ctx, u, models.NewID(), models.NewID())
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}
