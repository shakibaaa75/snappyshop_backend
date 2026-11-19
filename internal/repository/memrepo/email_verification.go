package memrepo

import (
	"context"
	"sync"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// EmailVerifications is an in-memory repository.EmailVerificationRepository.
type EmailVerifications struct {
	mu      sync.Mutex
	byEmail map[string]*models.EmailVerification
}

func NewEmailVerifications() *EmailVerifications {
	return &EmailVerifications{byEmail: map[string]*models.EmailVerification{}}
}

func (r *EmailVerifications) Replace(_ context.Context, v *models.EmailVerification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *v
	cp.Attempts = 0
	r.byEmail[v.Email] = &cp
	return nil
}

func (r *EmailVerifications) FindByEmail(_ context.Context, email string) (*models.EmailVerification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byEmail[email]
	if !ok || !v.ExpiresAt.After(time.Now()) {
		return nil, repository.ErrVerificationNotFound
	}
	cp := *v
	return &cp, nil
}

func (r *EmailVerifications) IncrementAttempts(_ context.Context, email string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byEmail[email]
	if !ok {
		return 0, repository.ErrVerificationNotFound
	}
	v.Attempts++
	return v.Attempts, nil
}

func (r *EmailVerifications) DeleteByEmail(_ context.Context, email string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byEmail, email)
	return nil
}
