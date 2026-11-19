package service

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"shopbd/backend/internal/models"
)

/* ------------------------------------------------------------------ */
/* Which statuses send an email, and what they say                     */
/* ------------------------------------------------------------------ */

type orderEmailCopy struct {
	Subject string
	Heading string
	Intro   string
}

func orderEmailCopyFor(st models.OrderStatus, number string) (orderEmailCopy, bool) {
	switch st {
	case models.OrderConfirmed:
		return orderEmailCopy{
			Subject: fmt.Sprintf("Your order %s is confirmed", number),
			Heading: "Your order is confirmed",
			Intro:   "the seller has confirmed your order and is getting it ready.",
		}, true
	case models.OrderShipped:
		return orderEmailCopy{
			Subject: fmt.Sprintf("Your order %s is on its way", number),
			Heading: "Your order has been shipped",
			Intro:   "your order has been handed to the courier and is on its way to you.",
		}, true
	case models.OrderDelivered:
		return orderEmailCopy{
			Subject: fmt.Sprintf("Your order %s has been delivered", number),
			Heading: "Your order has been delivered",
			Intro:   "your order is marked as delivered. Thank you for shopping with us!",
		}, true
	}
	// pending / cancelled: no email.
	return orderEmailCopy{}, false
}

/* ------------------------------------------------------------------ */
/* Template data                                                       */
/* ------------------------------------------------------------------ */

type orderEmailLine struct {
	Name     string
	Variant  string
	Qty      int
	Subtotal string
}

type orderEmailData struct {
	Heading  string
	Name     string
	Intro    string
	Number   string
	Lines    []orderEmailLine
	Subtotal string
	// DeliveryLabel is "Delivery" or, when the zone is known, "Delivery (Inside Dhaka)".
	DeliveryLabel string
	Delivery      string
	// Discount is empty when there is none; the row is then left out.
	Discount string
	Total    string
	Address  string
	Payment  string
}

var orderEmailTpl = template.Must(template.New("order-email").Parse(`<!doctype html>
<html>
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="margin:0;background:#f5f5f4;font-family:Arial,Helvetica,sans-serif;color:#1c1917;">
<table width="100%" cellpadding="0" cellspacing="0" style="background:#f5f5f4;padding:24px 0;">
<tr><td align="center">
<table width="560" cellpadding="0" cellspacing="0" style="max-width:560px;width:100%;background:#ffffff;border-radius:12px;">
<tr><td style="padding:28px;">
  <h1 style="margin:0 0 8px;font-size:22px;">{{.Heading}}</h1>
  <p style="margin:0 0 18px;color:#57534e;font-size:14px;line-height:1.6;">Hi {{.Name}}, {{.Intro}}</p>
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

  <p style="margin:20px 0 4px;font-size:13px;color:#57534e;">Delivery address</p>
  <p style="margin:0 0 12px;font-size:14px;line-height:1.5;">{{.Address}}</p>
  <p style="margin:0;font-size:13px;color:#57534e;">Payment: {{.Payment}}</p>
</td></tr>
</table>
<p style="margin:16px 0 0;font-size:12px;color:#a8a29e;">You received this email because you placed an order. Please do not reply to this message.</p>
</td></tr>
</table>
</body>
</html>`))

// zoneLabel is the customer-facing name of a delivery zone, or "" for orders
// that have none (placed before Delivery Settings existed).
func zoneLabel(z models.DeliveryZone) string {
	switch z {
	case models.ZoneInsideDhaka:
		return "Inside Dhaka"
	case models.ZoneOutsideDhaka:
		return "Outside Dhaka"
	}
	return ""
}

// orderMoneyLines returns the delivery amount text ("Free" or a taka amount),
// the row label ("Delivery" or "Delivery (Inside Dhaka)") and the discount
// text ("" when there is no discount). Both order emails use it so they can
// never word delivery differently.
func orderMoneyLines(o models.Order) (delivery, label, discount string) {
	delivery = "Free"
	if o.Delivery > 0 {
		delivery = formatTaka(o.Delivery)
	}
	label = "Delivery"
	if z := zoneLabel(o.DeliveryZone); z != "" {
		label = "Delivery (" + z + ")"
	}
	if o.Discount > 0 {
		discount = formatTaka(o.Discount)
	}
	return delivery, label, discount
}

/* ------------------------------------------------------------------ */
/* Build + send                                                        */
/* ------------------------------------------------------------------ */

type orderEmail struct {
	Subject string
	HTML    string
	Text    string
}

func buildOrderStatusEmail(o models.Order) (orderEmail, bool) {
	txt, ok := orderEmailCopyFor(o.Status, o.Number)
	if !ok {
		return orderEmail{}, false
	}

	lines := make([]orderEmailLine, 0, len(o.Lines))
	for _, l := range o.Lines {
		lines = append(lines, orderEmailLine{
			Name:     l.Name,
			Variant:  l.Variant,
			Qty:      l.Qty,
			Subtotal: formatTaka(l.Subtotal),
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

	data := orderEmailData{
		Heading:       txt.Heading,
		Name:          o.CustomerName,
		Intro:         txt.Intro,
		Number:        o.Number,
		Lines:         lines,
		Subtotal:      formatTaka(o.Subtotal),
		DeliveryLabel: deliveryLabel,
		Delivery:      delivery,
		Discount:      discount,
		Total:         formatTaka(o.Total),
		Address:       address,
		Payment:       paymentLabel(o.PaymentMethod),
	}

	var htmlBuf bytes.Buffer
	if err := orderEmailTpl.Execute(&htmlBuf, data); err != nil {
		slog.Error("order email template failed", "order", o.Number, "err", err)
		return orderEmail{}, false
	}

	var t strings.Builder
	t.WriteString(data.Heading + "\n\n")
	t.WriteString("Hi " + data.Name + ", " + data.Intro + "\n\n")
	t.WriteString("Order number: " + data.Number + "\n\n")
	for _, l := range data.Lines {
		name := l.Name
		if l.Variant != "" {
			name += " (" + l.Variant + ")"
		}
		t.WriteString(fmt.Sprintf("- %s x %d  %s\n", name, l.Qty, l.Subtotal))
	}
	t.WriteString("\nSubtotal: " + data.Subtotal + "\n")
	t.WriteString(data.DeliveryLabel + ": " + data.Delivery + "\n")
	if data.Discount != "" {
		t.WriteString("Discount: -" + data.Discount + "\n")
	}
	t.WriteString("Total: " + data.Total + "\n\n")
	t.WriteString("Delivery address: " + data.Address + "\n")
	t.WriteString("Payment: " + data.Payment + "\n")

	return orderEmail{Subject: txt.Subject, HTML: htmlBuf.String(), Text: t.String()}, true
}

// notifyStatus emails the customer about the order's current status.
// It is meant to run in its own goroutine: it never returns an error to the
// caller, it only logs. A failed email must never undo a status change.
func (s *OrderService) notifyStatus(o models.Order) {
	to := strings.TrimSpace(o.CustomerEmail)
	if to == "" {
		return
	}
	msg, ok := buildOrderStatusEmail(o)
	if !ok {
		return
	}

	// The HTTP request context is cancelled when the response is written,
	// so the email uses its own context.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := s.mailer.Send(ctx, to, msg.Subject, msg.HTML, msg.Text); err != nil {
		slog.Error("order email failed", "order", o.Number, "status", string(o.Status), "err", err)
		return
	}
	slog.Info("order email sent", "order", o.Number, "status", string(o.Status))
}

/* ------------------------------------------------------------------ */
/* Small helpers                                                       */
/* ------------------------------------------------------------------ */

func paymentLabel(p models.PaymentMethod) string {
	switch p {
	case models.PayBkash:
		return "bKash"
	case models.PayNagad:
		return "Nagad"
	case models.PayRocket:
		return "Rocket"
	case models.PayCOD:
		return "Cash on delivery"
	case models.PayCard:
		return "Card"
	}
	return string(p)
}

// formatTaka renders 12500 as "৳12,500".
func formatTaka(n float64) string {
	v := int64(math.Round(n))
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	res := "৳" + string(out)
	if neg {
		res = "-" + res
	}
	return res
}
