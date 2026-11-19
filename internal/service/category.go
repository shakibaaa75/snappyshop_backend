package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type CategoryService struct {
	repo repository.CategoryRepository
	// imageOK, when set, decides whether a seller may use an image address.
	// Wired when R2 is on: only images this seller uploaded are accepted.
	imageOK func(sellerID, url string) bool
	// imageGone, when set, deletes a category picture from storage after it
	// was replaced, removed, or its category was deleted. Best effort.
	imageGone func(ctx context.Context, sellerID, url string)
}

// SetImagePolicy restricts category image addresses (see imageOK).
func (s *CategoryService) SetImagePolicy(f func(sellerID, url string) bool) { s.imageOK = f }

// SetImageRemover sets the function that deletes an unused picture from
// storage. It must ignore addresses that are not ours (old rows, data: URLs).
func (s *CategoryService) SetImageRemover(f func(ctx context.Context, sellerID, url string)) {
	s.imageGone = f
}

func (s *CategoryService) dropImage(ctx context.Context, sellerID, url string) {
	if s.imageGone == nil || url == "" {
		return
	}
	// The category change is already saved; never let a storage hiccup undo it.
	s.imageGone(context.WithoutCancel(ctx), sellerID, url)
}

func NewCategoryService(repo repository.CategoryRepository) *CategoryService {
	return &CategoryService{repo: repo}
}

type CategoryInput struct {
	Name     string
	Slug     string
	Emoji    string
	ImageURL string
	// RemoveImage clears the current picture. An empty ImageURL alone still
	// means "keep the current picture".
	RemoveImage bool
	Position    int
}

func (s *CategoryService) Create(ctx context.Context, sellerID string, in CategoryInput) (*models.Category, error) {
	if err := validateCategory(in); err != nil {
		return nil, err
	}
	if err := s.checkImage(sellerID, in.ImageURL); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	c := &models.Category{
		ID:        models.NewID(),
		SellerID:  sellerID,
		Name:      strings.TrimSpace(in.Name),
		Slug:      strings.ToLower(strings.TrimSpace(in.Slug)),
		Emoji:     strings.TrimSpace(in.Emoji),
		ImageURL:  strings.TrimSpace(in.ImageURL),
		Position:  in.Position,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if c.Emoji == "" {
		c.Emoji = "🏷️"
	}
	if err := s.repo.Create(ctx, c); err != nil {
		if errors.Is(err, repository.ErrSlugTaken) {
			return nil, apperr.Conflict(apperr.CodeSlugTaken, "You already have a category with this name", "slug")
		}
		return nil, err
	}
	return c, nil
}

func (s *CategoryService) List(ctx context.Context, sellerID string) ([]*models.Category, error) {
	return s.repo.List(ctx, sellerID)
}

func (s *CategoryService) Update(ctx context.Context, sellerID, id string, in CategoryInput) (*models.Category, error) {
	if err := validateCategory(in); err != nil {
		return nil, err
	}
	if in.RemoveImage && strings.TrimSpace(in.ImageURL) != "" {
		return nil, apperr.Validation(map[string]string{"imageUrl": "Send either a new image or remove the image, not both"})
	}
	existing, err := s.repo.FindByID(ctx, sellerID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	oldImage := existing.ImageURL
	newImage := strings.TrimSpace(in.ImageURL)
	// Sending back the picture the row already has is allowed, so rows that
	// predate the ownership rule can still be renamed.
	if newImage != "" && newImage != oldImage {
		if err := s.checkImage(sellerID, newImage); err != nil {
			return nil, err
		}
	}

	existing.Name = strings.TrimSpace(in.Name)
	existing.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if e := strings.TrimSpace(in.Emoji); e != "" {
		existing.Emoji = e // empty means keep, like the picture
	}
	switch {
	case in.RemoveImage:
		existing.ImageURL = ""
	case newImage != "":
		existing.ImageURL = newImage
	} // otherwise: empty means keep the current image
	existing.Position = in.Position
	existing.UpdatedAt = time.Now().UTC()
	if err := s.repo.Update(ctx, sellerID, id, existing); err != nil {
		if errors.Is(err, repository.ErrSlugTaken) {
			return nil, apperr.Conflict(apperr.CodeSlugTaken, "You already have a category with this name", "slug")
		}
		return nil, err
	}
	// Only after the row is saved: free the file that is no longer used.
	if oldImage != "" && oldImage != existing.ImageURL {
		s.dropImage(ctx, sellerID, oldImage)
	}
	return existing, nil
}

func (s *CategoryService) Delete(ctx context.Context, sellerID, id string) error {
	// Read the row first so its picture can be removed after it is gone.
	var image string
	if existing, err := s.repo.FindByID(ctx, sellerID, id); err == nil {
		image = existing.ImageURL
	}
	if err := s.repo.Delete(ctx, sellerID, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	s.dropImage(ctx, sellerID, image)
	return nil
}

// checkImage applies the ownership rule to a new picture address. With no
// policy set (R2 off) nothing is checked beyond validateCategory.
func (s *CategoryService) checkImage(sellerID, url string) error {
	url = strings.TrimSpace(url)
	if url == "" || s.imageOK == nil || s.imageOK(sellerID, url) {
		return nil
	}
	return apperr.Validation(map[string]string{"imageUrl": "Use a picture you uploaded"})
}

func validateCategory(in CategoryInput) error {
	fields := map[string]string{}
	name := strings.TrimSpace(in.Name)
	if len(name) < 2 {
		fields["name"] = "Category name is required"
	} else if len(name) > 40 {
		fields["name"] = "Category name must be at most 40 characters"
	}
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if len(slug) < 2 {
		fields["slug"] = "Category slug is required"
	} else if !productSlugRe.MatchString(slug) {
		fields["slug"] = "Use only lowercase letters, numbers and hyphens"
	}
	// Pictures live in R2; a row only stores the https address. data: URLs
	// would put the image bytes into the database, so they are refused.
	if img := strings.TrimSpace(in.ImageURL); img != "" {
		if !strings.HasPrefix(img, "https://") {
			fields["imageUrl"] = "Invalid image"
		} else if len(img) > 2048 {
			fields["imageUrl"] = "Invalid image"
		}
	}
	if len(fields) > 0 {
		return apperr.Validation(fields)
	}
	return nil
}
