package models

import "time"

// Notification levels. They only change how the notification looks.
const (
	NotificationInfo    = "info"
	NotificationSuccess = "success"
	NotificationWarning = "warning"
	NotificationUrgent  = "urgent"
)

// Notification audiences.
const (
	NotificationAudienceAll      = "all"      // every seller, including sellers who join later
	NotificationAudienceSelected = "selected" // only the sellers in RecipientIDs
)

// Notification is one message an admin sent to sellers. It is stored once,
// even when it goes to every seller; who has read it lives in NotificationRead.
type Notification struct {
	ID           string     `bson:"_id"`
	Title        string     `bson:"title"`
	Body         string     `bson:"body"`
	Level        string     `bson:"level"`
	LinkURL      string     `bson:"linkUrl,omitempty"` // "/path" on this site, or https://
	Audience     string     `bson:"audience"`
	RecipientIDs []string   `bson:"recipientIds,omitempty"` // only when Audience == selected
	CreatedBy    string     `bson:"createdBy"`
	CreatedAt    time.Time  `bson:"createdAt"`
	ExpiresAt    *time.Time `bson:"expiresAt,omitempty"` // hidden from sellers after this time
}

// NotificationRead records that one user has read one notification.
// _id is "<userId>:<notificationId>", so marking twice is harmless.
type NotificationRead struct {
	ID             string    `bson:"_id"`
	UserID         string    `bson:"userId"`
	NotificationID string    `bson:"notificationId"`
	ReadAt         time.Time `bson:"readAt"`
}
