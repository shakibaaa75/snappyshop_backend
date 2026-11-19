package service

import (
	"context"
	"strings"
	"testing"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// fakeCategoryRepo is a minimal in-memory CategoryRepository.
type fakeCategoryRepo struct{ rows map[string]*models.Category }

func newFakeCategoryRepo() *fakeCategoryRepo {
	return &fakeCategoryRepo{rows: map[string]*models.Category{}}
}

func (f *fakeCategoryRepo) Create(_ context.Context, c *models.Category) error {
	cp := *c
	f.rows[c.ID] = &cp
	return nil
}
func (f *fakeCategoryRepo) List(context.Context, string) ([]*models.Category, error) { return nil, nil }
func (f *fakeCategoryRepo) FindByID(_ context.Context, sellerID, id string) (*models.Category, error) {
	c, ok := f.rows[id]
	if !ok || c.SellerID != sellerID {
		return nil, repository.ErrNotFound
	}
	cp := *c
	return &cp, nil
}
func (f *fakeCategoryRepo) Update(_ context.Context, sellerID, id string, c *models.Category) error {
	if _, ok := f.rows[id]; !ok {
		return repository.ErrNotFound
	}
	cp := *c
	f.rows[id] = &cp
	return nil
}
func (f *fakeCategoryRepo) Delete(_ context.Context, sellerID, id string) error {
	if _, ok := f.rows[id]; !ok {
		return repository.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

const catHost = "https://cdn.example.com/catagory_image/"

func catURL(owner, ch string) string {
	return catHost + owner + "/" + strings.Repeat(ch, 32) + ".webp"
}

func newCategoryTestService() (*CategoryService, *fakeCategoryRepo, *[]string) {
	repo := newFakeCategoryRepo()
	s := NewCategoryService(repo)
	var removed []string
	s.SetImagePolicy(func(seller, u string) bool {
		return strings.HasPrefix(u, catHost+seller+"/")
	})
	s.SetImageRemover(func(_ context.Context, _ string, u string) { removed = append(removed, u) })
	return s, repo, &removed
}

func TestCategoryRejectsDataURLs(t *testing.T) {
	s, _, _ := newCategoryTestService()
	ctx := context.Background()
	in := CategoryInput{Name: "Shoes", Slug: "shoes", ImageURL: "data:image/png;base64,AAAA"}
	if _, err := s.Create(ctx, "sellerA", in); err == nil {
		t.Fatal("create accepted a data: URL")
	}
	c, err := s.Create(ctx, "sellerA", CategoryInput{Name: "Shoes", Slug: "shoes"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "sellerA", c.ID, in); err == nil {
		t.Fatal("update accepted a data: URL")
	}
	if _, err := s.Create(ctx, "sellerA", CategoryInput{Name: "Bags", Slug: "bags", ImageURL: "http://x/a.png"}); err == nil {
		t.Fatal("create accepted a non-https URL")
	}
}

func TestCategoryImageOwnership(t *testing.T) {
	s, _, _ := newCategoryTestService()
	ctx := context.Background()
	if _, err := s.Create(ctx, "sellerA", CategoryInput{Name: "Mine", Slug: "mine", ImageURL: catURL("sellerA", "a")}); err != nil {
		t.Fatalf("own image rejected: %v", err)
	}
	if _, err := s.Create(ctx, "sellerA", CategoryInput{Name: "Theirs", Slug: "theirs", ImageURL: catURL("sellerB", "a")}); err == nil {
		t.Fatal("another seller's image accepted")
	}
}

func TestCategoryUpdateKeepReplaceRemove(t *testing.T) {
	s, repo, removed := newCategoryTestService()
	ctx := context.Background()
	first, second := catURL("sellerA", "a"), catURL("sellerA", "b")
	c, err := s.Create(ctx, "sellerA", CategoryInput{Name: "Shoes", Slug: "shoes", ImageURL: first})
	if err != nil {
		t.Fatal(err)
	}

	// Empty imageUrl keeps the picture and deletes nothing.
	if _, err := s.Update(ctx, "sellerA", c.ID, CategoryInput{Name: "Sneakers", Slug: "sneakers"}); err != nil {
		t.Fatal(err)
	}
	if repo.rows[c.ID].ImageURL != first || len(*removed) != 0 {
		t.Fatalf("keep failed: image=%q removed=%v", repo.rows[c.ID].ImageURL, *removed)
	}

	// Replacing deletes the old file only.
	if _, err := s.Update(ctx, "sellerA", c.ID, CategoryInput{Name: "Sneakers", Slug: "sneakers", ImageURL: second}); err != nil {
		t.Fatal(err)
	}
	if repo.rows[c.ID].ImageURL != second || len(*removed) != 1 || (*removed)[0] != first {
		t.Fatalf("replace failed: image=%q removed=%v", repo.rows[c.ID].ImageURL, *removed)
	}

	// Remove clears the row and deletes the file.
	if _, err := s.Update(ctx, "sellerA", c.ID, CategoryInput{Name: "Sneakers", Slug: "sneakers", RemoveImage: true}); err != nil {
		t.Fatal(err)
	}
	if repo.rows[c.ID].ImageURL != "" || len(*removed) != 2 || (*removed)[1] != second {
		t.Fatalf("remove failed: image=%q removed=%v", repo.rows[c.ID].ImageURL, *removed)
	}

	// New image and remove together is a client mistake.
	if _, err := s.Update(ctx, "sellerA", c.ID, CategoryInput{Name: "Sneakers", Slug: "sneakers", ImageURL: first, RemoveImage: true}); err == nil {
		t.Fatal("image + removeImage accepted")
	}
}

func TestCategoryDeleteRemovesImage(t *testing.T) {
	s, _, removed := newCategoryTestService()
	ctx := context.Background()
	u := catURL("sellerA", "c")
	c, err := s.Create(ctx, "sellerA", CategoryInput{Name: "Hats", Slug: "hats", ImageURL: u})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "sellerA", c.ID); err != nil {
		t.Fatal(err)
	}
	if len(*removed) != 1 || (*removed)[0] != u {
		t.Fatalf("delete did not remove the picture: %v", *removed)
	}
}
