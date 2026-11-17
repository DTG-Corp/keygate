package store

import (
	"database/sql"
	"errors"

	"github.com/uptrace/bun/driver/pgdriver"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). Uses pgdriver.Error for type-safe inspection
// rather than substring matching on the human-readable message.
//
// Driver-specific: relies on uptrace/bun pgdriver. If the project ever
// migrates to bun's pgxdialect (or another driver), this check must be
// revisited.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23505"
	}
	return false
}

// isUniqueViolationOn reports whether err is a unique-constraint
// violation (SQLSTATE 23505) on the named constraint or unique index.
// Postgres reports a unique index's own name in the constraint field, so
// this matches both a table constraint (payment_orders_code_key) and a
// standalone unique index (payment_orders_renewal_once).
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Field('C') == "23505" && pgErr.Field('n') == constraint
}

// isDeadlock reports whether err is the losing side of a deadlock the
// server broke (SQLSTATE 40P01).
func isDeadlock(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "40P01"
}

// isLockTimeout reports whether err is a wait that lock_timeout ended
// (SQLSTATE 55P03).
func isLockTimeout(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "55P03"
}

// IsUniqueViolation is isUniqueViolation for handlers that turn a
// duplicate into a 409 instead of a 500.
func IsUniqueViolation(err error) bool { return isUniqueViolation(err) }

// IsUniqueViolationOn reports whether err is a unique violation on the named index.
func IsUniqueViolationOn(err error, constraint string) bool { return isUniqueViolationOn(err, constraint) }

// IsForeignKeyViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503): a row that names a parent which does not
// exist, or a delete of a parent that still has children.
func IsForeignKeyViolation(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23503"
}

// IsStillReferenced reports whether err is a delete refused because rows
// still refer to the row: a foreign key (23503) or a restrict (23001)
// violation. A delete that counted those rows first and found none met
// one added after the count.
func IsStillReferenced(err error) bool {
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) {
		return false
	}
	code := pgErr.Field('C')
	return code == "23503" || code == "23001"
}

// IsTrialFallbackMissing reports whether err is the foreign key on
// plans.trial_fallback_plan_id: the plan named as a trial's fallback
// does not exist (deleted after it was checked).
func IsTrialFallbackMissing(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23503" && pgErr.Field('n') == "plans_trial_fallback_plan_id_fkey"
}

// deletedOne turns a delete that matched no row into sql.ErrNoRows, so a
// handler can answer 404 for an id that does not exist instead of 204.
func deletedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
