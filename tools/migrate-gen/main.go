// migrate-gen writes the initial Atlas migration from the GORM models and refreshes atlas.sum.
//
// For later schema changes use the Atlas CLI, which diffs the models against the migration directory (see
// atlas.hcl in the repository root):
//
//	atlas migrate diff <name> --env gorm
//
// Run this tool from the repository root, in its own module: (cd tools/migrate-gen && go build -o /tmp/migrate-gen .) && /tmp/migrate-gen
// (it only creates the first migration; `-rehash` just rewrites atlas.sum, e.g. after editing a file by hand).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ariga.io/atlas-provider-gorm/gormschema"
	"ariga.io/atlas/sql/migrate"

	"github.com/perryprojects/docker-swagger-aggregator/internal/store/schema"
)

const dirPath = "internal/store/migrations"

func main() {
	rehash := flag.Bool("rehash", false, "only rewrite atlas.sum")
	printDDL := flag.Bool("print", false, "print the schema of the models as SQL (Atlas external_schema source)")
	flag.Parse()
	if *printDDL {
		ddl, err := gormschema.New("sqlite").Load(schema.Models()...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate-gen:", err)
			os.Exit(1)
		}
		fmt.Print(ddl)
		return
	}
	if err := run(*rehash); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-gen:", err)
		os.Exit(1)
	}
}

func run(rehash bool) error {
	dir, err := migrate.NewLocalDir(dirPath)
	if err != nil {
		return err
	}
	files, err := dir.Files()
	if err != nil {
		return err
	}
	if !rehash && len(files) == 0 {
		ddl, loadErr := gormschema.New("sqlite").Load(schema.Models()...)
		if loadErr != nil {
			return loadErr
		}
		name := time.Now().UTC().Format("20060102150405") + "_init.sql"
		if err = os.WriteFile(filepath.Join(dirPath, name), []byte(ddl+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("wrote", name)
	} else if !rehash {
		return fmt.Errorf("%s already has migrations; use `atlas migrate diff` for changes", dirPath)
	}
	if files, err = dir.Files(); err != nil {
		return err
	}
	sum, err := migrate.NewHashFile(files)
	if err != nil {
		return err
	}
	return migrate.WriteSumFile(dir, sum)
}
