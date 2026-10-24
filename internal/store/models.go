package store

import "github.com/perryprojects/docker-swagger-aggregator/internal/store/schema"

// Package store is the SQLite persistence of the built-in authentication (users). It uses GORM with a
// CGO-free SQLite driver (modernc.org/sqlite via github.com/glebarez/sqlite) and applies Atlas migrations at startup.
//
// The tables are described by the models in package schema; the SQL in migrations/ is generated from
// them (see tools/migrate-gen and atlas.hcl).

// User is a login of the built-in authentication.
type User = schema.User

// Roles. An admin can manage users; a user can use the documentation.
const (
	RoleAdmin = schema.RoleAdmin
	RoleUser  = schema.RoleUser
)
