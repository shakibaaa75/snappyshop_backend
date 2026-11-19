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
	notificationMaxTitle      = 120
	notificationMaxBody       = 2000
	notificationMaxLink       = 300
	notificationMaxRecipients = 200
	// How many notifications a seller sees at most (and how many one
	// "mark all read" can touch).
	notificationSellerWindow = 50
)

// NotificationService lets admins send notifications to sellers and lets
// sellers read them.
type NotificationService struct {
	repo  repository.NotificationRepository
	users repository.UserRepository
	logs  repository.AdminActivityLogRepository
	now   func() time.Time
}

func NewNotificationService(
	repo repository.NotificationRepository,
	users repository.UserRepository,
	logs repository.AdminActivityLogRepository,
) *NotificationService {
	return &NotificationService{repo: repo, users: users, logs: logs, now: time.Now}
}

// ---------------------------------------------------------------- admin

type SendNotificationInput struct {
	Title     string
	Body      string
	Level     string
	LinkURL   string
	Audience  string // all | selected
	SellerIDs []string
	ExpiresAt *time.Time
	AdminID   string
	IP        string
}

// Send validates and stores a notification. Sellers see it on their next load.
func (s *NotificationService) Send(ctx context.Context, in SendNotificationInput) (*models.Notification, error) {
	fields := map[string]string{}

	title := strings.TrimSpace(in.Title)
	switch n := utf8.RuneCountInString(title); {
	case n == 0:
		fields["title"] = "Title is required"
	case n > notificationMaxTitle:
		fields["title"] = "Title is too long"
	}

	body := strings.TrimSpace(in.Body)
	switch n := utf8.RuneCountInString(body); {
	case n == 0:
		fields["body"] = "Message is required"
	case n > notificationMaxBody:
		fields["body"] = "Message is too long"
	}

	level := strings.TrimSpace(in.Level)
	if level == "" {
		level = models.NotificationInfo
	}
	switch level {
	case models.NotificationInfo, models.NotificationSuccess, models.NotificationWarning, models.NotificationUrgent:
	default:
		fields["level"] = "Must be info, success, warning or urgent"
	}

	link := strings.TrimSpace(in.LinkURL)
	if link != "" && !validNotificationLink(link) {
		fields["linkUrl"] = "Use a path starting with / or a full https:// address"
	}

	now := s.now().UTC()
	var expires *time.Time
	if in.ExpiresAt != nil {
		t := in.ExpiresAt.UTC()
		if !t.After(now) {
			fields["expiresAt"] = "Expiry must be in the future"
		} else {
			expires = &t
		}
	}

	audience := strings.TrimSpace(in.Audience)
	var recipients []string
	switch audience {
	case models.NotificationAudienceAll:
	case models.NotificationAudienceSelected:
		ids := uniqueNonEmpty(in.SellerIDs)
		switch {
		case len(ids) == 0:
			fields["sellerIds"] = "Pick at least one seller"
		case len(ids) > notificationMaxRecipients:
			fields["sellerIds"] = "Too many sellers selected"
		default:
			found, err := s.repo.ExistingSellerIDs(ctx, ids)
			if err != nil {
				return nil, err
			}
			if len(found) != len(ids) {
				fields["sellerIds"] = "One or more selected accounts are not sellers"
			} else {
				recipients = ids
			}
		}
	default:
		fields["audience"] = "Must be all or selected"
	}

	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}

	n := &models.Notification{
		ID: models.NewID(), Title: title, Body: body, Level: level, LinkURL: link,
		Audience: audience, RecipientIDs: recipients,
		CreatedBy: in.AdminID, CreatedAt: now, ExpiresAt: expires,
	}
	if err := s.repo.Create(ctx, n); err != nil {
		return nil, err
	}
	s.log(ctx, in.AdminID, "notification.send", n.ID, in.IP, map[string]any{
		"title": title, "audience": audience, "recipients": len(recipients),
	})
	return n, nil
}

// AdminItem is a notification as the admin history shows it.
type AdminNotification struct {
	*models.Notification
	ReadCount      int64
	RecipientCount int64
}

// ListAdmin returns the sent history with read and recipient counts.
func (s *NotificationService) ListAdmin(ctx context.Context, limit, skip int) ([]AdminNotification, int64, error) {
	items, total, err := s.repo.ListAdmin(ctx, limit, skip)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(items))
	for i, n := range items {
		ids[i] = n.ID
	}
	counts, err := s.repo.ReadCounts(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	// "All sellers" reaches today's sellers; counted once for the whole page.
	var allCount int64 = -1
	out := make([]AdminNotification, 0, len(items))
	for _, n := range items {
		recipients := int64(len(n.RecipientIDs))
		if n.Audience == models.NotificationAudienceAll {
			if allCount < 0 {
				c, err := s.users.CountByRole(ctx, models.RoleSeller)
				if err != nil {
					return nil, 0, err
				}
				allCount = c
			}
			recipients = allCount
		}
		out = append(out, AdminNotification{Notification: n, ReadCount: counts[n.ID], RecipientCount: recipients})
	}
	return out, total, nil
}

// Delete recalls a notification: it disappears for every seller.
func (s *NotificationService) Delete(ctx context.Context, id, adminID, ip string) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	s.log(ctx, adminID, "notification.delete", id, ip, nil)
	return nil
}

// --------------------------------------------------------------- seller

type SellerNotification struct {
	*models.Notification
	Read bool
}

// ListForSeller returns this seller's notifications (newest first) and how
// many of them are unread. Sellers never see notifications sent before they
// joined.
func (s *NotificationService) ListForSeller(ctx context.Context, sellerID string) ([]SellerNotification, int, error) {
	visible, err := s.visible(ctx, sellerID)
	if err != nil || len(visible) == 0 {
		return nil, 0, err
	}
	ids := make([]string, len(visible))
	for i, n := range visible {
		ids[i] = n.ID
	}
	read, err := s.repo.ReadSet(ctx, sellerID, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]SellerNotification, 0, len(visible))
	unread := 0
	for _, n := range visible {
		r := read[n.ID]
		if !r {
			unread++
		}
		out = append(out, SellerNotification{Notification: n, Read: r})
	}
	return out, unread, nil
}

// MarkRead marks one notification as read. A notification the seller cannot
// see is reported as not found.
func (s *NotificationService) MarkRead(ctx context.Context, sellerID, id string) error {
	visible, err := s.visible(ctx, sellerID)
	if err != nil {
		return err
	}
	for _, n := range visible {
		if n.ID == id {
			return s.repo.MarkRead(ctx, sellerID, []string{id}, s.now().UTC())
		}
	}
	return apperr.NotFound()
}

// MarkAllRead marks everything the seller can currently see as read.
func (s *NotificationService) MarkAllRead(ctx context.Context, sellerID string) error {
	visible, err := s.visible(ctx, sellerID)
	if err != nil || len(visible) == 0 {
		return err
	}
	ids := make([]string, len(visible))
	for i, n := range visible {
		ids[i] = n.ID
	}
	return s.repo.MarkRead(ctx, sellerID, ids, s.now().UTC())
}

func (s *NotificationService) visible(ctx context.Context, sellerID string) ([]*models.Notification, error) {
	u, err := s.users.FindByID(ctx, sellerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return s.repo.ListForSeller(ctx, sellerID, u.CreatedAt, s.now().UTC(), notificationSellerWindow)
}

// -------------------------------------------------------------- helpers

func (s *NotificationService) log(ctx context.Context, adminID, action, targetID, ip string, next any) {
	_ = s.logs.Create(ctx, &models.AdminActivityLog{
		ID: models.NewID(), AdminID: adminID, Action: action,
		TargetType: "notification", TargetID: targetID,
		NewValue: next, IP: ip, CreatedAt: s.now().UTC(),
	})
}

// validNotificationLink allows a path on this site ("/seller/x/orders") or an
// https address. It rejects javascript:, data:, protocol-relative "//host"
// and anything with backslashes or control characters.
func validNotificationLink(l string) bool {
	if len(l) > notificationMaxLink || strings.ContainsAny(l, "\\ \t\r\n") {
		return false
	}
	if strings.HasPrefix(l, "/") {
		return !strings.HasPrefix(l, "//")
	}
	u, err := url.Parse(l)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func uniqueNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
