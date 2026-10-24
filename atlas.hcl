# Atlas (https://atlasgo.io) manages the schema of the built-in authentication database.
#
# The models are the source of truth (internal/store/schema). To change the schema: edit the models, then
#
#   atlas migrate diff <name> --env gorm
#
# which writes a new versioned migration to internal/store/migrations and updates atlas.sum. The binaries
# embed that directory and apply pending migrations at startup, in process (no Atlas CLI at runtime).
# The very first migration was produced by tools/migrate-gen.
#
# The schema is printed by tools/migrate-gen (a separate Go module, so the application does not depend on
# the GORM provider and its MySQL/Postgres drivers).
data "external_schema" "gorm" {
  program = ["go", "run", "-C", "tools/migrate-gen", ".", "-print"]
}

env "gorm" {
  src = data.external_schema.gorm.url
  dev = "sqlite://dev?mode=memory"
  migration {
    dir = "file://internal/store/migrations"
  }
}
