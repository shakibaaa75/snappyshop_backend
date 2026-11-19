package memrepo

import (
	"context"
	"sync"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// PasswordResets is an in-memory repository.PasswordResetRepository.
type PasswordResets struct {
	mu     sync.Mutex
	byID   map[string]*models.PasswordReset
	byUser map[string]string // userID -> reset ID
}

func NewPasswordResets() *PasswordResets {
	return &PasswordResets{
		byID:   map[string]*models.PasswordReset{},
		byUser: map[string]string{},
	}
}

func (r *PasswordResets) Create(_ context.Context, pr *models.PasswordReset) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *pr
	r.byID[pr.ID] = &cp
	r.byUser[pr.UserID] = pr.ID
	return nil
}

func (r *PasswordResets) live(id string) (*models.PasswordReset, bool) {
	pr, ok := r.byID[id]
	if !ok || !pr.ExpiresAt.After(time.Now()) {
		return nil, false
	}
	return pr, true
}

func (r *PasswordResets) FindByUserID(_ context.Context, userID string) (*models.PasswordReset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr, ok := r.live(r.byUser[userID])
	if !ok {
		return nil, repository.ErrResetNotFound
	}
	cp := *pr
	return &cp, nil
}

func (r *PasswordResets) IncrementAttempts(_ context.Context, id string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr, ok := r.live(id)
	if !ok {
		return 0, repository.ErrResetNotFound
	}
	pr.Attempts++
	return pr.Attempts, nil
}

func (r *PasswordResets) Consume(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr, ok := r.live(id)
	if !ok {
		return false, nil
	}
	delete(r.byID, id)
	delete(r.byUser, pr.UserID)
	return true, nil
}

func (r *PasswordResets) DeleteByUserID(_ context.Context, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.byUser[userID]; ok {
		delete(r.byID, id)
		delete(r.byUser, userID)
	}
	return nil
}
