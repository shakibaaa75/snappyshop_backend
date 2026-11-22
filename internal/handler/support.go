package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"shopbd/backend/internal/mailer"
	"strings"
	"time"

	"shopbd/backend/internal/middleware"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Support struct {
	DB   *mongo.Database
	Mail mailer.Mailer
}
type supportMessage struct {
	Author    string    `json:"author" bson:"author"`
	Body      string    `json:"body" bson:"body"`
	CreatedAt time.Time `json:"createdAt" bson:"createdAt"`
	Internal  bool      `json:"internal" bson:"internal"`
}
type supportTicket struct {
	ID          primitive.ObjectID `json:"id" bson:"_id,omitempty"`
	Number      string             `json:"number" bson:"number"`
	UserID      string             `json:"-" bson:"userId"`
	Role        string             `json:"role" bson:"role"`
	Name        string             `json:"name" bson:"name"`
	Email       string             `json:"email" bson:"email"`
	Category    string             `json:"category" bson:"category"`
	Subcategory string             `json:"subcategory" bson:"subcategory"`
	Subject     string             `json:"subject" bson:"subject"`
	Message     string             `json:"message" bson:"message"`
	Status      string             `json:"status" bson:"status"`
	Priority    string             `json:"priority" bson:"priority"`
	AssignedTo  string             `json:"assignedTo" bson:"assignedTo"`
	OrderID     string             `json:"orderId,omitempty" bson:"orderId,omitempty"`
	ShopID      string             `json:"shopId,omitempty" bson:"shopId,omitempty"`
	Messages    []supportMessage   `json:"messages" bson:"messages"`
	CreatedAt   time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt   time.Time          `json:"updatedAt" bson:"updatedAt"`
}

func (s *Support) coll() *mongo.Collection { return s.DB.Collection("support_tickets") }
func supportJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func supportErr(w http.ResponseWriter, status int, msg string) {
	supportJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "code": "SUPPORT_ERROR"}})
}
func supportDecode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		supportErr(w, 400, "Invalid request body")
		return false
	}
	return true
}
func supportID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return "SNP-" + strings.ToUpper(hex.EncodeToString(b))
}
func (s *Support) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Email       string `json:"email"`
		Role        string `json:"role"`
		Category    string `json:"category"`
		Subcategory string `json:"subcategory"`
		Subject     string `json:"subject"`
		Message     string `json:"message"`
		OrderID     string `json:"orderId"`
		ShopID      string `json:"shopId"`
	}
	if !supportDecode(w, r, &in) {
		return
	}
	c, authenticated := middleware.ClaimsFrom(r.Context())
	if !authenticated || !strings.EqualFold(string(c.Role), "SELLER") {
		supportErr(w, 403, "Seller account required for support tickets")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Subject = strings.TrimSpace(in.Subject)
	in.Message = strings.TrimSpace(in.Message)
	if len(in.Name) < 2 || len(in.Name) > 120 || len(in.Email) > 254 || !strings.Contains(in.Email, "@") || len(in.Subject) < 3 || len(in.Subject) > 180 || len(in.Message) < 10 || len(in.Message) > 8000 || len(in.Category) > 100 || len(in.Subcategory) > 100 || in.Category == "" || in.Subcategory == "" {
		supportErr(w, 400, "Please complete all required fields")
		return
	}
	now := time.Now().UTC()
	t := supportTicket{Number: supportID(), Name: in.Name, Email: in.Email, Category: in.Category, Subcategory: in.Subcategory, Subject: in.Subject, Message: in.Message, Status: "Open", Priority: "Normal", CreatedAt: now, UpdatedAt: now, Messages: []supportMessage{{Author: "user", Body: in.Message, CreatedAt: now}}, OrderID: in.OrderID, ShopID: in.ShopID}
	t.UserID = c.UserID
	t.Role = "SELLER"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.coll().InsertOne(ctx, t)
	if err != nil {
		supportErr(w, 500, "Unable to create ticket")
		return
	}
	t.ID = result.InsertedID.(primitive.ObjectID)
	supportJSON(w, 201, map[string]any{"ticket": t})
}
func (s *Support) List(w http.ResponseWriter, r *http.Request, admin bool) {
	filter := bson.M{"role": "SELLER"}
	if !admin {
		c, ok := middleware.ClaimsFrom(r.Context())
		if !ok || !strings.EqualFold(string(c.Role), "SELLER") {
			supportErr(w, 403, "Seller account required")
			return
		}
		filter["userId"] = c.UserID
		filter["role"] = "SELLER"
	} else {
		if v := r.URL.Query().Get("status"); v != "" {
			filter["status"] = v
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cur, err := s.coll().Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "updatedAt", Value: -1}}).SetLimit(50))
	if err != nil {
		supportErr(w, 500, "Could not load tickets")
		return
	}
	defer cur.Close(ctx)
	tickets := []supportTicket{}
	if err = cur.All(ctx, &tickets); err != nil {
		supportErr(w, 500, "Could not load tickets")
		return
	}
	for i := range tickets {
		if !admin {
			tickets[i].Messages = publicMessages(tickets[i].Messages)
		}
	}
	supportJSON(w, 200, map[string]any{"tickets": tickets})
}
func publicMessages(ms []supportMessage) []supportMessage {
	out := []supportMessage{}
	for _, m := range ms {
		if !m.Internal {
			out = append(out, m)
		}
	}
	return out
}
func (s *Support) Detail(w http.ResponseWriter, r *http.Request, admin bool) {
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		supportErr(w, 404, "Ticket not found")
		return
	}
	filter := bson.M{"_id": id, "role": "SELLER"}
	if !admin {
		c, ok := middleware.ClaimsFrom(r.Context())
		if !ok || !strings.EqualFold(string(c.Role), "SELLER") {
			supportErr(w, 403, "Seller account required")
			return
		}
		filter["userId"] = c.UserID
		filter["role"] = "SELLER"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var t supportTicket
	if err = s.coll().FindOne(ctx, filter).Decode(&t); err != nil {
		supportErr(w, 404, "Ticket not found")
		return
	}
	if !admin {
		t.Messages = publicMessages(t.Messages)
	}
	supportJSON(w, 200, map[string]any{"ticket": t})
}
func (s *Support) Reply(w http.ResponseWriter, r *http.Request, admin bool) {
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		supportErr(w, 404, "Ticket not found")
		return
	}
	var in struct {
		Message  string `json:"message"`
		Internal bool   `json:"internal"`
		Status   string `json:"status"`
		Priority string `json:"priority"`
	}
	if !supportDecode(w, r, &in) {
		return
	}
	in.Message = strings.TrimSpace(in.Message)
	if len(in.Message) < 1 || len(in.Message) > 8000 {
		supportErr(w, 400, "Message must be 1–8000 characters")
		return
	}
	if !admin && in.Internal {
		supportErr(w, 403, "Not permitted")
		return
	}
	filter := bson.M{"_id": id, "role": "SELLER"}
	if !admin {
		c, ok := middleware.ClaimsFrom(r.Context())
		if !ok || !strings.EqualFold(string(c.Role), "SELLER") {
			supportErr(w, 403, "Seller account required")
			return
		}
		filter["userId"] = c.UserID
		filter["role"] = "SELLER"
	}
	now := time.Now().UTC()
	author := "user"
	if admin {
		author = "admin"
	}
	update := bson.M{"$push": bson.M{"messages": supportMessage{Author: author, Body: in.Message, Internal: admin && in.Internal, CreatedAt: now}}, "$set": bson.M{"updatedAt": now}}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.coll().UpdateOne(ctx, filter, update)
	if err != nil || result.MatchedCount == 0 {
		supportErr(w, 404, "Ticket not found")
		return
	}
	// Admin public replies generate a seller bell notification; internal notes do not.
	if admin && !in.Internal {
		var ticket supportTicket
		if err := s.coll().FindOne(ctx, bson.M{"_id": id}).Decode(&ticket); err == nil && ticket.UserID != "" {
			notificationID := primitive.NewObjectID().Hex()
			_, notifyErr := s.DB.Collection("notifications").InsertOne(ctx, bson.M{
				"_id": notificationID, "title": "New support reply", "body": "SnappyShop support replied to ticket " + ticket.Number,
				"level": "info", "linkUrl": "/help", "audience": "selected", "recipientIds": []string{ticket.UserID},
				"createdBy": "system", "createdAt": now,
			})
			if notifyErr != nil {
				slog.Error("support reply notification failed", "ticket", ticket.Number, "error", notifyErr)
			}
		}
	}
	supportJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Support) Update(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		supportErr(w, 404, "Ticket not found")
		return
	}
	var in struct {
		Status     string `json:"status"`
		Priority   string `json:"priority"`
		AssignedTo string `json:"assignedTo"`
	}
	if !supportDecode(w, r, &in) {
		return
	}
	fields := bson.M{"updatedAt": time.Now().UTC()}
	if in.Status != "" {
		valid := map[string]bool{"Open": true, "In Progress": true, "Waiting for User": true, "Resolved": true, "Closed": true}
		if !valid[in.Status] {
			supportErr(w, 400, "Invalid status")
			return
		}
		fields["status"] = in.Status
	}
	if in.Priority != "" {
		if in.Priority != "Low" && in.Priority != "Normal" && in.Priority != "High" && in.Priority != "Urgent" {
			supportErr(w, 400, "Invalid priority")
			return
		}
		fields["priority"] = in.Priority
	}
	if len(in.AssignedTo) > 120 {
		supportErr(w, 400, "Invalid assignee")
		return
	}
	if in.AssignedTo != "" {
		fields["assignedTo"] = in.AssignedTo
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.coll().UpdateOne(ctx, bson.M{"_id": id, "role": "SELLER"}, bson.M{"$set": fields})
	if err != nil || result.MatchedCount == 0 {
		supportErr(w, 404, "Ticket not found")
		return
	}
	supportJSON(w, 200, map[string]bool{"ok": true})
}

var _ = fmt.Sprintf

// Contact creates a one-way inquiry for buyers and guests, not a support ticket.
func (s *Support) Contact(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Email       string `json:"email"`
		Category    string `json:"category"`
		Subcategory string `json:"subcategory"`
		Subject     string `json:"subject"`
		Message     string `json:"message"`
	}
	if !supportDecode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Subject = strings.TrimSpace(in.Subject)
	in.Message = strings.TrimSpace(in.Message)
	if len(in.Name) < 2 || len(in.Name) > 120 || len(in.Email) > 254 || !strings.Contains(in.Email, "@") || len(in.Subject) < 3 || len(in.Subject) > 180 || len(in.Message) < 10 || len(in.Message) > 8000 || len(in.Category) == 0 || len(in.Category) > 100 || len(in.Subcategory) == 0 || len(in.Subcategory) > 100 {
		supportErr(w, 400, "Invalid contact information")
		return
	}
	role := "GUEST"
	userID := ""
	if c, ok := middleware.ClaimsFrom(r.Context()); ok {
		role = string(c.Role)
		userID = c.UserID
	}
	if strings.EqualFold(role, "SELLER") {
		supportErr(w, 403, "Sellers should use support tickets")
		return
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.DB.Collection("contact_messages").InsertOne(ctx, bson.M{"name": in.Name, "email": in.Email, "role": role, "userId": userID, "category": in.Category, "subcategory": in.Subcategory, "subject": in.Subject, "message": in.Message, "status": "New", "createdAt": now, "updatedAt": now})
	if err != nil {
		supportErr(w, 500, "Unable to send message")
		return
	}
	supportJSON(w, 201, map[string]any{"ok": true})
}
func (s *Support) Contacts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cur, err := s.DB.Collection("contact_messages").Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(100))
	if err != nil {
		supportErr(w, 500, "Could not load messages")
		return
	}
	defer cur.Close(ctx)
	messages := []bson.M{}
	if err = cur.All(ctx, &messages); err != nil {
		supportErr(w, 500, "Could not load messages")
		return
	}
	supportJSON(w, 200, map[string]any{"messages": messages})
}

// ReplyContact allows Super Admin to email buyers and guests without creating a ticket.
// The message is saved with delivery state for auditing and retries.
func (s *Support) ReplyContact(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		supportErr(w, 404, "Contact message not found")
		return
	}
	var in struct {
		Message string `json:"message"`
	}
	if !supportDecode(w, r, &in) {
		return
	}
	in.Message = strings.TrimSpace(in.Message)
	if len(in.Message) < 1 || len(in.Message) > 8000 {
		supportErr(w, 400, "Reply must be 1–8000 characters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	coll := s.DB.Collection("contact_messages")
	var contact struct {
		Email   string `bson:"email"`
		Name    string `bson:"name"`
		Subject string `bson:"subject"`
	}
	if err := coll.FindOne(ctx, bson.M{"_id": id}).Decode(&contact); err != nil {
		supportErr(w, 404, "Contact message not found")
		return
	}
	if s.Mail == nil {
		supportErr(w, 503, "Email service unavailable")
		return
	}
	subject := "Re: " + contact.Subject + " — SnappyShop Support"
	body := "Hello " + contact.Name + ",\n\n" + in.Message + "\n\nSnappyShop Support"
	htmlBody := "<p>Hello " + html.EscapeString(contact.Name) + ",</p><p style=\"white-space:pre-wrap\">" + html.EscapeString(in.Message) + "</p><p>SnappyShop Support</p>"
	if err := s.Mail.Send(ctx, contact.Email, subject, htmlBody, body); err != nil {
		slog.Error("contact reply email failed", "contactId", id.Hex(), "error", err)
		supportErr(w, 502, "Email delivery failed. Please retry.")
		return
	}
	_, err = coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$push": bson.M{"replies": bson.M{"message": in.Message, "sentAt": time.Now().UTC()}}, "$set": bson.M{"status": "Replied", "updatedAt": time.Now().UTC()}})
	if err != nil {
		slog.Error("contact reply audit update failed", "contactId", id.Hex(), "error", err)
	}
	supportJSON(w, 200, map[string]bool{"ok": true})
}
