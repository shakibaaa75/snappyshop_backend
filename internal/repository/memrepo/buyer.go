package memrepo

import (
	"context"
	"sync"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Buyers is an in-memory repository.BuyerRepository for tests.
type Buyers struct {
	mu sync.Mutex
	m  map[string]*models.BuyerProfile
}

func NewBuyers() *Buyers { return &Buyers{m: map[string]*models.BuyerProfile{}} }

func (r *Buyers) ensure(id string) *models.BuyerProfile {
	p, ok := r.m[id]
	if !ok {
		p = &models.BuyerProfile{UserID: id, Addresses: []models.BuyerAddress{}, CreatedAt: time.Now().UTC()}
		r.m[id] = p
	}
	return p
}

func (r *Buyers) Get(_ context.Context, id string) (*models.BuyerProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *p
	cp.Addresses = append([]models.BuyerAddress(nil), p.Addresses...)
	return &cp, nil
}

func (r *Buyers) SaveProfile(_ context.Context, id string, f repository.BuyerProfileFields) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.ensure(id)
	p.Phone, p.AvatarURL, p.Bio, p.Language = f.Phone, f.AvatarURL, f.Bio, f.Language
	p.UpdatedAt = time.Now().UTC()
	return nil
}

func (r *Buyers) SaveAddresses(_ context.Context, id string, addrs []models.BuyerAddress) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.ensure(id)
	p.Addresses = append([]models.BuyerAddress{}, addrs...)
	p.UpdatedAt = time.Now().UTC()
	return nil
}

func (r *Buyers) SaveHomeTheme(_ context.Context, id, theme string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.ensure(id)
	p.HomeTheme = theme
	p.UpdatedAt = time.Now().UTC()
	return nil
}
