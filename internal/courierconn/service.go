// Package courierconn manages a seller's courier connections: listing,
// connecting, testing, rotating credentials and disconnecting.
//
// The seller ID always comes from the authenticated session (the handler
// passes claims.UserID); it is never read from the request body or URL.
package courierconn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/courier"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/secretbox"
)

const (
	CodeCredentialsRejected = "COURIER_CREDENTIALS_REJECTED"
	CodeUnavailable         = "COURIER_UNAVAILABLE"
	CodeAlreadyConnected    = "COURIER_ALREADY_CONNECTED"
	CodeLimitReached        = "COURIER_LIMIT_REACHED"
	CodeNotUsable           = "COURIER_NOT_USABLE"

	// MaxConnectionsPerSeller is a reasonable system limit.
	MaxConnectionsPerSeller = 10
	verifyTimeout           = 12 * time.Second
	maxLabelLen             = 60
	maxCredentialLen        = 256
)

type Service struct {
	repo repository.CourierConnectionRepository
	reg  *courier.Registry
	box  *secretbox.Box
	log  *slog.Logger
	now  func() time.Time
	gate Gate // optional: nil means no plan checks
}

// Gate is satisfied by the subscription service. It decides whether a seller's
// plan includes automatic courier integration.
type Gate interface {
	RequireAutoCourier(ctx context.Context, sellerID string) error
}

// SetGate turns on plan checks for connecting, updating, testing and booking
// with a courier. Listing and disconnecting stay open so a seller whose plan
// ended can still see and remove what they connected. Call before serving.
func (s *Service) SetGate(g Gate) { s.gate = g }

func (s *Service) checkPlan(ctx context.Context, sellerID string) error {
	if s.gate == nil {
		return nil
	}
	return s.gate.RequireAutoCourier(ctx, sellerID)
}

func New(repo repository.CourierConnectionRepository, reg *courier.Registry, box *secretbox.Box, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, reg: reg, box: box, log: log, now: func() time.Time { return time.Now().UTC() }}
}

/* ---------------- views (what the API returns) ---------------- */

// FieldView, ProviderView and View are the only shapes sent to the browser.
// None of them has a place for a credential value.
type FieldView struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Placeholder string `json:"placeholder,omitempty"`
	HelpText    string `json:"helpText,omitempty"`
	Required    bool   `json:"required"`
}

type ProviderView struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	CredentialFields []FieldView `json:"credentialFields"`
	CredentialHelp   []string    `json:"credentialHelp"`
	DocsURL          string      `json:"docsUrl,omitempty"`
}

type View struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ProviderName  string `json:"providerName"`
	Label         string `json:"label,omitempty"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	AccountHint   string `json:"accountHint,omitempty"`
	ConnectedAt   string `json:"connectedAt"`
	LastCheckedAt string `json:"lastCheckedAt,omitempty"`
}

func (s *Service) view(c *models.CourierConnection) View {
	name := c.Provider
	if p, ok := s.reg.Get(c.Provider); ok {
		name = p.Name()
	}
	v := View{
		ID: c.ID, Provider: c.Provider, ProviderName: name, Label: c.Label,
		Status: string(c.Status), StatusMessage: c.StatusMessage, AccountHint: c.AccountHint,
		ConnectedAt: c.ConnectedAt.UTC().Format(time.RFC3339),
	}
	if c.LastCheckedAt != nil {
		v.LastCheckedAt = c.LastCheckedAt.UTC().Format(time.RFC3339)
	}
	return v
}

/* ---------------- queries ---------------- */

// Providers lists only implemented adapters.
func (s *Service) Providers() []ProviderView {
	list := s.reg.List()
	out := make([]ProviderView, 0, len(list))
	for _, p := range list {
		fields := make([]FieldView, 0, len(p.Fields()))
		for _, f := range p.Fields() {
			fields = append(fields, FieldView{f.Key, f.Label, f.Type, f.Placeholder, f.HelpText, f.Required})
		}
		out = append(out, ProviderView{ID: p.ID(), Name: p.Name(), CredentialFields: fields, CredentialHelp: p.Help(), DocsURL: p.DocsURL()})
	}
	return out
}

func (s *Service) List(ctx context.Context, sellerID string) ([]View, error) {
	items, err := s.repo.List(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(items))
	for _, c := range items {
		out = append(out, s.view(c))
	}
	return out, nil
}

/* ---------------- commands ---------------- */

type ConnectInput struct {
	Provider    string
	Label       string
	Credentials map[string]string
}

// Connect verifies the credentials with the courier and saves them only if
// the courier positively accepts them.
func (s *Service) Connect(ctx context.Context, sellerID string, in ConnectInput) (*View, error) {
	if err := s.checkPlan(ctx, sellerID); err != nil {
		return nil, err
	}
	prov, ok := s.reg.Get(strings.TrimSpace(in.Provider))
	if !ok {
		return nil, apperr.Validation(map[string]string{"provider": "This courier is not supported"})
	}
	label, err := cleanLabel(in.Label)
	if err != nil {
		return nil, err
	}
	creds, err := cleanCredentials(prov, in.Credentials)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.List(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= MaxConnectionsPerSeller {
		return nil, apperr.New(http.StatusUnprocessableEntity, CodeLimitReached,
			"You have reached the maximum number of courier connections. Disconnect one first.")
	}
	fp := s.box.Fingerprint(sellerID, prov.ID(), prov.Identity(creds))
	for _, e := range existing {
		if e.Provider == prov.ID() && e.AccountFingerprint == fp {
			return nil, errAlreadyConnected()
		}
	}

	if err := s.verify(ctx, prov, creds); err != nil {
		s.log.Info("courier connect rejected", "seller_id", sellerID, "provider", prov.ID(), "reason", reasonOf(err))
		return nil, mapVerifyError(prov, err)
	}

	id := models.NewID()
	sealed, err := s.seal(sellerID, id, prov.ID(), creds)
	if err != nil {
		return nil, err
	}
	now := s.now()
	conn := &models.CourierConnection{
		ID: id, SellerID: sellerID, Provider: prov.ID(), Label: label,
		Status: models.CourierConnected, AccountHint: prov.Hint(creds),
		AccountFingerprint: fp, EncryptedCredentials: sealed,
		ConnectedAt: now, LastCheckedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, conn); err != nil {
		if errors.Is(err, repository.ErrCourierDuplicate) {
			return nil, errAlreadyConnected()
		}
		return nil, err
	}
	s.log.Info("courier connected", "seller_id", sellerID, "connection_id", id, "provider", prov.ID())
	v := s.view(conn)
	return &v, nil
}

// Test re-checks a saved connection and records the result. A bad result is
// returned as a status on the connection, not as an HTTP error.
func (s *Service) Test(ctx context.Context, sellerID, id string) (*View, error) {
	if err := s.checkPlan(ctx, sellerID); err != nil {
		return nil, err
	}
	conn, err := s.find(ctx, sellerID, id)
	if err != nil {
		return nil, err
	}
	prov, ok := s.reg.Get(conn.Provider)
	if !ok {
		return s.record(ctx, conn, models.CourierFailed, "This courier is no longer supported.")
	}
	creds, err := s.open(conn)
	if err != nil {
		s.log.Error("courier credentials unreadable", "seller_id", sellerID, "connection_id", id, "provider", conn.Provider)
		return s.record(ctx, conn, models.CourierNeedsReconnection, "Saved credentials could not be read. Update them to reconnect.")
	}
	verr := s.verify(ctx, prov, creds)
	switch {
	case verr == nil:
		s.rotateIfOld(ctx, conn, prov, creds)
		return s.record(ctx, conn, models.CourierConnected, "")
	case errors.Is(verr, courier.ErrInvalidCredentials):
		return s.record(ctx, conn, models.CourierNeedsReconnection, prov.Name()+" rejected the saved credentials. Update them to reconnect.")
	case isInvalidField(verr):
		var bad *courier.ErrInvalidField
		errors.As(verr, &bad)
		return s.record(ctx, conn, models.CourierNeedsReconnection, bad.Message)
	default:
		return s.record(ctx, conn, models.CourierFailed, "Could not reach "+prov.Name()+". Try again in a moment.")
	}
}

// UpdateCredentials verifies the new credentials first. If verification
// fails, the old credentials and status are left exactly as they were.
func (s *Service) UpdateCredentials(ctx context.Context, sellerID, id string, raw map[string]string) (*View, error) {
	if err := s.checkPlan(ctx, sellerID); err != nil {
		return nil, err
	}
	conn, err := s.find(ctx, sellerID, id)
	if err != nil {
		return nil, err
	}
	prov, ok := s.reg.Get(conn.Provider)
	if !ok {
		return nil, apperr.Validation(map[string]string{"provider": "This courier is not supported"})
	}
	creds, err := cleanCredentials(prov, raw)
	if err != nil {
		return nil, err
	}
	fp := s.box.Fingerprint(sellerID, prov.ID(), prov.Identity(creds))
	if fp != conn.AccountFingerprint {
		others, err := s.repo.List(ctx, sellerID)
		if err != nil {
			return nil, err
		}
		for _, o := range others {
			if o.ID != conn.ID && o.Provider == prov.ID() && o.AccountFingerprint == fp {
				return nil, errAlreadyConnected()
			}
		}
	}
	if err := s.verify(ctx, prov, creds); err != nil {
		s.log.Info("courier credential update rejected", "seller_id", sellerID, "connection_id", id, "provider", prov.ID(), "reason", reasonOf(err))
		return nil, mapVerifyError(prov, err)
	}
	sealed, err := s.seal(sellerID, conn.ID, prov.ID(), creds)
	if err != nil {
		return nil, err
	}
	now := s.now()
	err = s.repo.ReplaceCredentials(ctx, sellerID, conn.ID, repository.CourierCredentialUpdate{
		EncryptedCredentials: sealed, AccountFingerprint: fp, AccountHint: prov.Hint(creds), At: now,
	})
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrNotFound):
			return nil, apperr.NotFound()
		case errors.Is(err, repository.ErrCourierDuplicate):
			return nil, errAlreadyConnected()
		}
		return nil, err
	}
	conn.EncryptedCredentials, conn.AccountFingerprint, conn.AccountHint = sealed, fp, prov.Hint(creds)
	conn.Status, conn.StatusMessage, conn.LastCheckedAt, conn.UpdatedAt = models.CourierConnected, "", &now, now
	s.log.Info("courier credentials rotated", "seller_id", sellerID, "connection_id", id, "provider", prov.ID())
	v := s.view(conn)
	return &v, nil
}

// Disconnect permanently deletes the connection and its stored credentials.
func (s *Service) Disconnect(ctx context.Context, sellerID, id string) error {
	if err := s.repo.Delete(ctx, sellerID, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	s.log.Info("courier disconnected", "seller_id", sellerID, "connection_id", id)
	return nil
}

/* ---------------- used by the shipping service ---------------- */

// Usable is a connection that is ready to book parcels.
type Usable struct {
	ID       string
	Provider courier.Provider
	Shipper  courier.Shipper
	Creds    map[string]string
}

// Usable returns a seller's connection with its decrypted credentials, or an
// error the seller can act on. Credentials stay inside the backend.
func (s *Service) Usable(ctx context.Context, sellerID, id string) (*Usable, error) {
	if err := s.checkPlan(ctx, sellerID); err != nil {
		return nil, err
	}
	conn, err := s.find(ctx, sellerID, id)
	if err != nil {
		return nil, err
	}
	if conn.Status != models.CourierConnected {
		return nil, apperr.New(http.StatusUnprocessableEntity, CodeNotUsable,
			"This courier is not connected right now. Test or update its credentials first.")
	}
	prov, ok := s.reg.Get(conn.Provider)
	if !ok {
		return nil, apperr.New(http.StatusUnprocessableEntity, CodeNotUsable, "This courier is no longer supported.")
	}
	shipper, ok := prov.(courier.Shipper)
	if !ok {
		return nil, apperr.New(http.StatusUnprocessableEntity, CodeNotUsable, prov.Name()+" cannot book parcels yet.")
	}
	creds, err := s.open(conn)
	if err != nil {
		_, _ = s.record(ctx, conn, models.CourierNeedsReconnection, "Saved credentials could not be read. Update them to reconnect.")
		return nil, apperr.New(http.StatusUnprocessableEntity, CodeNotUsable, "Saved credentials could not be read. Update them to reconnect.")
	}
	return &Usable{ID: conn.ID, Provider: prov, Shipper: shipper, Creds: creds}, nil
}

// ReportRejected flips a connection to "needs reconnection" after the courier
// rejected its credentials during a real booking.
func (s *Service) ReportRejected(ctx context.Context, sellerID, id string) {
	conn, err := s.find(ctx, sellerID, id)
	if err != nil {
		return
	}
	name := conn.Provider
	if p, ok := s.reg.Get(conn.Provider); ok {
		name = p.Name()
	}
	_, _ = s.record(ctx, conn, models.CourierNeedsReconnection, name+" rejected the saved credentials. Update them to reconnect.")
}

/* ---------------- helpers ---------------- */

func (s *Service) find(ctx context.Context, sellerID, id string) (*models.CourierConnection, error) {
	c, err := s.repo.FindByID(ctx, sellerID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound() // same answer for "missing" and "someone else's"
		}
		return nil, err
	}
	return c, nil
}

func (s *Service) verify(ctx context.Context, p courier.Provider, creds map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	return p.Verify(ctx, creds)
}

func (s *Service) record(ctx context.Context, c *models.CourierConnection, st models.CourierStatus, msg string) (*View, error) {
	now := s.now()
	if err := s.repo.SetStatus(ctx, c.SellerID, c.ID, st, msg, now); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	c.Status, c.StatusMessage, c.LastCheckedAt = st, msg, &now
	s.log.Info("courier tested", "seller_id", c.SellerID, "connection_id", c.ID, "provider", c.Provider, "status", string(st))
	v := s.view(c)
	return &v, nil
}

func aad(sellerID, connID, provider string) []byte {
	return []byte("courier|" + sellerID + "|" + connID + "|" + provider)
}

func (s *Service) seal(sellerID, connID, provider string, creds map[string]string) (string, error) {
	b, err := json.Marshal(creds)
	if err != nil {
		return "", err
	}
	return s.box.Seal(b, aad(sellerID, connID, provider))
}

func (s *Service) open(c *models.CourierConnection) (map[string]string, error) {
	pt, err := s.box.Open(c.EncryptedCredentials, aad(c.SellerID, c.ID, c.Provider))
	if err != nil {
		return nil, err
	}
	var creds map[string]string
	if err := json.Unmarshal(pt, &creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// rotateIfOld re-encrypts credentials written with a retired key. It is
// best effort: a failure here must not fail the test.
func (s *Service) rotateIfOld(ctx context.Context, c *models.CourierConnection, p courier.Provider, creds map[string]string) {
	if !s.box.NeedsRotation(c.EncryptedCredentials) {
		return
	}
	sealed, err := s.seal(c.SellerID, c.ID, c.Provider, creds)
	if err != nil {
		return
	}
	err = s.repo.ReplaceCredentials(ctx, c.SellerID, c.ID, repository.CourierCredentialUpdate{
		EncryptedCredentials: sealed, AccountFingerprint: c.AccountFingerprint, AccountHint: c.AccountHint, At: s.now(),
	})
	if err == nil {
		c.EncryptedCredentials = sealed
	}
}

func errAlreadyConnected() *apperr.Error {
	return apperr.Conflict(CodeAlreadyConnected, "This courier account is already connected.", "")
}

func isInvalidField(err error) bool {
	var bad *courier.ErrInvalidField
	return errors.As(err, &bad)
}

func reasonOf(err error) string {
	if errors.Is(err, courier.ErrInvalidCredentials) {
		return "rejected"
	}
	return "unavailable"
}

func mapVerifyError(p courier.Provider, err error) error {
	var bad *courier.ErrInvalidField
	if errors.As(err, &bad) {
		return apperr.Validation(map[string]string{bad.Key: bad.Message})
	}
	if errors.Is(err, courier.ErrInvalidCredentials) {
		return apperr.New(http.StatusUnprocessableEntity, CodeCredentialsRejected,
			p.Name()+" did not accept these credentials. Check them and try again.")
	}
	return apperr.New(http.StatusBadGateway, CodeUnavailable,
		"Could not reach "+p.Name()+" to verify the credentials. Nothing was saved. Try again in a moment.")
}

func cleanLabel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > maxLabelLen {
		return "", apperr.Validation(map[string]string{"label": "Label must be 60 characters or fewer"})
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", apperr.Validation(map[string]string{"label": "Label contains invalid characters"})
		}
	}
	return s, nil
}

// cleanCredentials keeps only fields the provider declares, trims them, and
// rejects missing, unknown, oversized or control-character values.
func cleanCredentials(p courier.Provider, in map[string]string) (map[string]string, error) {
	fields := map[string]string{}
	allowed := map[string]courier.CredentialField{}
	for _, f := range p.Fields() {
		allowed[f.Key] = f
	}
	for k := range in {
		if _, ok := allowed[k]; !ok {
			fields[k] = "Unknown field"
		}
	}
	out := make(map[string]string, len(allowed))
	for key, f := range allowed {
		v := strings.TrimSpace(in[key])
		switch {
		case v == "" && f.Required:
			fields[key] = f.Label + " is required"
		case len(v) > maxCredentialLen:
			fields[key] = f.Label + " is too long"
		case strings.IndexFunc(v, unicode.IsControl) >= 0 || strings.ContainsAny(v, " \t"):
			fields[key] = f.Label + " contains invalid characters"
		default:
			out[key] = v
		}
	}
	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}
	return out, nil
}
