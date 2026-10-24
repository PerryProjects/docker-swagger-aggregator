package store

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"
)

var (
	ErrNotFound  = errors.New("user not found")
	ErrExists    = errors.New("user already exists")
	ErrLastAdmin = errors.New("there must be at least one active admin")
)

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	return n, s.db.WithContext(ctx).Model(&User{}).Count(&n).Error
}

// CreateUser adds a user. The username is normalised to lower case.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (User, error) {
	u := User{Username: strings.ToLower(username), PasswordHash: passwordHash, Role: role}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&User{}).Where("username = ?", u.Username).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrExists
		}
		return tx.Create(&u).Error
	})
	return u, err
}

// UserByName finds a user by (case-insensitive) username.
func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	return s.first(ctx, "username = ?", strings.ToLower(username))
}

// UserByID finds a user by ID.
func (s *Store) UserByID(ctx context.Context, id uint) (User, error) {
	return s.first(ctx, "id = ?", id)
}

func (s *Store) first(ctx context.Context, query string, arg any) (User, error) {
	var u User
	err := s.db.WithContext(ctx).Where(query, arg).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, ErrNotFound
	}
	return u, err
}

// ListUsers returns all users ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var us []User
	return us, s.db.WithContext(ctx).Order("username").Find(&us).Error
}

// SetPassword replaces the password hash and signs the user out everywhere.
func (s *Store) SetPassword(ctx context.Context, id uint, passwordHash string) error {
	return s.update(ctx, id, func(tx *gorm.DB, u *User) error {
		u.PasswordHash = passwordHash
		u.SessionEpoch++
		return tx.Save(u).Error
	})
}

// SetRole changes the role; the last active admin cannot be demoted.
func (s *Store) SetRole(ctx context.Context, id uint, role string) error {
	return s.update(ctx, id, func(tx *gorm.DB, u *User) error {
		if u.Role == RoleAdmin && role != RoleAdmin {
			if err := ensureOtherAdmin(tx, u); err != nil {
				return err
			}
		}
		u.Role = role
		u.SessionEpoch++ // the session carries no role, but a changed role should be visible at once
		return tx.Save(u).Error
	})
}

// SetDisabled disables or enables a user; a disabled user is signed out. The last active admin cannot be disabled.
func (s *Store) SetDisabled(ctx context.Context, id uint, disabled bool) error {
	return s.update(ctx, id, func(tx *gorm.DB, u *User) error {
		if disabled && u.Role == RoleAdmin && !u.Disabled {
			if err := ensureOtherAdmin(tx, u); err != nil {
				return err
			}
		}
		u.Disabled = disabled
		u.SessionEpoch++
		return tx.Save(u).Error
	})
}

// DeleteUser removes a user; the last active admin cannot be deleted.
func (s *Store) DeleteUser(ctx context.Context, id uint) error {
	return s.update(ctx, id, func(tx *gorm.DB, u *User) error {
		if u.Role == RoleAdmin && !u.Disabled {
			if err := ensureOtherAdmin(tx, u); err != nil {
				return err
			}
		}
		return tx.Delete(u).Error
	})
}

func (s *Store) update(ctx context.Context, id uint, f func(tx *gorm.DB, u *User) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u User
		if err := tx.First(&u, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		return f(tx, &u)
	})
}

// ensureOtherAdmin fails unless an enabled admin besides u exists.
func ensureOtherAdmin(tx *gorm.DB, u *User) error {
	var n int64
	err := tx.Model(&User{}).Where("role = ? AND disabled = ? AND id <> ?", RoleAdmin, false, u.ID).Count(&n).Error
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// ErrNotEmpty is returned by CreateFirstUser when a user exists already.
var ErrNotEmpty = errors.New("a user exists already")

// CreateFirstUser adds the user only if the table is empty, atomically: of two concurrent setups exactly
// one wins.
func (s *Store) CreateFirstUser(ctx context.Context, username, passwordHash, role string) (User, error) {
	u := User{Username: strings.ToLower(username), PasswordHash: passwordHash, Role: role}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&User{}).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrNotEmpty
		}
		return tx.Create(&u).Error
	})
	return u, err
}
