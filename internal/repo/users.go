package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/guofan/pio/internal/model"
)

// InsertLocalUser stores a new (username, plaintext-password) pair with no
// upstream mapping. Returns an error if the username already exists. The
// new row is placed at the bottom of the user-defined order so existing
// drag-reordered positions stay stable.
func InsertLocalUser(ctx context.Context, db *sql.DB, username, passwordPlain, notes string) error {
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx,
		`INSERT INTO local_users (username, password_plain, upstream_proxy_id, broken, notes, order_index, created_at, updated_at)
		 VALUES (?, ?, NULL, 0, ?, COALESCE((SELECT MAX(order_index) + 1 FROM local_users), 0), ?, ?)`,
		username, passwordPlain, notes, now, now,
	)
	return err
}

// GetLocalUser returns the row for username, or ErrNotFound.
func GetLocalUser(ctx context.Context, db *sql.DB, username string) (*model.LocalUser, error) {
	row := db.QueryRowContext(ctx,
		`SELECT username, password_plain, upstream_proxy_id, broken, notes, created_at, updated_at
		   FROM local_users WHERE username = ?`,
		username,
	)
	u := &model.LocalUser{}
	var upstream sql.NullString
	if err := row.Scan(&u.Username, &u.PasswordPlain, &upstream, &u.Broken, &u.Notes, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if upstream.Valid {
		s := upstream.String
		u.UpstreamProxyID = &s
	}
	return u, nil
}

// ListLocalUsers returns all rows in user-defined order (drag-reordered
// position). Username breaks ties so the result is fully deterministic
// even when multiple rows still share the default order_index=0.
func ListLocalUsers(ctx context.Context, db *sql.DB) ([]model.LocalUser, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT username, password_plain, upstream_proxy_id, broken, notes, created_at, updated_at
		   FROM local_users ORDER BY order_index ASC, username ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LocalUser
	for rows.Next() {
		var u model.LocalUser
		var upstream sql.NullString
		if err := rows.Scan(&u.Username, &u.PasswordPlain, &upstream, &u.Broken, &u.Notes, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		if upstream.Valid {
			s := upstream.String
			u.UpstreamProxyID = &s
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateLocalUserPassword sets a new password and bumps updated_at.
func UpdateLocalUserPassword(ctx context.Context, db ExecCtx, username, passwordPlain string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE local_users SET password_plain = ?, updated_at = ? WHERE username = ?`,
		passwordPlain, time.Now().UTC(), username,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateLocalUserMapping rebinds (or unmaps with nil) and resets broken=false
// when a non-nil upstream is supplied.
func UpdateLocalUserMapping(ctx context.Context, db ExecCtx, username string, upstreamID *string) error {
	broken := upstreamID == nil
	res, err := db.ExecContext(ctx,
		`UPDATE local_users SET upstream_proxy_id = ?, broken = ?, updated_at = ? WHERE username = ?`,
		upstreamID, broken, time.Now().UTC(), username,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderLocalUsers writes order_index = i for each username at position i
// in the supplied slice. Runs in a single transaction so the new ordering
// is committed atomically. Usernames not present in the supplied slice
// are left untouched (their position relative to each other persists, but
// they appear after the reordered set on next ListLocalUsers).
func ReorderLocalUsers(ctx context.Context, db *sql.DB, usernames []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, u := range usernames {
		if _, err := tx.ExecContext(ctx,
			`UPDATE local_users SET order_index = ? WHERE username = ?`, i, u,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteLocalUser removes the row.
func DeleteLocalUser(ctx context.Context, db ExecCtx, username string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM local_users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
