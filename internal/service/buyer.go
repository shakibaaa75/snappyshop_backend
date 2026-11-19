package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

const (
	buyerMaxAddresses = 10
	buyerMaxName      = 80
	buyerMaxBio       = 160
	buyerMaxArea      = 200
	buyerMaxPlace     = 60
	buyerMaxAvatarURL = 500
)

// BangladeshDivisions are the eight divisions an address may use.
var BangladeshDivisions = []string{
	"Barishal", "Chattogram", "Dhaka", "Khulna",
	"Mymensingh", "Rajshahi", "Rangpur", "Sylhet",
}

var addressLabels = map[string]bool{"Home": true, "Office": true, "Other": true}

// BuyerService is the buyer's own profile and saved addresses. The user ID
// always comes from the sign-in claims, never from the request.
type BuyerService struct {
	buyers repository.BuyerRepository
	users  repository.UserRepository
	now    func() time.Time
}

func NewBuyerService(buyers repository.BuyerRepository, users repository.UserRepository) *BuyerService {
	return &BuyerService{buyers: buyers, users: users, now: time.Now}
}

// BuyerProfileView is the profile as the buyer sees it.
type BuyerProfileView struct {
	Name         string
	Email        string
	Phone        string
	AvatarURL    string
	Bio          string
	Language     string
	HomeTheme    string
	MemberSince  time.Time
	AddressCount int
}

type UpdateBuyerProfileInput struct {
	Name      string
	Phone     string
	AvatarURL string
	Bio       string
	Language  string
}

type BuyerAddressInput struct {
	Label      string
	FullName   string
	Phone      string
	Division   string
	District   string
	Upazila    string
	Area       string
	PostalCode string
	IsDefault  bool
}

// ------------------------------------------------------------ profile

func (s *BuyerService) load(ctx context.Context, userID string) (*models.User, *models.BuyerProfile, error) {
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, apperr.NotFound()
		}
		return nil, nil, err
	}
	p, err := s.buyers.Get(ctx, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return u, &models.BuyerProfile{UserID: userID, Addresses: []models.BuyerAddress{}}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return u, p, nil
}

// loadProfile reads only the buyer document (addresses never need the user).
func (s *BuyerService) loadProfile(ctx context.Context, userID string) (*models.BuyerProfile, error) {
	p, err := s.buyers.Get(ctx, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return &models.BuyerProfile{UserID: userID, Addresses: []models.BuyerAddress{}}, nil
	}
	return p, err
}

func (s *BuyerService) view(u *models.User, p *models.BuyerProfile) *BuyerProfileView {
	lang := p.Language
	if lang == "" {
		lang = models.LangEnglish
	}
	theme := p.HomeTheme
	if theme != models.ThemeDark {
		theme = models.ThemeLight
	}
	return &BuyerProfileView{
		Name: u.Name, Email: u.Email, Phone: p.Phone, AvatarURL: p.AvatarURL,
		Bio: p.Bio, Language: lang, HomeTheme: theme, MemberSince: u.CreatedAt, AddressCount: len(p.Addresses),
	}
}

func (s *BuyerService) Profile(ctx context.Context, userID string) (*BuyerProfileView, error) {
	u, p, err := s.load(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.view(u, p), nil
}

func (s *BuyerService) UpdateProfile(ctx context.Context, userID string, in UpdateBuyerProfileInput) (*BuyerProfileView, error) {
	u, p, err := s.load(ctx, userID)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}

	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 2 {
		fields["name"] = "Name must be at least 2 characters"
	} else if n > buyerMaxName {
		fields["name"] = "Name is too long"
	}

	phone := strings.TrimSpace(in.Phone)
	if phone != "" {
		if np, ok := NormalizeBDPhone(phone); ok {
			phone = np
		} else {
			fields["phone"] = "Enter a valid Bangladesh mobile number (01XXXXXXXXX)"
		}
	}

	bio := strings.TrimSpace(in.Bio)
	if utf8.RuneCountInString(bio) > buyerMaxBio {
		fields["bio"] = "Bio is too long"
	}

	avatar := strings.TrimSpace(in.AvatarURL)
	if avatar != "" && !validAvatarURL(avatar) {
		fields["avatarUrl"] = "Avatar must be an https:// image address"
	}

	lang := strings.TrimSpace(in.Language)
	switch lang {
	case "":
		lang = models.LangEnglish
	case models.LangEnglish, models.LangBangla:
	default:
		fields["language"] = "Must be en or bn"
	}
	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}

	if name != u.Name {
		if err := s.users.UpdateProfile(ctx, userID, name, ""); err != nil {
			return nil, err
		}
		u.Name = name
	}
	f := repository.BuyerProfileFields{Phone: phone, AvatarURL: avatar, Bio: bio, Language: lang}
	if err := s.buyers.SaveProfile(ctx, userID, f); err != nil {
		return nil, err
	}
	p.Phone, p.AvatarURL, p.Bio, p.Language = phone, avatar, bio, lang
	return s.view(u, p), nil
}

// SetHomeTheme saves how the buyer wants the home page to look (light or dark).
// It changes nothing else on the profile and returns the saved value.
func (s *BuyerService) SetHomeTheme(ctx context.Context, userID, theme string) (string, error) {
	theme = strings.TrimSpace(theme)
	if theme != models.ThemeLight && theme != models.ThemeDark {
		return "", apperr.Validation(map[string]string{"homeTheme": "Must be light or dark"})
	}
	if err := s.buyers.SaveHomeTheme(ctx, userID, theme); err != nil {
		return "", err
	}
	return theme, nil
}

// ---------------------------------------------------------- addresses

func (s *BuyerService) Addresses(ctx context.Context, userID string) ([]models.BuyerAddress, error) {
	p, err := s.loadProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	return p.Addresses, nil
}

func (s *BuyerService) AddAddress(ctx context.Context, userID string, in BuyerAddressInput) (*models.BuyerAddress, error) {
	a, err := s.buildAddress(in)
	if err != nil {
		return nil, err
	}
	p, err := s.loadProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(p.Addresses) >= buyerMaxAddresses {
		return nil, apperr.Conflict("address_limit", "You can save up to 10 addresses", "")
	}
	a.ID = models.NewID()
	a.CreatedAt = s.now().UTC()
	// The first address is always the default.
	a.IsDefault = in.IsDefault || len(p.Addresses) == 0
	list := append([]models.BuyerAddress{}, p.Addresses...)
	if a.IsDefault {
		clearDefault(list)
	}
	list = append(list, *a)
	if err := s.buyers.SaveAddresses(ctx, userID, list); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *BuyerService) UpdateAddress(ctx context.Context, userID, id string, in BuyerAddressInput) (*models.BuyerAddress, error) {
	a, err := s.buildAddress(in)
	if err != nil {
		return nil, err
	}
	p, err := s.loadProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	list := append([]models.BuyerAddress{}, p.Addresses...)
	i := indexOfAddress(list, id)
	if i < 0 {
		return nil, apperr.NotFound()
	}
	a.ID, a.CreatedAt = list[i].ID, list[i].CreatedAt
	a.IsDefault = list[i].IsDefault
	if in.IsDefault {
		clearDefault(list)
		a.IsDefault = true
	}
	list[i] = *a
	if err := s.buyers.SaveAddresses(ctx, userID, list); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *BuyerService) SetDefaultAddress(ctx context.Context, userID, id string) error {
	p, err := s.loadProfile(ctx, userID)
	if err != nil {
		return err
	}
	list := append([]models.BuyerAddress{}, p.Addresses...)
	i := indexOfAddress(list, id)
	if i < 0 {
		return apperr.NotFound()
	}
	clearDefault(list)
	list[i].IsDefault = true
	return s.buyers.SaveAddresses(ctx, userID, list)
}

func (s *BuyerService) DeleteAddress(ctx context.Context, userID, id string) error {
	p, err := s.loadProfile(ctx, userID)
	if err != nil {
		return err
	}
	i := indexOfAddress(p.Addresses, id)
	if i < 0 {
		return apperr.NotFound()
	}
	wasDefault := p.Addresses[i].IsDefault
	list := make([]models.BuyerAddress, 0, len(p.Addresses))
	list = append(list, p.Addresses[:i]...)
	list = append(list, p.Addresses[i+1:]...)
	// Deleting the default hands the default to the oldest remaining address.
	if wasDefault && len(list) > 0 {
		list[0].IsDefault = true
	}
	return s.buyers.SaveAddresses(ctx, userID, list)
}

// ------------------------------------------------------------ helpers

func (s *BuyerService) buildAddress(in BuyerAddressInput) (*models.BuyerAddress, error) {
	fields := map[string]string{}
	trim := strings.TrimSpace

	label := trim(in.Label)
	if label == "" {
		label = "Home"
	}
	if !addressLabels[label] {
		fields["label"] = "Must be Home, Office or Other"
	}
	name := trim(in.FullName)
	if n := utf8.RuneCountInString(name); n < 2 || n > buyerMaxName {
		fields["fullName"] = "Enter the receiver's name"
	}
	phone, ok := NormalizeBDPhone(in.Phone)
	if !ok {
		fields["phone"] = "Enter a valid Bangladesh mobile number (01XXXXXXXXX)"
	}
	division := canonicalDivision(in.Division)
	if division == "" {
		fields["division"] = "Choose one of the 8 divisions"
	}
	required := func(key, v, msg string, max int) string {
		v = trim(v)
		if n := utf8.RuneCountInString(v); n == 0 {
			fields[key] = msg
		} else if n > max {
			fields[key] = "Too long"
		}
		return v
	}
	district := required("district", in.District, "District is required", buyerMaxPlace)
	upazila := required("upazila", in.Upazila, "Upazila / thana is required", buyerMaxPlace)
	area := required("area", in.Area, "Address is required", buyerMaxArea)

	postal := trim(in.PostalCode)
	if postal != "" && !isDigits(postal, 4) {
		fields["postalCode"] = "Postal code is 4 digits"
	}
	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}
	return &models.BuyerAddress{
		Label: label, FullName: name, Phone: phone, Division: division,
		District: district, Upazila: upazila, Area: area, PostalCode: postal,
	}, nil
}

// NormalizeBDPhone accepts 01XXXXXXXXX, 8801XXXXXXXXX and +8801XXXXXXXXX
// (spaces and dashes allowed) and returns 01XXXXXXXXX. The operator digit
// after 01 must be 3 to 9.
func NormalizeBDPhone(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '+':
		default:
			return "", false
		}
	}
	d := b.String()
	d = strings.TrimPrefix(d, "880")
	if len(d) == 10 && d[0] == '1' {
		d = "0" + d
	}
	if len(d) != 11 || !strings.HasPrefix(d, "01") || d[2] < '3' || d[2] > '9' {
		return "", false
	}
	return d, true
}

func canonicalDivision(v string) string {
	v = strings.TrimSpace(v)
	for _, d := range BangladeshDivisions {
		if strings.EqualFold(d, v) {
			return d
		}
	}
	return ""
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validAvatarURL(v string) bool {
	if utf8.RuneCountInString(v) > buyerMaxAvatarURL {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func indexOfAddress(list []models.BuyerAddress, id string) int {
	for i := range list {
		if list[i].ID == id {
			return i
		}
	}
	return -1
}

func clearDefault(list []models.BuyerAddress) {
	for i := range list {
		list[i].IsDefault = false
	}
}
