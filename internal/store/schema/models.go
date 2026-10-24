// Package schema holds the GORM models of the database. It is a package of its own so that the schema
// generator (tools/migrate-gen, Atlas) can load the models without the embedded migrations they produce.
package schema

import "time"

// Roles. An admin can manage users; a user can use the documentation.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is a login of the built-in authentication.
type User struct {
	ID uint `gorm:"primaryKey"`
	// Username is stored lower-case: logins are case-insensitive.
	Username     string `gorm:"size:64;not null;uniqueIndex"`
	PasswordHash string `gorm:"not null"`
	Role         string `gorm:"size:16;not null;default:user"`
	Disabled     bool   `gorm:"not null;default:false"`
	// SessionEpoch is part of every session; changing the password or disabling the user bumps it and so
	// signs the user out everywhere.
	SessionEpoch int `gorm:"not null;default:0"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Models lists every table, for the schema generator.
func Models() []any { return []any{&User{}} }
