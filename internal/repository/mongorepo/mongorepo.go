// Package mongorepo implements the repository interfaces on MongoDB.
package mongorepo

import (
	"context"
	"errors"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// opTimeout bounds every database call, on top of the request deadline.
const opTimeout = 5 * time.Second

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, opTimeout)
}

// Users implements repository.UserRepository.
type Users struct{ col *mongo.Collection }

func NewUsers(db *mongo.Database) *Users {
	return &Users{col: db.Collection(database.UsersCollection)}
}

func (r *Users) Create(ctx context.Context, u *models.User) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := r.col.InsertOne(ctx, u); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrDuplicateEmail
		}
		return err
	}
	return nil
}

func (r *Users) FindByEmail(ctx context.Context, email string) (*models.User, error) {
	return r.findOne(ctx, bson.D{{Key: "email", Value: email}})
}

func (r *Users) FindByID(ctx context.Context, id string) (*models.User, error) {
	return r.findOne(ctx, bson.D{{Key: "_id", Value: id}})
}

func (r *Users) findOne(ctx context.Context, filter bson.D) (*models.User, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var u models.User
	if err := r.col.FindOne(ctx, filter).Decode(&u); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, repository.ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// FindByProvider finds the user who signed in with this Google / Apple account.
func (r *Users) FindByProvider(ctx context.Context, provider, subject string) (*models.User, error) {
	field := models.ProviderField(provider)
	if field == "" || subject == "" {
		return nil, repository.ErrNotFound
	}
	return r.findOne(ctx, bson.D{{Key: field, Value: subject}})
}

// LinkProvider attaches a Google / Apple account to an existing user. The
// provider has already confirmed the email, so it is marked verified too.
//
// Only the provider ID, emailVerified and updatedAt are written: the password
// hash, role and status are never touched. The update only applies while the
// slot is empty (or already holds this same ID), so a different linked account
// is never silently replaced.
func (r *Users) LinkProvider(ctx context.Context, userID, provider, subject string) error {
	field := models.ProviderField(provider)
	if field == "" || subject == "" {
		return repository.ErrNotFound
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{
			{Key: "_id", Value: userID},
			{Key: field, Value: bson.D{{Key: "$in", Value: bson.A{nil, "", subject}}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: field, Value: subject},
			{Key: "emailVerified", Value: true},
			{Key: "updatedAt", Value: time.Now().UTC()},
		}}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) { // unique index: ID belongs to another user
			return repository.ErrProviderLinked
		}
		return err
	}
	if res.MatchedCount == 0 {
		// Either no such user, or the slot holds a different ID.
		if _, ferr := r.FindByID(ctx, userID); ferr != nil {
			return ferr
		}
		return repository.ErrProviderLinked
	}
	return nil
}

func (r *Users) UpdatePassword(ctx context.Context, userID, passwordHash string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "passwordHash", Value: passwordHash},
			{Key: "updatedAt", Value: time.Now().UTC()},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Users) UpdateStatus(ctx context.Context, userID string, status models.Status) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: status},
			{Key: "updatedAt", Value: time.Now().UTC()},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Users) UpdateProfile(ctx context.Context, userID, name, email string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	set := bson.D{{Key: "updatedAt", Value: time.Now().UTC()}}
	if name != "" {
		set = append(set, bson.E{Key: "name", Value: name})
	}
	if email != "" {
		set = append(set, bson.E{Key: "email", Value: email})
	}
	res, err := r.col.UpdateOne(ctx, bson.D{{Key: "_id", Value: userID}}, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrDuplicateEmail
		}
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Users) CountByRole(ctx context.Context, role models.Role) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{{Key: "role", Value: role}})
}

func (r *Users) CountByRoleAndStatus(ctx context.Context, role models.Role, status models.Status) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{
		{Key: "role", Value: role},
		{Key: "status", Value: status},
	})
}

func (r *Users) ListByRole(ctx context.Context, filter repository.UserListFilter) ([]*models.User, int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	q := bson.D{}
	if filter.Role != "" {
		q = append(q, bson.E{Key: "role", Value: filter.Role})
	}
	if filter.Status != nil {
		q = append(q, bson.E{Key: "status", Value: *filter.Status})
	}
	if filter.Search != "" {
		// Escape the text: it is a literal search, never a regex (stops
		// regex injection and ReDoS), and cap its length.
		search := filter.Search
		if len(search) > 100 {
			search = search[:100]
		}
		pat := regexp.QuoteMeta(search)
		q = append(q, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: pat}, {Key: "$options", Value: "i"}}}},
			bson.D{{Key: "email", Value: bson.D{{Key: "$regex", Value: pat}, {Key: "$options", Value: "i"}}}},
		}})
	}

	total, err := r.col.CountDocuments(ctx, q)
	if err != nil {
		return nil, 0, err
	}

	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	skip := filter.Skip
	if skip < 0 {
		skip = 0
	}
	sortDir := int32(1)
	if filter.SortNewest {
		sortDir = -1
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: sortDir}}).
		SetLimit(int64(limit)).
		SetSkip(int64(skip))

	cur, err := r.col.Find(ctx, q, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)

	var out []*models.User
	for cur.Next(ctx) {
		var u models.User
		if err := cur.Decode(&u); err != nil {
			return nil, 0, err
		}
		out = append(out, &u)
	}
	return out, total, cur.Err()
}

func (r *Users) UpdateRole(ctx context.Context, userID string, role models.Role) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "role", Value: role},
			{Key: "updatedAt", Value: time.Now().UTC()},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}
