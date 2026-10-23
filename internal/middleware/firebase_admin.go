package middleware

import (
	"context"

	"firebase.google.com/go/v4/auth"
)

// FirebaseAdmin is the account-management slice of the Firebase Admin SDK that
// the MM24 routes use (sign out everywhere, delete the Firebase user). Built the
// same way as the token verifier, from GOOGLE_APPLICATION_CREDENTIALS and
// FIREBASE_PROJECT_ID.
type FirebaseAdmin struct {
	client *auth.Client
}

// NewFirebaseAdmin builds the admin client.
func NewFirebaseAdmin(ctx context.Context, projectID string) (*FirebaseAdmin, error) {
	client, err := newFirebaseAuthClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return &FirebaseAdmin{client: client}, nil
}

// RevokeRefreshTokens invalidates every refresh token of the user.
func (a *FirebaseAdmin) RevokeRefreshTokens(ctx context.Context, uid string) error {
	return a.client.RevokeRefreshTokens(ctx, uid)
}

// DeleteUser deletes the Firebase user. A user that is already gone is success,
// which is what makes a retried account deletion idempotent.
func (a *FirebaseAdmin) DeleteUser(ctx context.Context, uid string) error {
	err := a.client.DeleteUser(ctx, uid)
	if err != nil && auth.IsUserNotFound(err) {
		return nil
	}
	return err
}
