// Package memrepo provides in-memory repositories used by unit and handler
// tests. It is not used by the running server.
package memrepo

import (
	"context"
	"sync"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type Users struct {
	mu      sync.Mutex
	byID    map[string]*models.User
	byEmail map[string]string
}

func NewUsers() *Users {
	return &Users{byID: map[string]*models.User{}, byEmail: map[string]string{}}
}

func (r *Users) Create(_ context.Context, u *models.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byEmail[u.Email]; ok {
		return repository.ErrDuplicateEmail
	}
	cp := *u
	r.byID[u.ID] = &cp
	r.byEmail[u.Email] = u.ID
	return nil
}

func (r *Users) FindByEmail(_ context.Context, email string) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byEmail[email]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *r.byID[id]
	return &cp, nil
}

func (r *Users) FindByID(_ context.Context, id string) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

// FindByProvider finds the user linked to this Google / Apple account.
func (r *Users) FindByProvider(_ context.Context, provider, subject string) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if models.ProviderField(provider) == "" || subject == "" {
		return nil, repository.ErrNotFound
	}
	for _, u := range r.byID {
		if (provider == models.ProviderGoogle && u.GoogleID == subject) ||
			(provider == models.ProviderApple && u.AppleID == subject) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

// LinkProvider attaches a Google / Apple account and marks the email verified.
// Like the Mongo version it never touches the password and refuses to replace
// a different linked account or take one that belongs to another user.
func (r *Users) LinkProvider(_ context.Context, userID, provider, subject string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok || models.ProviderField(provider) == "" || subject == "" {
		return repository.ErrNotFound
	}
	cur := func(x *models.User) string {
		if provider == models.ProviderGoogle {
			return x.GoogleID
		}
		return x.AppleID
	}
	if c := cur(u); c != "" && c != subject {
		return repository.ErrProviderLinked
	}
	for _, o := range r.byID {
		if o.ID != userID && cur(o) == subject {
			return repository.ErrProviderLinked
		}
	}
	u.SetProviderID(provider, subject)
	u.EmailVerified = true
	return nil
}

// SetStatus is a test helper.
func (r *Users) SetStatus(id string, s models.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.byID[id]; ok {
		u.Status = s
	}
}

type Tokens struct {
	mu     sync.Mutex
	byID   map[string]*models.RefreshToken
	byHash map[string]string
}

func NewTokens() *Tokens {
	return &Tokens{byID: map[string]*models.RefreshToken{}, byHash: map[string]string{}}
}

func (r *Tokens) Create(_ context.Context, t *models.RefreshToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *t
	r.byID[t.ID] = &cp
	r.byHash[t.TokenHash] = t.ID
	return nil
}

func (r *Tokens) FindByHash(_ context.Context, hash string) (*models.RefreshToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byHash[hash]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *r.byID[id]
	return &cp, nil
}

func (r *Tokens) Revoke(_ context.Context, id, replacedBy string, at time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok || t.RevokedAt != nil {
		return false, nil
	}
	t.RevokedAt = &at
	t.ReplacedBy = replacedBy
	return true, nil
}

func (r *Tokens) RevokeFamily(_ context.Context, familyID string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.byID {
		if t.FamilyID == familyID && t.RevokedAt == nil {
			ts := at
			t.RevokedAt = &ts
		}
	}
	return nil
}

func (r *Tokens) RevokeAllForUser(_ context.Context, userID string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.byID {
		if t.UserID == userID && t.RevokedAt == nil {
			ts := at
			t.RevokedAt = &ts
		}
	}
	return nil
}

// ActiveInFamily is a test helper counting non-revoked tokens in a family.
func (r *Tokens) ActiveInFamily(familyID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.byID {
		if t.FamilyID == familyID && t.RevokedAt == nil {
			n++
		}
	}
	return n
}

// UpdatePassword stores the new hash, like the Mongo implementation does.
// An empty hash removes the password (used when a social login takes over an
// account whose email was never verified).
func (r *Users) UpdatePassword(_ context.Context, userID, passwordHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	u.PasswordHash = passwordHash
	return nil
}

func (r *Users) UpdateStatus(_ context.Context, userID string, status models.Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	u.Status = status
	return nil
}

func (r *Users) UpdateProfile(_ context.Context, userID, name, email string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	if name != "" {
		u.Name = name
	}
	if email != "" {
		delete(r.byEmail, u.Email)
		u.Email = email
		r.byEmail[email] = userID
	}
	return nil
}

func (r *Users) CountByRole(_ context.Context, role models.Role) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, u := range r.byID {
		if u.Role == role {
			n++
		}
	}
	return n, nil
}

func (r *Users) CountByRoleAndStatus(_ context.Context, role models.Role, status models.Status) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, u := range r.byID {
		if u.Role == role && u.Status == status {
			n++
		}
	}
	return n, nil
}

func (r *Users) ListByRole(_ context.Context, filter repository.UserListFilter) ([]*models.User, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*models.User
	for _, u := range r.byID {
		if filter.Role != "" && u.Role != filter.Role {
			continue
		}
		if filter.Status != nil && u.Status != *filter.Status {
			continue
		}
		cp := *u
		all = append(all, &cp)
	}
	total := int64(len(all))
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	skip := filter.Skip
	if skip < 0 {
		skip = 0
	}
	if skip > len(all) {
		return nil, total, nil
	}
	end := skip + limit
	if end > len(all) {
		end = len(all)
	}
	return all[skip:end], total, nil
}

func (r *Users) UpdateRole(_ context.Context, userID string, role models.Role) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	u.Role = role
	return nil
}

// ChangeEmail mirrors the Mongo version: it only applies while the email is
// still oldEmail and refuses an address another account holds.
func (r *Users) ChangeEmail(_ context.Context, userID, oldEmail, newEmail string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	if u.Email != oldEmail {
		return repository.ErrEmailChanged
	}
	if other, taken := r.byEmail[newEmail]; taken && other != userID {
		return repository.ErrDuplicateEmail
	}
	delete(r.byEmail, u.Email)
	u.Email = newEmail
	u.EmailVerified = true
	r.byEmail[newEmail] = userID
	return nil
}

func (r *Users) MarkEmailVerified(_ context.Context, userID, email string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	if u.Email != email {
		return repository.ErrEmailChanged
	}
	u.EmailVerified = true
	return nil
}

func (r *Users) ChangePhone(_ context.Context, userID, phone string, verified bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	if verified {
		for _, o := range r.byID {
			if o.ID != userID && o.Phone == phone && o.PhoneVerified {
				return repository.ErrDuplicatePhone
			}
		}
	}
	u.Phone, u.PhoneVerified = phone, verified
	return nil
}

func (r *Users) PhoneInUse(_ context.Context, phone, exceptUserID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.byID {
		if o.ID != exceptUserID && o.Phone == phone && o.PhoneVerified {
			return true, nil
		}
	}
	return false, nil
}

func (r *Users) SetAvatar(_ context.Context, userID, url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	u.AvatarURL = url
	return nil
}
