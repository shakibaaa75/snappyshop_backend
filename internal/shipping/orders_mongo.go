package shipping

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/repository"
)

// MongoOrders reads and updates the "orders" collection directly.
//
// CHECK AGAINST internal/models/order.go: the bson field names below
// (number, status, customerName, customerPhone, shippingAddress, shippingArea,
// shippingCity, notes, paymentMethod, paid, total, events[].{status,at,note})
// are taken from the frontend's order JSON. If your model differs, change them
// here; this is the only file that knows the order document shape.
//
// If OrderService.UpdateStatus also sends the "your order has shipped" email,
// prefer calling it from MarkShipped instead of the raw update below, so
// customers keep getting that email.
type MongoOrders struct{ col *mongo.Collection }

func NewMongoOrders(db *mongo.Database) *MongoOrders {
	return &MongoOrders{col: db.Collection(database.OrdersCollection)}
}

type orderDoc struct {
	ID              string  `bson:"_id"`
	Number          string  `bson:"number"`
	Status          string  `bson:"status"`
	CustomerName    string  `bson:"customerName"`
	CustomerPhone   string  `bson:"customerPhone"`
	ShippingAddress string  `bson:"shippingAddress"`
	ShippingArea    string  `bson:"shippingArea"`
	ShippingCity    string  `bson:"shippingCity"`
	Notes           string  `bson:"notes"`
	PaymentMethod   string  `bson:"paymentMethod"`
	Paid            bool    `bson:"paid"`
	Total           float64 `bson:"total"`
}

func (m *MongoOrders) Get(ctx context.Context, sellerID, orderID string) (*OrderInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d orderDoc
	err := m.col.FindOne(ctx, bson.D{{Key: "_id", Value: orderID}, {Key: "sellerId", Value: sellerID}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{d.ShippingAddress, d.ShippingArea, d.ShippingCity} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	var cod float64
	if d.PaymentMethod == "cod" && !d.Paid {
		cod = d.Total
	}
	return &OrderInfo{ID: d.ID, Number: d.Number, Status: d.Status, Name: d.CustomerName, Phone: d.CustomerPhone,
		Address: strings.Join(parts, ", "), Note: d.Notes, COD: cod}, nil
}

func (m *MongoOrders) MarkShipped(ctx context.Context, sellerID, orderID, note string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	res, err := m.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: orderID}, {Key: "sellerId", Value: sellerID}, {Key: "status", Value: statusConfirmed}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: "shipped"}, {Key: "updatedAt", Value: now}}},
			{Key: "$push", Value: bson.D{{Key: "events", Value: bson.D{
				{Key: "status", Value: "shipped"}, {Key: "at", Value: now}, {Key: "note", Value: note},
			}}}},
		})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}
