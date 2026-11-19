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

// Error code returned when the provider gave no verified email address.
const CodeSocialEmailUnverified = "SOCIAL_EMAIL_UNVERIFIED"

// SocialProfile is what Google or Apple confirmed about the person.
type SocialProfile struct {
	Provider      string // models.ProviderGoogle / models.ProviderApple
	Subject       string // the provider's ID for this person
	Email         string
	EmailVerified bool
	Name          string
}

// LoginWithSocial signs a person in after Google / Apple has vouched for them:
//
//  1. Known provider ID          -> that account.
//  2. Same, verified email       -> the existing account gets the provider linked.
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
		if !existing.EmailVerified {
			// This account was made without proving the email, so its password
			// may belong to someone else who typed this address at sign-up.
			// Drop that password and end its sessions before linking.
			if err := s.users.UpdatePassword(ctx, existing.ID, ""); err != nil {
				return nil, err
			}
			if err := s.tokens.RevokeAllForUser(ctx, existing.ID, s.now()); err != nil {
				return nil, err
			}
			existing.PasswordHash = ""
		}
		if err := s.users.LinkProvider(ctx, existing.ID, p.Provider, p.Subject); err != nil {
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
