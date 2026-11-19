package service

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// systemSender marks notifications the platform creates by itself (for
// example "new order"). The admin's "Sent" history hides them, so it only
// lists messages an admin wrote.
const systemSender = "system"

// SetSellerAlerts turns on "new order" alerts for sellers: an in-app
// notification (the bell) and an email sent with the same SMTP sender that
// delivers password-reset mails. Call it once at startup. Without it, new
// orders simply do not alert the seller.
func (s *OrderService) SetSellerAlerts(
	users repository.UserRepository,
	notifs repository.NotificationRepository,
	frontendURL string,
) {
	s.users = users
	s.notifs = notifs
	s.frontendURL = strings.TrimRight(strings.TrimSpace(frontendURL), "/")
}

/* ------------------------------------------------------------------ */
/* Email template                                                      */
/* ------------------------------------------------------------------ */

type sellerOrderEmailData struct {
	SellerName string
	Number     string
	Customer   string
	Phone      string
	Lines      []orderEmailLine
	Subtotal   string
	// DeliveryLabel is "Delivery" or "Delivery (Inside Dhaka)"; Discount is
	// empty when there is none and its row is then left out.
	DeliveryLabel string
	Delivery      string
	Discount      string
	Total         string
	Address       string
	Payment       string
	Notes         string
	OrderURL      string
}

var sellerOrderEmailTpl = template.Must(template.New("seller-order-email").Parse(`<!doctype html>
<html>
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="margin:0;background:#f5f5f4;font-family:Arial,Helvetica,sans-serif;color:#1c1917;">
<table width="100%" cellpadding="0" cellspacing="0" style="background:#f5f5f4;padding:24px 0;">
<tr><td align="center">
<table width="560" cellpadding="0" cellspacing="0" style="max-width:560px;width:100%;background:#ffffff;border-radius:12px;">
<tr><td style="padding:28px;">
  <h1 style="margin:0 0 8px;font-size:22px;">You have a new order</h1>
  <p style="margin:0 0 18px;color:#57534e;font-size:14px;line-height:1.6;">Hi {{.SellerName}}, a customer just placed an order in your store.</p>
  <p style="margin:0 0 16px;font-size:14px;">Order number: <strong>{{.Number}}</strong></p>

  <table width="100%" cellpadding="0" cellspacing="0" style="border-top:1px solid #e7e5e4;font-size:14px;">
  {{range .Lines}}
    <tr>
      <td style="padding:10px 0;border-bottom:1px solid #f5f5f4;">{{.Name}}{{if .Variant}} ({{.Variant}}){{end}} &times; {{.Qty}}</td>
      <td align="right" style="padding:10px 0;border-bottom:1px solid #f5f5f4;">{{.Subtotal}}</td>
    </tr>
  {{end}}
    <tr><td style="padding:10px 0 2px;color:#57534e;">Subtotal</td><td align="right" style="padding:10px 0 2px;">{{.Subtotal}}</td></tr>
    <tr><td style="padding:2px 0;color:#57534e;">{{.DeliveryLabel}}</td><td align="right" style="padding:2px 0;">{{.Delivery}}</td></tr>
    {{if .Discount}}<tr><td style="padding:2px 0;color:#57534e;">Discount</td><td align="right" style="padding:2px 0;">-{{.Discount}}</td></tr>{{end}}
    <tr><td style="padding:8px 0;font-weight:bold;">Total</td><td align="right" style="padding:8px 0;font-weight:bold;">{{.Total}}</td></tr>
  </table>

  <p style="margin:20px 0 4px;font-size:13px;color:#57534e;">Customer</p>
  <p style="margin:0 0 12px;font-size:14px;line-height:1.5;">{{.Customer}}<br>{{.Phone}}</p>
  <p style="margin:0 0 4px;font-size:13px;color:#57534e;">Delivery address</p>
  <p style="margin:0 0 12px;font-size:14px;line-height:1.5;">{{.Address}}</p>
  <p style="margin:0 0 4px;font-size:13px;color:#57534e;">Payment: {{.Payment}}</p>
  {{if .Notes}}<p style="margin:12px 0 0;font-size:13px;color:#57534e;">Customer note: {{.Notes}}</p>{{end}}

  {{if .OrderURL}}
  <p style="margin:24px 0 0;"><a href="{{.OrderURL}}" style="display:inline-block;background:#17645F;color:#ffffff;text-decoration:none;font-size:14px;font-weight:bold;padding:12px 22px;border-radius:999px;">View order</a></p>
  {{end}}
</td></tr>
</table>
<p style="margin:16px 0 0;font-size:12px;color:#a8a29e;">You received this email because you are a seller on ShopBD. Please do not reply to this message.</p>
</td></tr>
</table>
</body>
</html>`))

func buildSellerOrderEmail(sellerName string, o models.Order, orderURL string) (orderEmail, bool) {
	lines := make([]orderEmailLine, 0, len(o.Lines))
	for _, l := range o.Lines {
		lines = append(lines, orderEmailLine{
			Name: l.Name, Variant: l.Variant, Qty: l.Qty, Subtotal: formatTaka(l.Subtotal),
		})
	}

	address := strings.TrimSpace(o.ShippingAddress)
	if a := strings.TrimSpace(o.ShippingArea); a != "" {
		address += ", " + a
	}
	if c := strings.TrimSpace(o.ShippingCity); c != "" {
		address += ", " + c
	}
	delivery, deliveryLabel, discount := orderMoneyLines(o)
	if strings.TrimSpace(sellerName) == "" {
		sellerName = "there"
	}

	d := sellerOrderEmailData{
		SellerName: sellerName, Number: o.Number,
		Customer: o.CustomerName, Phone: o.CustomerPhone,
		Lines: lines, Subtotal: formatTaka(o.Subtotal),
		DeliveryLabel: deliveryLabel, Delivery: delivery, Discount: discount,
		Total: formatTaka(o.Total), Address: address,
		Payment: paymentLabel(o.PaymentMethod), Notes: o.Notes, OrderURL: orderURL,
	}

	var buf bytes.Buffer
	if err := sellerOrderEmailTpl.Execute(&buf, d); err != nil {
		slog.Error("seller order email template failed", "order", o.Number, "err", err)
		return orderEmail{}, false
	}

	var t strings.Builder
	t.WriteString("You have a new order\n\n")
	t.WriteString("Hi " + d.SellerName + ", a customer just placed an order in your store.\n\n")
	t.WriteString("Order number: " + d.Number + "\n\n")
	for _, l := range d.Lines {
		name := l.Name
		if l.Variant != "" {
			name += " (" + l.Variant + ")"
		}
		t.WriteString(fmt.Sprintf("- %s x %d  %s\n", name, l.Qty, l.Subtotal))
	}
	t.WriteString("\nSubtotal: " + d.Subtotal + "\n")
	t.WriteString(d.DeliveryLabel + ": " + d.Delivery + "\n")
	if d.Discount != "" {
		t.WriteString("Discount: -" + d.Discount + "\n")
	}
	t.WriteString("Total: " + d.Total + "\n\n")
	t.WriteString("Customer: " + d.Customer + " (" + d.Phone + ")\n")
	t.WriteString("Delivery address: " + d.Address + "\n")
	t.WriteString("Payment: " + d.Payment + "\n")
	if d.Notes != "" {
		t.WriteString("Customer note: " + d.Notes + "\n")
	}
	if orderURL != "" {
		t.WriteString("\nView order: " + orderURL + "\n")
	}

	return orderEmail{
		Subject: fmt.Sprintf("New order %s - %s", o.Number, d.Total),
		HTML:    buf.String(),
		Text:    t.String(),
	}, true
}

/* ------------------------------------------------------------------ */
/* Alert                                                               */
/* ------------------------------------------------------------------ */

// alertSellerNewOrder tells the seller about a new order: one in-app
// notification and one email. It runs in its own goroutine and only logs, so
// a failure here can never undo or slow down the customer's order.
func (s *OrderService) alertSellerNewOrder(o models.Order) {
	if s.users == nil && s.notifs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Where "View order" goes: /seller/<current slug>/orders/<id>
	path := ""
	if d, err := s.designs.GetBySeller(ctx, o.SellerID); err == nil && d != nil && d.Slug != "" {
		path = "/seller/" + url.PathEscape(d.Slug) + "/orders/" + url.PathEscape(o.ID)
	}

	// 1) In-app notification (the bell on the header).
	if s.notifs != nil {
		n := &models.Notification{
			ID:           models.NewID(),
			Title:        "New order " + o.Number,
			Body:         fmt.Sprintf("%s ordered %d item(s), total %s.", o.CustomerName, orderItemCount(o), formatTaka(o.Total)),
			Level:        models.NotificationSuccess,
			LinkURL:      path,
			Audience:     models.NotificationAudienceSelected,
			RecipientIDs: []string{o.SellerID},
			CreatedBy:    systemSender,
			CreatedAt:    time.Now().UTC(),
		}
		if err := s.notifs.Create(ctx, n); err != nil {
			slog.Error("new-order notification failed", "order", o.Number, "err", err)
		}
	}

	// 2) Email to the seller's account address.
	if s.users == nil {
		return
	}
	u, err := s.users.FindByID(ctx, o.SellerID)
	if err != nil || u == nil || strings.TrimSpace(u.Email) == "" {
		slog.Error("new-order email: seller not found", "order", o.Number, "err", err)
		return
	}
	orderURL := ""
	if path != "" && s.frontendURL != "" {
		orderURL = s.frontendURL + path
	}
	msg, ok := buildSellerOrderEmail(u.Name, o, orderURL)
	if !ok {
		return
	}
	if err := s.mailer.Send(ctx, u.Email, msg.Subject, msg.HTML, msg.Text); err != nil {
		slog.Error("new-order email failed", "order", o.Number, "err", err)
		return
	}
	slog.Info("new-order email sent", "order", o.Number)
}

func orderItemCount(o models.Order) int {
	n := 0
	for _, l := range o.Lines {
		n += l.Qty
	}
	return n
}
